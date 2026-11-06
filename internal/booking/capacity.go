package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
)

// Capacity rules per service type, kept out of the shared lifecycle code
// (request.go, jobs, cancel, confirm): which capacity rows a checkout locks
// first, and how a booking gives its capacity back.

// lockCheckoutCapacity locks the capacity rows of a checkout's bookings:
// the trips of its ride bookings (id order), then the vehicle models of its
// rental bookings (id order); nothing for an attraction checkout. Every tx
// that locks a checkout holding ride or rental bookings calls it first:
// ride_trips and rental_vehicle_models come before checkouts in the lock
// order (DB §1.1). The ids are read unlocked; a booking's trip or model
// never changes.
func (s *Service) lockCheckoutCapacity(ctx context.Context, tx pgx.Tx, checkoutID uuid.UUID) error {
	q := store.New(tx)
	trips, err := q.ListCheckoutRideTrips(ctx, checkoutID)
	if err != nil {
		return fmt.Errorf("list checkout ride trips: %w", err)
	}
	if len(trips) > 0 {
		if _, err := s.listings.LockTrips(ctx, tx, trips); err != nil {
			return err
		}
	}
	models, err := q.ListCheckoutRentalModels(ctx, checkoutID)
	if err != nil {
		return fmt.Errorf("list checkout rental models: %w", err)
	}
	if len(models) > 0 {
		if _, err := s.listings.LockRentalModels(ctx, tx, models); err != nil {
			return err
		}
	}
	return nil
}

// releaseHold gives back the capacity a booking held, in the caller's tx,
// which holds its capacity lock (lockCheckoutCapacity) and moves the booking
// out of a holding status: a ride booking returns its seats to its trip, a
// rental booking stops holding its vehicle. An attraction booking holds no
// capacity: nothing is done.
func (s *Service) releaseHold(ctx context.Context, tx pgx.Tx, b store.Booking) error {
	switch b.ServiceType {
	case serviceTypeRide:
		return s.releaseRideSeats(ctx, tx, b)
	case serviceTypeRental:
		return s.setRentalHold(ctx, tx, b.ID, false, nil)
	}
	return nil
}

// setRentalHold sets holds_vehicle and hold_until of a rental booking (until
// must be nil when holds is false), in the tx that changes its status, which
// holds the model lock.
func (s *Service) setRentalHold(ctx context.Context, tx pgx.Tx, bookingID uuid.UUID, holds bool, until *time.Time) error {
	if !holds {
		until = nil
	}
	if err := store.New(tx).SetRentalHold(ctx, store.SetRentalHoldParams{
		HoldsVehicle: holds, HoldUntil: until, Now: s.clock.Now(), BookingID: bookingID,
	}); err != nil {
		return fmt.Errorf("set rental hold: %w", err)
	}
	return nil
}

// keepHoldUntil keeps the capacity a booking holds until until, in the
// caller's tx, which holds its capacity lock and moves it to a status whose
// hold ends then (an accepted request: hold_release_at). A rental booking's
// vehicle hold gets hold_until = until; nothing else is held per booking
// with an end.
func (s *Service) keepHoldUntil(ctx context.Context, tx pgx.Tx, b store.Booking, until time.Time) error {
	if b.ServiceType == serviceTypeRental {
		return s.setRentalHold(ctx, tx, b.ID, true, &until)
	}
	return nil
}

// holdForService makes the capacity hold of a booking that becomes
// confirmed permanent, in the caller's tx, which holds its capacity lock: a
// rental booking holds its vehicle with no end (holds_vehicle true,
// hold_until NULL), also when a late payment re-confirms it after its hold
// was released. Ride seats stay counted on the trip (reserveRideSeats takes
// them again after an expiry); an attraction holds nothing.
func (s *Service) holdForService(ctx context.Context, tx pgx.Tx, b store.Booking) error {
	if b.ServiceType == serviceTypeRental {
		return s.setRentalHold(ctx, tx, b.ID, true, nil)
	}
	return nil
}

// rentalModelIDs returns the vehicle models of a checkout's rental bookings,
// sorted (the lock order); none for another checkout. A booking's model never
// changes, so the unlocked read is enough.
func rentalModelIDs(rows []store.ListCheckoutBookingsRow) []uuid.UUID {
	var ids []uuid.UUID
	for _, r := range rows {
		if r.VehicleModelID != nil {
			ids = append(ids, *r.VehicleModelID)
		}
	}
	return sortedIDs(ids)
}

// rentalChecks decides, with the checkout's vehicle models locked (models,
// as locked) and before the checkout is locked, whether each rental booking
// of rows can be (re-)confirmed by the money arriving now, and records it in
// rebookable for every rental booking:
//
//   - awaiting_payment, not late: yes while its vehicle is still held for
//     it, i.e. the peak of the other holds plus one is within the model's
//     quantity (rentalAvailable, excluding this booking; the model may have
//     been deactivated since the accept). Otherwise no: refunded in full
//     (late_payment) without any ledger entry (final review I1).
//   - Late (expired, or awaiting_payment past hold_release_at; lateBooking):
//     only an accepted request (accepted_at set: a request that expired
//     unanswered was never accepted by the partner), with its listing
//     bookable (checked by reconfirmChecks; a late booking it did not see is
//     errStateChanged), not yet picked up (pickup_at > now), and one vehicle
//     of its model still free over [pickup_at, return_at): the model active
//     and the peak of the other holds plus one within its quantity
//     (rentalAvailable, excluding this booking) (spec §8).
//   - Anything else (requested, rejected, cancelled, ...): no.
//
// A checkout holds a single rental booking (CreateRental), so the
// availability checks of its late bookings never compete with each other.
func (s *Service) rentalChecks(ctx context.Context, tx pgx.Tx, rows []store.ListCheckoutBookingsRow, models map[uuid.UUID]listing.RentalModel, rebookable map[uuid.UUID]bool, now time.Time) error {
	q := store.New(tx)
	for _, r := range rows {
		b := r.Booking
		if b.ServiceType != serviceTypeRental {
			continue
		}
		if r.VehicleModelID == nil || r.RentalPickupAt == nil || r.RentalReturnAt == nil {
			return fmt.Errorf("booking: rental booking %s has no rental details", b.ID)
		}
		ok := false
		switch {
		case lateBooking(b, now):
			listed, checked := rebookable[b.ID]
			if !checked {
				return errStateChanged
			}
			model, found := models[*r.VehicleModelID]
			ok = listed && found && b.AcceptedAt != nil && r.RentalPickupAt.After(now)
			if ok {
				err := s.rentalAvailable(ctx, q, model, *r.RentalPickupAt, *r.RentalReturnAt, now, b.ID)
				switch {
				case errors.Is(err, ErrVehicleUnavailable):
					ok = false
				case err != nil:
					return err
				}
			}
		case b.Status == StatusAwaitingPayment:
			// On time, its own hold is live, so the other holds leave one
			// vehicle for it. Checked anyway (final review I1): if the hold
			// was treated as lapsed and the vehicle re-sold meanwhile, the
			// payment is refunded like a late one instead of confirming a
			// second holder. The model being deactivated since the accept
			// does not matter here: an accepted booking is still served.
			model, found := models[*r.VehicleModelID]
			if !found {
				return fmt.Errorf("booking: rental model %s of booking %s not locked", *r.VehicleModelID, b.ID)
			}
			model.Active = true
			err := s.rentalAvailable(ctx, q, model, *r.RentalPickupAt, *r.RentalReturnAt, now, b.ID)
			switch {
			case err == nil:
				ok = true
			case !errors.Is(err, ErrVehicleUnavailable):
				return err
			}
		}
		rebookable[b.ID] = ok
	}
	return nil
}

// hasCharter reports whether a checkout holds a private charter booking.
func hasCharter(rows []store.ListCheckoutBookingsRow) bool {
	for _, r := range rows {
		if r.Booking.ServiceType == serviceTypeCharter {
			return true
		}
	}
	return false
}

// charterChecks decides whether each late private charter booking of rows
// (lateBooking: expired, or awaiting_payment past hold_release_at) can be
// re-confirmed by the money arriving now, and records it in rebookable (spec
// §8): only an accepted request (accepted_at set: a request that expired
// unanswered was never accepted by the partner), with its listing bookable
// (listing visible, partner active; checked by reconfirmChecks, ruling R2; a
// late booking it did not see is errStateChanged), and its pickup
// (service_start_at) at least minLead after now (charter_late_confirm_min_hours:
// exactly minLead before pickup still re-confirms). Otherwise the booking is
// (or stays) expired and refunded in full without any ledger entry.
//
// A charter holds no capacity, so nothing is locked. A charter booking
// awaiting_payment and not late is not recorded: it is confirmed (eligible),
// also when its partner was suspended after accepting (ruling R2).
func charterChecks(rows []store.ListCheckoutBookingsRow, rebookable map[uuid.UUID]bool, minLead time.Duration, now time.Time) error {
	for _, r := range rows {
		b := r.Booking
		if b.ServiceType != serviceTypeCharter || !lateBooking(b, now) {
			continue
		}
		listed, checked := rebookable[b.ID]
		if !checked {
			return errStateChanged
		}
		rebookable[b.ID] = listed && b.AcceptedAt != nil && !b.ServiceStartAt.Before(now.Add(minLead))
	}
	return nil
}
