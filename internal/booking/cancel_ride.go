package booking

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	auditActionTripCancel = "ride_trip.cancel"
	auditKindRideTrip     = "ride_trip"

	// tripCancelAttempts bounds the retries of CancelTrip when a checkout on
	// the trip gained a trip that was not locked (see cancelTripOnce).
	tripCancelAttempts = 3
)

// errRideDeparted: a customer cancels a round trip only while no leg has
// departed or been checked in (spec §8).
var errRideDeparted = httpx.NewError(http.StatusConflict, ErrInvalidBookingState.Code,
	"Chỉ hủy được khi chưa đi chiều nào")

// errTripSetChanged: under the trip locks, a checkout to cancel turned out to
// hold a trip this tx did not lock; the tx is rolled back and retried.
var errTripSetChanged = errors.New("booking: trips of the affected checkouts changed")

// rideCancelBlock is the cancellation predicate of ride bookings (nil:
// allowed), shared by the commands and BookingView.can_cancel (byCustomer).
//
//   - byCustomer: legs are every booking of the checkout; each leg not
//     cancelled must be confirmed and now must be before the earliest
//     service_start_at of those legs (no leg departed or checked in).
//   - admin: legs is the chosen leg; it must be confirmed and now before its
//     service_end_at (Ruling 21).
func rideCancelBlock(legs []store.Booking, now time.Time, byCustomer bool) error {
	if !byCustomer {
		for _, b := range legs {
			if err := cancelBlock(b, now); err != nil {
				return err
			}
		}
		return nil
	}
	live := 0
	for _, b := range legs {
		if b.Status == StatusCancelled {
			continue
		}
		live++
		if b.Status != StatusConfirmed || !now.Before(b.ServiceStartAt) {
			return errRideDeparted
		}
	}
	if live == 0 {
		return ErrInvalidBookingState
	}
	return nil
}

// cancelRideCheckout cancels a ride booking together with the other leg of
// its round trip. owner, when set, must own the booking (else
// ErrBookingNotFound): a customer cancels every leg, only while no leg has
// departed (rideCancelBlock); Super Admin chooses a confirmed leg not yet
// ended, and every leg of the checkout still confirmed and not ended is
// cancelled with it (legs checked_in, completed or no_show stay). Each leg
// is cancelled by cancelLocked: its refund follows its own
// original_service_start_at (or 100% in force majeure) and its seats go back
// to its trip. The customer's bank account is saved once, before the locks,
// and prefills every refund of the cancellation.
//
// Lock order (DB §1.1): refund account (users child), the checkout's trips
// (id order), the checkout, its bookings (id order), then cancelLocked
// (refunds, payments by FK, partner_balances).
func (s *Service) cancelRideCheckout(ctx context.Context, bookingID uuid.UUID, owner *uuid.UUID, c cancellation) error {
	q0 := store.New(s.pool)
	b, err := q0.GetBooking(ctx, bookingID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != nil && b.CustomerID != *owner) {
		return ErrBookingNotFound
	}
	if err != nil {
		return fmt.Errorf("get booking: %w", err)
	}
	// Ownership (404) first, then the bank account (422), then the state (409).
	if c.bank != nil {
		acc, err := c.bank.Normalize()
		if err != nil {
			return err
		}
		c.bank = &acc
	}
	byCustomer := c.actorKind == actorCustomer
	rows, err := q0.ListCheckoutBookings(ctx, b.CheckoutID)
	if err != nil {
		return fmt.Errorf("list checkout bookings: %w", err)
	}
	legs := make([]store.Booking, len(rows))
	for i, r := range rows {
		legs[i] = r.Booking
	}
	if err := rideBlockFor(legs, b.ID, s.clock.Now(), byCustomer); err != nil {
		return err
	}
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if c.bank != nil {
			if _, err := s.payments.SaveRefundAccountTx(ctx, tx, b.CustomerID, *c.bank); err != nil {
				return err
			}
		}
		if err := s.lockCheckoutTrips(ctx, tx, b.CheckoutID); err != nil {
			return err
		}
		co, err := q.LockCheckout(ctx, b.CheckoutID)
		if err != nil {
			return fmt.Errorf("lock checkout: %w", err)
		}
		legs, err := q.LockCheckoutBookings(ctx, b.CheckoutID)
		if err != nil {
			return fmt.Errorf("lock checkout bookings: %w", err)
		}
		now := s.clock.Now()
		if err := rideBlockFor(legs, b.ID, now, byCustomer); err != nil {
			return err
		}
		for _, leg := range legs {
			if !rideLegCancellable(leg, now) {
				continue
			}
			if _, err := s.cancelLocked(ctx, tx, co, leg, c, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// rideBlockFor applies rideCancelBlock to the legs of a checkout for a
// cancel of booking id: the customer rule on every leg, the admin rule on
// the chosen leg.
func rideBlockFor(legs []store.Booking, id uuid.UUID, now time.Time, byCustomer bool) error {
	if byCustomer {
		return rideCancelBlock(legs, now, true)
	}
	i := slices.IndexFunc(legs, func(b store.Booking) bool { return b.ID == id })
	if i < 0 {
		return ErrBookingNotFound
	}
	return rideCancelBlock(legs[i:i+1], now, false)
}

// rideLegCancellable: a leg a ride cancellation cancels — confirmed and not
// yet ended (the admin rule; under the customer rule every live leg passes
// it). checked_in, completed, no_show, expired and cancelled legs stay.
func rideLegCancellable(b store.Booking, now time.Time) bool {
	return rideCancelBlock([]store.Booking{b}, now, false) == nil
}

// TripCancelResult is the outcome of an admin trip cancel: the bookings
// cancelled (both legs of every affected round trip) and the total refunded.
type TripCancelResult struct {
	TripID              uuid.UUID   `json:"trip_id"`
	CancelledBookingIDs []uuid.UUID `json:"cancelled_booking_ids"`
	RefundTotal         int64       `json:"refund_total"`
}

// CancelTrip cancels a scheduled trip as Super Admin, always in force
// majeure: every checkout whose booking on the trip is confirmed (and not
// ended) has each of its confirmed, not ended legs cancelled — both legs of a
// round trip, even the one on another trip — with a 100% refund, the
// commission waived and the seats returned. Legs checked_in, completed or
// no_show stay; pending_payment bookings are left to the hold expiry job (a
// later payment is refunded, Task 6). reason is 1..500 characters. An
// unknown trip is listing.ErrTripNotFound; a trip no longer scheduled is
// listing.ErrTripNotScheduled. Audited as ride_trip.cancel (and
// booking.cancel per booking) in the same tx.
func (s *Service) CancelTrip(ctx context.Context, actor, tripID uuid.UUID, reason string) (TripCancelResult, error) {
	reason, err := normalizeAdminReason(reason)
	if err != nil {
		return TripCancelResult{}, err
	}
	for range tripCancelAttempts {
		res, err := s.cancelTripOnce(ctx, actor, tripID, reason)
		if !errors.Is(err, errTripSetChanged) {
			return res, err
		}
	}
	return TripCancelResult{}, fmt.Errorf("cancel trip %s: %w", tripID, errTripSetChanged)
}

// tripCheckouts reads the checkouts with a confirmed booking on tripID, their
// ride bookings and the set of their trips (tripID included).
func tripCheckouts(ctx context.Context, q *store.Queries, tripID uuid.UUID) ([]uuid.UUID, []store.ListCheckoutsRideBookingsRow, []uuid.UUID, error) {
	checkouts, err := q.ListTripCheckouts(ctx, tripID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list trip checkouts: %w", err)
	}
	rows, err := q.ListCheckoutsRideBookings(ctx, checkouts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list checkouts ride bookings: %w", err)
	}
	trips := []uuid.UUID{tripID}
	for _, r := range rows {
		trips = append(trips, r.TripID)
	}
	return checkouts, rows, sortedIDs(trips), nil
}

// cancelTripOnce is one attempt of CancelTrip.
//
// Lock order (DB §1.1): the checkouts on the trip and all their trips are
// read unlocked, then every one of those trips is locked (id order) and the
// trip cancelled, so no booking on it can be created or confirmed any more.
// The checkouts are read again under the locks; one holding a trip not
// locked (it was confirmed in between) is errTripSetChanged, retried. Then
// the checkouts (id order), their bookings (id order) and, per cancelled
// booking, cancelLocked (refunds, payments by FK, partner_balances).
func (s *Service) cancelTripOnce(ctx context.Context, actor, tripID uuid.UUID, reason string) (TripCancelResult, error) {
	_, _, trips, err := tripCheckouts(ctx, store.New(s.pool), tripID)
	if err != nil {
		return TripCancelResult{}, err
	}
	res := TripCancelResult{TripID: tripID, CancelledBookingIDs: []uuid.UUID{}}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		locked, err := s.listings.LockTrips(ctx, tx, trips)
		if err != nil {
			return err
		}
		before := tripByID(locked, tripID)
		now := s.clock.Now()
		if err := s.listings.CancelTripByAdmin(ctx, tx, tripID, actor, reason, now); err != nil {
			return err
		}
		checkouts, rows, again, err := tripCheckouts(ctx, q, tripID)
		if err != nil {
			return err
		}
		for _, id := range again {
			if !slices.Contains(trips, id) {
				return errTripSetChanged
			}
		}
		cos := make(map[uuid.UUID]store.Checkout, len(checkouts))
		for _, id := range checkouts { // sorted by the query
			co, err := q.LockCheckout(ctx, id)
			if err != nil {
				return fmt.Errorf("lock checkout: %w", err)
			}
			cos[id] = co
		}
		ids := make([]uuid.UUID, len(rows))
		tripOf := make(map[uuid.UUID]uuid.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
			tripOf[r.ID] = r.TripID
		}
		bookings, err := q.LockBookingsByIDs(ctx, sortedIDs(ids))
		if err != nil {
			return fmt.Errorf("lock bookings: %w", err)
		}
		// Affected: the checkouts whose booking on this trip may still be
		// cancelled (re-checked under the locks; check-in does not take the
		// trip lock).
		affected := map[uuid.UUID]bool{}
		for _, b := range bookings {
			if tripOf[b.ID] == tripID && rideLegCancellable(b, now) {
				affected[b.CheckoutID] = true
			}
		}
		c := cancellation{
			kind: cancelKindAdminForceMajeure, refundReason: payment.RefundAdminCancel,
			actorKind: actorAdmin, actor: actor, reason: &reason, forceMajeure: true,
		}
		for _, b := range bookings {
			if !affected[b.CheckoutID] || !rideLegCancellable(b, now) {
				continue
			}
			refund, err := s.cancelLocked(ctx, tx, cos[b.CheckoutID], b, c, now)
			if err != nil {
				return err
			}
			res.CancelledBookingIDs = append(res.CancelledBookingIDs, b.ID)
			res.RefundTotal += refund
		}
		// Already locked by this tx: a plain re-read of the trip.
		after, err := s.listings.LockTrips(ctx, tx, []uuid.UUID{tripID})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor, Action: auditActionTripCancel, TargetKind: auditKindRideTrip, TargetID: &tripID,
			Before: map[string]any{
				"status": before.Status, "seats_reserved": before.SeatsReserved,
				"cancelled_booking_ids": []uuid.UUID{}, "refund_total": 0,
			},
			After: map[string]any{
				"status": after[0].Status, "seats_reserved": after[0].SeatsReserved,
				"cancelled_booking_ids": res.CancelledBookingIDs, "refund_total": res.RefundTotal,
			},
			Reason: reason,
		})
	})
	if err != nil {
		return TripCancelResult{}, err
	}
	return res, nil
}

func tripByID(trips []listing.Trip, id uuid.UUID) listing.Trip {
	for _, t := range trips {
		if t.ID == id {
			return t
		}
	}
	return listing.Trip{}
}
