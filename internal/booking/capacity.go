package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
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
