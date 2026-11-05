package booking_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

func vnDay(s string) time.Time {
	d, err := time.ParseInLocation("2006-01-02", s, clock.VN)
	if err != nil {
		panic(err)
	}
	return d
}

func (e *env) closeDate(day string) error {
	return e.Listing.AddClosedDate(context.Background(), e.P.ID, e.P.OwnerUserID, e.L.ID, vnDay(day), nil)
}

func (e *env) patchSlot(p listing.TimeSlotPatch) error {
	_, err := e.Listing.UpdateTimeSlot(context.Background(), e.P.ID, e.P.OwnerUserID, e.L.ID, e.Slot.ID, p)
	return err
}

func TestCloseDateWithConfirmedBooking409(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, v.ID))

	requireErr(t, e.closeDate(visitDate), http.StatusConflict, "has_active_bookings")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM attraction_closed_dates`))
	// Another date has no booking.
	require.NoError(t, e.closeDate("2026-11-08"))

	// A pending_payment booking blocks too; a cancelled one does not.
	in := e.input()
	in.VisitDate = "2026-11-10"
	p, err := e.Svc.CreateAttraction(context.Background(), e.Customer, "pending", in)
	require.NoError(t, err)
	require.Equal(t, booking.StatusPendingPayment, p.Status)
	requireErr(t, e.closeDate("2026-11-10"), http.StatusConflict, "has_active_bookings")
	e.cancel(t, v.ID)
	require.NoError(t, e.closeDate(visitDate))
}

func TestDeactivateSlotWithFutureBooking409(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	off, start, wd := false, "09:00", 0b0011111

	requireErr(t, e.patchSlot(listing.TimeSlotPatch{Active: &off}), http.StatusConflict, "has_active_bookings")
	requireErr(t, e.patchSlot(listing.TimeSlotPatch{StartTime: &start}), http.StatusConflict, "has_active_bookings")
	requireErr(t, e.patchSlot(listing.TimeSlotPatch{Weekdays: &wd}), http.StatusConflict, "has_active_bookings")
	require.Equal(t, "08:00", e.str(t, `SELECT to_char(start_time, 'HH24:MI') FROM attraction_time_slots WHERE id = $1`, e.Slot.ID))

	// A booking whose date is before today (VN) no longer blocks.
	e.Clock.T = time.Date(2026, 11, 8, 9, 0, 0, 0, clock.VN)
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, v.ID))
	require.NoError(t, e.patchSlot(listing.TimeSlotPatch{Active: &off}))
}

func TestChangeSlotTimeWithoutBookingsOK(t *testing.T) {
	e := newEnv(t)
	start, end, wd := "09:00", "18:00", 0b0011111
	require.NoError(t, e.patchSlot(listing.TimeSlotPatch{StartTime: &start, EndTime: &end, Weekdays: &wd}))
	require.Equal(t, "09:00", e.str(t, `SELECT to_char(start_time, 'HH24:MI') FROM attraction_time_slots WHERE id = $1`, e.Slot.ID))
	require.NoError(t, e.closeDate(visitDate))
}

// TestRescheduleWaitsForPartnerScheduleWrite: a reschedule takes the listing
// FOR SHARE before reading the slots, so a partner write holding the listing
// FOR NO KEY UPDATE (here: closing the target date) is seen once it commits.
func TestRescheduleWaitsForPartnerScheduleWrite(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	ctx := context.Background()
	tx, err := e.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT 1 FROM listings WHERE id = $1 FOR NO KEY UPDATE`, e.L.ID)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := e.Svc.Reschedule(ctx, e.Customer, v.ID, newVisit, e.Slot.ID)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("reschedule did not wait for the listing lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	_, err = tx.Exec(ctx, `INSERT INTO attraction_closed_dates (listing_id, date) VALUES ($1, $2)`, e.L.ID, newVisit)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	requireErr(t, <-done, http.StatusUnprocessableEntity, "slot_unavailable")
	require.Equal(t, visitDate, e.str(t, `SELECT visit_date::text FROM booking_attraction_details WHERE booking_id = $1`, v.ID))
}
