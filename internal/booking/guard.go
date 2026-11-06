package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

// Guard implements listing.BookingGuard: it counts active (pending_payment or
// confirmed) attraction bookings inside the partner's write tx, which already
// holds the listing FOR NO KEY UPDATE. Booking create/confirm (BookableListing)
// and Reschedule (ShareListingLock) take the listing FOR SHARE before reading
// the schedule, so a booking either committed before the count or waits for
// the partner write and then sees the new schedule. For rentals it also
// answers the peak number of vehicles held per model (RentalPeakHeld).
type Guard struct{}

// ActiveBookingsOnDate counts the active bookings of listingID on the Vietnam
// calendar date of date.
func (Guard) ActiveBookingsOnDate(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, date time.Time) (int, error) {
	n, err := store.New(tx).CountActiveBookingsOnDate(ctx, store.CountActiveBookingsOnDateParams{
		ListingID: listingID, VisitDate: date.In(clock.VN).Format(dateLayout),
	})
	if err != nil {
		return 0, fmt.Errorf("count active bookings on date: %w", err)
	}
	return int(n), nil
}

// ActiveBookingsForSlot counts the active bookings on slotID whose visit date
// is on or after the Vietnam calendar date of from and falls on a weekday in
// the weekdays mask (Monday = bit 0 ... Sunday = bit 6).
func (Guard) ActiveBookingsForSlot(ctx context.Context, tx pgx.Tx, slotID uuid.UUID, from time.Time, weekdays int) (int, error) {
	n, err := store.New(tx).CountActiveBookingsForSlot(ctx, store.CountActiveBookingsForSlotParams{
		TimeSlotID: slotID, FromDate: from.In(clock.VN).Format(dateLayout), Weekdays: int32(weekdays),
	})
	if err != nil {
		return 0, fmt.Errorf("count active bookings for slot: %w", err)
	}
	return int(n), nil
}

// RentalPeakHeld returns, per model, the peak number of bookings holding a
// vehicle at the same instant inside [from, to) (to nil: open-ended), as seen
// at now. A model without holds is absent (0). The caller holds the models
// FOR UPDATE in tx (listing.LockRentalModels or the partner's quantity edit),
// so the answer stays true until its tx ends.
func (Guard) RentalPeakHeld(ctx context.Context, tx pgx.Tx, modelIDs []uuid.UUID, from time.Time, to *time.Time, now time.Time) (map[uuid.UUID]int, error) {
	return peakRentalHolds(ctx, store.New(tx), modelIDs, from, to, now, uuid.Nil)
}

// peakRentalHolds is the single source of rental availability: create, late
// re-confirm, search and the quantity guard all use it. A booking holds a
// vehicle while holds_vehicle is set and hold_until (if any) is after now;
// periods are half-open, so back-to-back rentals do not overlap. except
// leaves one booking out (uuid.Nil: none).
func peakRentalHolds(ctx context.Context, q *store.Queries, modelIDs []uuid.UUID, from time.Time, to *time.Time, now time.Time, except uuid.UUID) (map[uuid.UUID]int, error) {
	out := make(map[uuid.UUID]int, len(modelIDs))
	if len(modelIDs) == 0 {
		return out, nil
	}
	rows, err := q.PeakRentalHolds(ctx, store.PeakRentalHoldsParams{
		WindowFrom: from, WindowTo: to, ModelIds: modelIDs, Now: now, ExceptBookingID: except,
	})
	if err != nil {
		return nil, fmt.Errorf("peak rental holds: %w", err)
	}
	for _, r := range rows {
		out[r.VehicleModelID] = int(r.Peak)
	}
	return out, nil
}
