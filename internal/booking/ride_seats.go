package booking

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
)

// sortedIDs returns ids sorted (byte order, as PostgreSQL orders uuid) with
// duplicates removed: the order in which rows of one table are locked.
func sortedIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return slices.Compact(out)
}

// lockCheckoutTrips locks the trips of a checkout's ride bookings (none for
// an attraction checkout). Every tx that locks a checkout holding ride
// bookings calls it first: ride_trips come before checkouts in the lock
// order (DB §1.1). The trip ids are read unlocked; a booking's trip never
// changes.
func (s *Service) lockCheckoutTrips(ctx context.Context, tx pgx.Tx, checkoutID uuid.UUID) error {
	ids, err := store.New(tx).ListCheckoutRideTrips(ctx, checkoutID)
	if err != nil {
		return fmt.Errorf("list checkout ride trips: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	_, err = s.listings.LockTrips(ctx, tx, ids)
	return err
}

// releaseRideSeats returns the seats of a ride booking to its trip, in the
// caller's tx, which holds the trip lock and moves the booking to expired or
// cancelled. A booking of another service type has no seats: nothing is
// done.
func (s *Service) releaseRideSeats(ctx context.Context, tx pgx.Tx, b store.Booking) error {
	if b.ServiceType != serviceTypeRide {
		return nil
	}
	d, err := store.New(tx).GetRideDetails(ctx, b.ID)
	if err != nil {
		return fmt.Errorf("get ride details: %w", err)
	}
	return s.listings.ReleaseSeats(ctx, tx, d.TripID, int(d.Seats))
}

// releaseOverdueOnTrips expires the pending_payment ride bookings on tripIDs
// whose hold passed release (hold_release_at <= now) but that the hold
// expiry job has not swept yet, except those of exceptCheckout, and returns
// their seats (spec §7). The caller holds the trip locks.
//
// Lock order (DB §1.1): the trips are already locked, then the checkouts
// concerned in id order, then the overdue bookings in id order; the state is
// re-checked under the locks. A checkout left with no booking that is not
// expired or cancelled gets its pending payments expired and is closed; their
// order codes are returned so the caller cancels the links after commit. The
// other leg of a round trip on a trip the caller did not lock is not touched:
// it stays pending_payment for the job, and its checkout stays open.
func (s *Service) releaseOverdueOnTrips(ctx context.Context, tx pgx.Tx, tripIDs []uuid.UUID, exceptCheckout uuid.UUID, now time.Time) ([]int64, error) {
	q := store.New(tx)
	rows, err := q.ListOverdueRideHolds(ctx, store.ListOverdueRideHoldsParams{
		TripIds: tripIDs, Now: now, ExceptCheckout: exceptCheckout,
	})
	if err != nil {
		return nil, fmt.Errorf("list overdue ride holds: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	var checkoutIDs, bookingIDs []uuid.UUID
	for _, r := range rows {
		checkoutIDs = append(checkoutIDs, r.CheckoutID)
		bookingIDs = append(bookingIDs, r.ID)
	}
	checkouts := make([]store.Checkout, 0, len(checkoutIDs))
	for _, id := range sortedIDs(checkoutIDs) {
		co, err := q.LockCheckout(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("lock checkout: %w", err)
		}
		checkouts = append(checkouts, co)
	}
	bookings, err := q.LockBookingsByIDs(ctx, sortedIDs(bookingIDs))
	if err != nil {
		return nil, fmt.Errorf("lock bookings: %w", err)
	}
	reason := reasonHoldExpired
	for _, b := range bookings {
		if b.Status != StatusPendingPayment || b.HoldReleaseAt == nil || b.HoldReleaseAt.After(now) {
			continue
		}
		if err := q.ExpireBooking(ctx, store.ExpireBookingParams{Now: now, ID: b.ID}); err != nil {
			return nil, fmt.Errorf("expire booking: %w", err)
		}
		if err := insertHistory(ctx, q, b, StatusExpired, actorSystem, nil, &reason, now); err != nil {
			return nil, err
		}
		if err := s.releaseRideSeats(ctx, tx, b); err != nil {
			return nil, err
		}
	}
	var codes []int64
	for _, co := range checkouts {
		// Every booking status change locks the checkout first, so this
		// unlocked read is stable while the checkout is held.
		all, err := q.ListCheckoutBookings(ctx, co.ID)
		if err != nil {
			return nil, fmt.Errorf("list checkout bookings: %w", err)
		}
		done := true
		for _, r := range all {
			if st := r.Booking.Status; st != StatusExpired && st != StatusCancelled {
				done = false
			}
		}
		if !done {
			continue
		}
		pays, err := s.payments.LockCheckoutPayments(ctx, tx, co.ID)
		if err != nil {
			return nil, err
		}
		for _, p := range pays {
			if p.Status != payment.StatusPending {
				continue
			}
			if err := s.payments.ExpirePending(ctx, tx, p.ID); err != nil {
				return nil, err
			}
			codes = append(codes, p.OrderCode)
		}
		if co.Status == checkoutOpen {
			if err := q.SetCheckoutStatus(ctx, store.SetCheckoutStatusParams{Status: checkoutClosed, Now: now, ID: co.ID}); err != nil {
				return nil, fmt.Errorf("close checkout: %w", err)
			}
		}
	}
	return codes, nil
}
