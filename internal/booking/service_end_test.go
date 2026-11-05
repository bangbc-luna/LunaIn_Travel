package booking_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

// Ruling 26 (spec §6.4): an attraction ticket is usable the whole visit day.
// service_end_at is 00:00 (VN) of the next day, whatever the slot's hours:
// with a 08:00-10:00 slot, at 15:00 on the visit day the booking is not a
// no-show, can be checked in and can still be cancelled (0%: the original
// start has passed); only after midnight does the no-show job take it.
func TestAttractionServiceEndsAtEndOfVisitDay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	end := "10:00"
	require.NoError(t, e.patchSlot(listing.TimeSlotPatch{EndTime: &end}))
	const day = "2026-11-06"
	book := func(key string) booking.BookingView {
		t.Helper()
		in := e.input()
		in.VisitDate = day
		v, err := e.Svc.CreateAttraction(ctx, e.Customer, key, in)
		require.NoError(t, err)
		require.True(t, time.Date(2026, 11, 7, 0, 0, 0, 0, clock.VN).Equal(v.ServiceEndAt), v.ServiceEndAt)
		require.Equal(t, booking.SlotView{ID: e.Slot.ID, StartTime: "08:00", EndTime: "10:00"}, v.TimeSlot)
		e.pay(t, e.orderCode(t, v.ID), gross)
		require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, v.ID))
		return v
	}
	checkedIn, cancelled, noShow := book("a"), book("b"), book("c")
	e.requireBalance(t, 3*(gross-commission), 0)

	e.Clock.T = time.Date(2026, 11, 6, 15, 0, 0, 0, clock.VN)
	require.Equal(t, 0, e.markNoShows(t), "slot over but the visit day is not")
	res, err := e.Svc.CheckIn(ctx, e.P.ID, e.P.OwnerUserID, e.qrToken(t, checkedIn.ID))
	require.NoError(t, err)
	require.Equal(t, booking.StatusCompleted, res.Booking.Status)
	got, err := e.Svc.GetMine(ctx, e.Customer, cancelled.ID)
	require.NoError(t, err)
	require.True(t, got.CanCancel)
	e.cancel(t, cancelled.ID)
	require.Empty(t, e.refunds(t), "original start passed: 0%")
	e.requireBalance(t, 2*(gross-commission), gross-commission)

	e.Clock.T = time.Date(2026, 11, 6, 23, 59, 0, 0, clock.VN)
	require.Equal(t, 0, e.markNoShows(t))
	e.Clock.T = time.Date(2026, 11, 7, 0, 0, 1, 0, clock.VN)
	require.Equal(t, 1, e.markNoShows(t))
	require.Equal(t, booking.StatusNoShow, e.bookingStatus(t, noShow.ID))
	require.Equal(t, booking.StatusCompleted, e.bookingStatus(t, checkedIn.ID))
	e.requireBalance(t, 2*(gross-commission), gross-commission)
	require.NoError(t, e.Ledger.Verify(ctx, e.P.ID))
}
