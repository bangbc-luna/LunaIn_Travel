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
)

// A1 departs 2026-11-07 07:00 (VN) and runs 180 minutes: service_end_at is
// 10:00. Settings: ride_checkin_window_minutes 60, ride_no_show_after_minutes
// 60, release_delay_days 1. Two seats on A1: 500.000, partner 450.000.
const rideOutNet = int64(450_000)

func (e *rideEnv) rideCheckIn(t *testing.T, id uuid.UUID) (booking.CheckInResult, error) {
	t.Helper()
	return e.Svc.CheckIn(context.Background(), e.RP.ID, e.RP.OwnerUserID, e.qrToken(t, id))
}

func (e *rideEnv) completeRides(t *testing.T) int {
	t.Helper()
	n, err := e.Svc.CompleteRides(context.Background())
	require.NoError(t, err)
	return n
}

// at sets the clock to minutes after A1's departure (negative: before).
func (e *rideEnv) at(minutes int) {
	e.Clock.T = e.A1.DepartureAt.Add(time.Duration(minutes) * time.Minute)
}

func TestRideCheckIn_WindowBounds(t *testing.T) {
	e := newRideEnv(t)
	first := e.paidOneWay(t, e.Customer, "r1", e.A1.ID, 2)
	second := e.paidOneWay(t, e.Customer, "r2", e.A1.ID, 2)
	third := e.paidOneWay(t, e.Customer, "r3", e.A1.ID, 2)

	e.at(-61) // 05:59
	_, err := e.rideCheckIn(t, first)
	requireErr(t, err, http.StatusUnprocessableEntity, "checkin_outside_window")
	require.Contains(t, err.Error(), "±60 phút")
	rec := e.do(t, "POST", checkInPath, e.token(t, e.RP.OwnerUserID, auth.RolePartner),
		map[string]string{"qr_token": e.qrToken(t, first)})
	requireCode(t, rec, http.StatusUnprocessableEntity, "checkin_outside_window")
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, first))

	e.at(-60) // 06:00
	res, err := e.rideCheckIn(t, first)
	require.NoError(t, err)
	require.False(t, res.Already)
	require.Equal(t, "checked_in", res.Booking.Status)

	e.at(60) // 08:00
	res, err = e.rideCheckIn(t, second)
	require.NoError(t, err)
	require.False(t, res.Already)
	require.True(t, res.CheckedInAt.Equal(e.A1.DepartureAt.Add(time.Hour)))

	e.at(61) // 08:01
	_, err = e.rideCheckIn(t, third)
	requireErr(t, err, http.StatusUnprocessableEntity, "checkin_outside_window")
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, third))
}

func TestRideCheckIn_SetsCheckedInNotCompleted(t *testing.T) {
	e := newRideEnv(t)
	id := e.paidOneWay(t, e.Customer, "r1", e.A1.ID, 2)
	e.at(-10)
	now := e.Clock.Now()

	res, err := e.rideCheckIn(t, id)
	require.NoError(t, err)
	require.False(t, res.Already)
	require.True(t, res.CheckedInAt.Equal(now))
	require.Equal(t, "checked_in", res.Booking.Status)
	require.False(t, res.Booking.CanReschedule)

	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM bookings
		WHERE id = $1 AND status = 'checked_in' AND checked_in_at = $2 AND completed_at IS NULL`, id, now))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'confirmed' AND to_status = 'checked_in'
		AND actor_kind = 'partner' AND actor_user_id = $2`, id, e.RP.OwnerUserID))
	require.Nil(t, e.releaseDueAt(t, id), "no release at check-in")
	e.requireRideBalance(t, rideOutNet, 0)
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))

	// Neither job touches it before service_end_at.
	require.Equal(t, 0, e.markNoShows(t))
	e.at(179)
	require.Equal(t, 0, e.markNoShows(t))
	require.Equal(t, 0, e.completeRides(t))
	require.Equal(t, booking.StatusCheckedIn, e.bookingStatus(t, id))
}

func TestRideCheckIn_Idempotent(t *testing.T) {
	e := newRideEnv(t)
	id := e.paidOneWay(t, e.Customer, "r1", e.A1.ID, 2)
	e.at(-30)
	first := e.Clock.Now()
	_, err := e.rideCheckIn(t, id)
	require.NoError(t, err)

	e.at(-20)
	res, err := e.rideCheckIn(t, id)
	require.NoError(t, err)
	require.True(t, res.Already)
	require.True(t, res.CheckedInAt.Equal(first))

	code := e.str(t, `SELECT code FROM bookings WHERE id = $1`, id)
	res, err = e.Svc.CheckInByCode(context.Background(), e.RP.ID, e.RP.OwnerUserID, code)
	require.NoError(t, err)
	require.True(t, res.Already)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND to_status = 'checked_in'`, id))

	// Completed later: a scan still answers Already, even out of the window.
	e.at(180)
	require.Equal(t, 1, e.completeRides(t))
	e.at(300)
	res, err = e.rideCheckIn(t, id)
	require.NoError(t, err)
	require.True(t, res.Already)
	require.True(t, res.CheckedInAt.Equal(first))
	require.Equal(t, "completed", res.Booking.Status)
}

func TestRideCheckIn_OtherPartner404(t *testing.T) {
	e := newRideEnv(t)
	id := e.paidOneWay(t, e.Customer, "r1", e.A1.ID, 2)
	other := e.ridePartner(t, "Thanh")
	e.at(0)
	tok := e.qrToken(t, id)

	_, err := e.Svc.CheckIn(context.Background(), other.ID, other.OwnerUserID, tok)
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.CheckIn(context.Background(), e.P.ID, e.P.OwnerUserID, tok)
	requireErr(t, err, http.StatusNotFound, "not_found")
	rec := e.do(t, "POST", checkInPath, e.token(t, other.OwnerUserID, auth.RolePartner), map[string]string{"qr_token": tok})
	requireCode(t, rec, http.StatusNotFound, "not_found")
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, id))
}

func TestRideCheckIn_RoundTripLegsIndependent(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)

	e.at(0)
	outAt := e.Clock.Now()
	_, err := e.rideCheckIn(t, out)
	require.NoError(t, err)
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, ret))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM bookings WHERE id = $1 AND checked_in_at IS NOT NULL`, ret))
	// The return leg's own window is around B1's departure.
	_, err = e.rideCheckIn(t, ret)
	requireErr(t, err, http.StatusUnprocessableEntity, "checkin_outside_window")

	e.Clock.T = e.B1.DepartureAt.Add(-15 * time.Minute)
	retAt := e.Clock.Now()
	res, err := e.rideCheckIn(t, ret)
	require.NoError(t, err)
	require.False(t, res.Already)
	require.True(t, res.CheckedInAt.Equal(retAt))
	// The outbound leg was not touched.
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM bookings
		WHERE id = $1 AND status = 'checked_in' AND checked_in_at = $2`, out, outAt))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND to_status = 'checked_in'`, out))
}

func TestRideCheckIn_NotConfirmed409(t *testing.T) {
	e := newRideEnv(t)
	v := e.createRide(t, e.Customer, "r1", rideInput(e.A1.ID, nil, 1))
	pending := v.Bookings[0].ID
	id := e.paidOneWay(t, e.Customer, "r2", e.A1.ID, 2)
	e.at(0)
	_, err := e.rideCheckIn(t, pending)
	requireErr(t, err, http.StatusConflict, "checkin_not_allowed")

	e.at(61)
	require.Equal(t, 1, e.markNoShows(t), "the pending booking is not a candidate")
	_, err = e.rideCheckIn(t, id)
	requireErr(t, err, http.StatusConflict, "checkin_not_allowed")
}

// A booking whose trip was cancelled cannot be checked in (409
// trip_cancelled); a leg checked in before the admin cancelled its trip is
// left checked in and completed at service_end_at with a normal release.
func TestRideCheckIn_TripCancelled409(t *testing.T) {
	e := newRideEnv(t)
	id := e.paidOneWay(t, e.Customer, "r1", e.A1.ID, 2)
	e.cancelTrip(t, e.A1.ID)
	require.Equal(t, booking.StatusCancelled, e.bookingStatus(t, id))

	e.at(0)
	_, err := e.rideCheckIn(t, id)
	requireErr(t, err, http.StatusConflict, "trip_cancelled")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM bookings WHERE id = $1 AND checked_in_at IS NOT NULL`, id))
}

func TestCompleteRides_CheckedInLegOnCancelledTrip(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)
	e.at(-60)
	checkedAt := e.Clock.Now()
	_, err := e.rideCheckIn(t, out)
	require.NoError(t, err)

	e.at(-30)
	res := e.cancelTrip(t, e.A1.ID)
	require.Empty(t, res.CancelledBookingIDs, "a checked-in leg is not cancelled")
	require.Equal(t, "cancelled", e.status(t, "ride_trips", e.A1.ID))
	require.Equal(t, booking.StatusCheckedIn, e.bookingStatus(t, out))
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, ret))
	e.requireRideBalance(t, 846_000, 0)

	// A scan of the checked-in leg still answers Already.
	again, err := e.rideCheckIn(t, out)
	require.NoError(t, err)
	require.True(t, again.Already)
	require.True(t, again.CheckedInAt.Equal(checkedAt))

	e.at(180)
	require.Equal(t, 1, e.completeRides(t))
	require.Equal(t, booking.StatusCompleted, e.bookingStatus(t, out))
	due := e.releaseDueAt(t, out)
	require.NotNil(t, due)
	require.True(t, due.Equal(e.Clock.Now().Add(24*time.Hour)), due)
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
	e.requireSeatsConsistent(t)

	e.Clock.T = *due
	require.Equal(t, 1, e.releaseDue(t))
	e.requireRideBalance(t, 396_000, rideOutNet)
	require.Empty(t, e.refunds(t))
}

func TestRideNoShow_AfterDeparturePlusSetting(t *testing.T) {
	e := newRideEnv(t)
	id := e.paidOneWay(t, e.Customer, "r1", e.A1.ID, 2)

	e.at(59) // 07:59
	require.Equal(t, 0, e.markNoShows(t))
	e.at(60) // 08:00: not yet (strictly after departure + 60')
	require.Equal(t, 0, e.markNoShows(t))
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, id))
	require.Nil(t, e.releaseDueAt(t, id))

	e.at(61) // 08:01
	now := e.Clock.Now()
	require.Equal(t, 1, e.markNoShows(t))
	require.Equal(t, booking.StatusNoShow, e.bookingStatus(t, id))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM bookings WHERE id = $1 AND no_show_at = $2`, id, now))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'confirmed' AND to_status = 'no_show' AND actor_kind = 'system'`, id))
	due := e.releaseDueAt(t, id)
	require.NotNil(t, due)
	require.True(t, due.Equal(now.Add(24*time.Hour)), due)
	require.Equal(t, 0, e.markNoShows(t), "marked once")
	require.Equal(t, 0, e.completeRides(t), "a no-show is never completed")

	// The partner is paid like a completed ride.
	e.Clock.T = *due
	require.Equal(t, 1, e.releaseDue(t))
	e.requireRideBalance(t, 0, rideOutNet)
}

func TestRideNoShow_SettingChangeApplies(t *testing.T) {
	e := newRideEnv(t)
	id := e.paidOneWay(t, e.Customer, "r1", e.A1.ID, 2)
	e.exec(t, `UPDATE platform_settings SET ride_no_show_after_minutes = 0`)

	e.at(0) // 07:00
	require.Equal(t, 0, e.markNoShows(t))
	e.at(1) // 07:01
	require.Equal(t, 1, e.markNoShows(t))
	require.Equal(t, booking.StatusNoShow, e.bookingStatus(t, id))
}

func TestCompleteRides_AtServiceEnd(t *testing.T) {
	e := newRideEnv(t)
	id := e.paidOneWay(t, e.Customer, "r1", e.A1.ID, 2)
	e.at(0)
	_, err := e.rideCheckIn(t, id)
	require.NoError(t, err)

	e.at(179) // 09:59
	require.Equal(t, 0, e.completeRides(t))
	require.Equal(t, booking.StatusCheckedIn, e.bookingStatus(t, id))

	e.at(180) // 10:00 = service_end_at
	now := e.Clock.Now()
	require.Equal(t, 1, e.completeRides(t))
	require.Equal(t, booking.StatusCompleted, e.bookingStatus(t, id))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM bookings WHERE id = $1 AND completed_at = $2`, id, now))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'checked_in' AND to_status = 'completed'
		AND actor_kind = 'system' AND actor_user_id IS NULL`, id))
	due := e.releaseDueAt(t, id)
	require.NotNil(t, due)
	require.True(t, due.Equal(now.Add(24*time.Hour)), due)
	require.Equal(t, 0, e.completeRides(t), "completed once")
	e.requireRideBalance(t, rideOutNet, 0)

	e.Clock.T = due.Add(-time.Second)
	require.Equal(t, 0, e.releaseDue(t))
	e.Clock.T = *due
	require.Equal(t, 1, e.releaseDue(t))
	e.requireRideBalance(t, 0, 450_000)
}

func TestRideNoShowKeepsSeats(t *testing.T) {
	e := newRideEnv(t)
	noShow := e.paidOneWay(t, e.Customer, "r1", e.A1.ID, 2)
	done := e.paidOneWay(t, e.Customer, "r2", e.A1.ID, 3)
	require.Equal(t, 5, e.seatsReserved(t, e.A1.ID))

	e.at(0)
	_, err := e.rideCheckIn(t, done)
	require.NoError(t, err)
	require.Equal(t, 5, e.seatsReserved(t, e.A1.ID))
	e.at(61)
	require.Equal(t, 1, e.markNoShows(t))
	require.Equal(t, booking.StatusNoShow, e.bookingStatus(t, noShow))
	require.Equal(t, 5, e.seatsReserved(t, e.A1.ID))
	e.at(180)
	require.Equal(t, 1, e.completeRides(t))
	require.Equal(t, booking.StatusCompleted, e.bookingStatus(t, done))
	require.Equal(t, 5, e.seatsReserved(t, e.A1.ID))
	e.requireSeatsConsistent(t)
}

func TestReschedule_RideRejected409(t *testing.T) {
	e := newRideEnv(t)
	id := e.paidOneWay(t, e.Customer, "r1", e.A1.ID, 2)

	_, err := e.Svc.Reschedule(context.Background(), e.Customer, id, "2026-11-20", e.Slot.ID)
	requireErr(t, err, http.StatusConflict, "reschedule_not_allowed")
	rec := e.do(t, "POST", "/v1/bookings/"+id.String()+"/reschedule", e.token(t, e.Customer, auth.RoleCustomer),
		map[string]any{"visit_date": "2026-11-20", "time_slot_id": e.Slot.ID})
	requireCode(t, rec, http.StatusConflict, "reschedule_not_allowed")
	got, err := e.Svc.GetMine(context.Background(), e.Customer, id)
	require.NoError(t, err)
	require.False(t, got.CanReschedule)
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM booking_reschedules`))
	require.True(t, got.ServiceStartAt.Equal(e.A1.DepartureAt))
}

// The attraction no-show rule (service_end_at passed) is unchanged next to
// the ride rule, and each list only takes its own service type.
func TestAttractionNoShowUnchanged(t *testing.T) {
	e := newRideEnv(t)
	att := e.paidBooking(t).ID
	ride := e.paidOneWay(t, e.Customer, "r1", e.A1.ID, 2)
	e.at(0)
	_, err := e.rideCheckIn(t, ride)
	require.NoError(t, err)

	// The attraction visit (2026-11-07 08:00) ends at 00:00 the next day.
	e.Clock.T = serviceEnd
	require.Equal(t, 0, e.markNoShows(t))
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, att))
	require.Equal(t, 1, e.completeRides(t), "only the checked-in ride")
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, att))

	e.Clock.T = serviceEnd.Add(time.Minute)
	require.Equal(t, 1, e.markNoShows(t))
	require.Equal(t, booking.StatusNoShow, e.bookingStatus(t, att))
	due := e.releaseDueAt(t, att)
	require.NotNil(t, due)
	require.True(t, due.Equal(e.Clock.Now().Add(24*time.Hour)), due)
	require.Equal(t, booking.StatusCompleted, e.bookingStatus(t, ride))
}
