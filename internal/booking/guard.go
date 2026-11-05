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
// the partner write and then sees the new schedule.
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
