package booking_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

const newVisit = "2026-11-20"

func (e *env) reschedule(t *testing.T, id uuid.UUID, date string) (booking.BookingView, error) {
	t.Helper()
	return e.Svc.Reschedule(context.Background(), e.Customer, id, date, e.Slot.ID)
}

func TestReschedule_OnceOnly(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	got, err := e.Svc.GetMine(context.Background(), e.Customer, v.ID)
	require.NoError(t, err)
	require.True(t, got.CanCancel)
	require.True(t, got.CanReschedule)

	// Through the customer route.
	tok := e.token(t, e.Customer, auth.RoleCustomer)
	path := "/v1/bookings/" + v.ID.String() + "/reschedule"
	requireCode(t, e.do(t, "POST", path, tok, map[string]any{"visit_date": "20/11/2026", "time_slot_id": e.Slot.ID}),
		http.StatusUnprocessableEntity, "invalid_booking")
	rec := e.do(t, "POST", path, tok, map[string]any{"visit_date": newVisit, "time_slot_id": e.Slot.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	out := decode[booking.BookingView](t, rec)
	require.Equal(t, newVisit, out.VisitDate)
	require.Equal(t, booking.StatusConfirmed, out.Status)
	require.True(t, out.CanCancel)
	require.False(t, out.CanReschedule)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_reschedules WHERE booking_id = $1
		AND from_visit_date = '2026-11-07' AND to_visit_date = '2026-11-20' AND created_by = $2`, v.ID, e.Customer))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history WHERE booking_id = $1
		AND from_status = 'confirmed' AND to_status = 'confirmed' AND actor_kind = 'customer' AND reason IS NOT NULL`, v.ID))

	_, err = e.reschedule(t, v.ID, "2026-11-21")
	requireErr(t, err, http.StatusConflict, "reschedule_not_allowed")
	requireCode(t, e.do(t, "POST", path, tok, map[string]any{"visit_date": "2026-11-21", "time_slot_id": e.Slot.ID}),
		http.StatusConflict, "reschedule_not_allowed")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_reschedules`))
}

func TestReschedule_AfterWindow409(t *testing.T) {
	e := newEnv(t)
	in := e.input()
	in.VisitDate = "2026-12-01"
	v, err := e.Svc.CreateAttraction(context.Background(), e.Customer, "far", in)
	require.NoError(t, err)
	e.pay(t, e.orderCode(t, v.ID), gross)

	// confirmed_at + 14 days is still allowed, + 15 days is not.
	e.Clock.T = testNow.Add(14 * 24 * time.Hour)
	got, err := e.Svc.GetMine(context.Background(), e.Customer, v.ID)
	require.NoError(t, err)
	require.True(t, got.CanReschedule)

	e.Clock.T = testNow.Add(15 * 24 * time.Hour)
	got, err = e.Svc.GetMine(context.Background(), e.Customer, v.ID)
	require.NoError(t, err)
	require.False(t, got.CanReschedule)
	_, err = e.reschedule(t, v.ID, "2026-12-05")
	requireErr(t, err, http.StatusConflict, "reschedule_not_allowed")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM booking_reschedules`))
}

func TestReschedule_AfterOriginalDatePassed409(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	e.Clock.T = time.Date(2026, 11, 7, 8, 0, 0, 0, clock.VN) // the original start
	_, err := e.reschedule(t, v.ID, newVisit)
	requireErr(t, err, http.StatusConflict, "reschedule_not_allowed")
	got, err := e.Svc.GetMine(context.Background(), e.Customer, v.ID)
	require.NoError(t, err)
	require.False(t, got.CanReschedule)
	require.True(t, got.CanCancel, "still cancellable (0% refund)")
}

func TestReschedule_ToClosedDate422(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	closed := time.Date(2026, 11, 20, 0, 0, 0, 0, clock.VN)
	require.NoError(t, e.Listing.AddClosedDate(context.Background(), e.P.ID, e.P.OwnerUserID, e.L.ID, closed, nil))

	_, err := e.reschedule(t, v.ID, newVisit)
	requireErr(t, err, http.StatusUnprocessableEntity, "slot_unavailable")
	_, err = e.Svc.Reschedule(context.Background(), e.Customer, v.ID, "2026-11-21", uuid.New())
	requireErr(t, err, http.StatusUnprocessableEntity, "slot_unavailable")
	_, err = e.reschedule(t, v.ID, "2026-11-04")
	requireErr(t, err, http.StatusUnprocessableEntity, "visit_date_past")
	// Today's 08:00 already started.
	_, err = e.reschedule(t, v.ID, "2026-11-05")
	requireErr(t, err, http.StatusUnprocessableEntity, "slot_started")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM booking_reschedules`))
	require.Equal(t, visitDate, e.str(t, `SELECT visit_date::text FROM booking_attraction_details WHERE booking_id = $1`, v.ID))
}

func TestReschedule_KeepsOriginalStartForPolicy(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	origStart := time.Date(2026, 11, 7, 8, 0, 0, 0, clock.VN)

	out, err := e.reschedule(t, v.ID, newVisit)
	require.NoError(t, err)
	newStart := time.Date(2026, 11, 20, 8, 0, 0, 0, clock.VN)
	require.True(t, out.ServiceStartAt.Equal(newStart), out.ServiceStartAt)
	require.True(t, out.ServiceEndAt.Equal(time.Date(2026, 11, 20, 17, 0, 0, 0, clock.VN)), out.ServiceEndAt)

	var orig, start time.Time
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT original_service_start_at, service_start_at FROM bookings WHERE id = $1`, v.ID).Scan(&orig, &start))
	require.True(t, orig.Equal(origStart), orig)
	require.True(t, start.Equal(newStart), start)
	require.Equal(t, newVisit, e.str(t, `SELECT visit_date::text FROM booking_attraction_details WHERE booking_id = $1`, v.ID))

	// No-show now follows the new date: nothing happens after the old one.
	e.Clock.T = time.Date(2026, 11, 8, 9, 0, 0, 0, clock.VN)
	require.Equal(t, 0, e.markNoShows(t))
	// Once the original start passed, a cancellation refunds 0%.
	_, err = e.Svc.CancelByCustomer(context.Background(), e.Customer, v.ID, nil)
	require.NoError(t, err)
	require.Empty(t, e.refunds(t))
	e.requireBalance(t, 0, gross-commission)
}
