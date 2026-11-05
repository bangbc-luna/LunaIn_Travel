package booking_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const tripCancelReason = "Xe hỏng, nhà xe hủy chuyến"

var testBank = booking.BankAccount{BankBIN: "970436", AccountNumber: "0011001234567", AccountHolder: "Trần Thị Bình"}

// paidRoundTrip books and pays 2 seats on A1 (500.000) and B1 (440.000).
func (e *rideEnv) paidRoundTrip(t *testing.T) (booking.CheckoutView, uuid.UUID, uuid.UUID) {
	t.Helper()
	v, out, ret, oc := e.roundTrip(t)
	e.pay(t, oc, rideTotal)
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, out))
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, ret))
	e.requireRideBalance(t, 846_000, 0)
	return v, out, ret
}

// paidOneWay books and pays seats on trip for customer (idempotency key key).
func (e *rideEnv) paidOneWay(t *testing.T, customer uuid.UUID, key string, trip uuid.UUID, seats int) uuid.UUID {
	t.Helper()
	v := e.createRide(t, customer, key, rideInput(trip, nil, seats))
	require.Len(t, v.Bookings, 1)
	_, _, amount, oc := e.checkoutPayment(t, v.ID)
	e.pay(t, oc, amount)
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, v.Bookings[0].ID))
	return v.Bookings[0].ID
}

// checkIn forces a ride booking to checked_in with raw SQL (Task 8 does it
// for real).
func (e *rideEnv) checkIn(t *testing.T, id uuid.UUID) {
	t.Helper()
	e.exec(t, `UPDATE bookings SET status = 'checked_in', checked_in_at = $2 WHERE id = $1`, id, e.Clock.Now())
}

func (e *rideEnv) cancelTrip(t *testing.T, tripID uuid.UUID) booking.TripCancelResult {
	t.Helper()
	res, err := e.Svc.CancelTrip(context.Background(), e.Admin, tripID, tripCancelReason)
	require.NoError(t, err)
	return res
}

func TestCancelRide_CustomerRoundTripPerLegPolicy(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)
	before := e.emails(t)

	// Cancelled through the return leg's id: both legs go.
	v, err := e.Svc.CancelByCustomer(context.Background(), e.Customer, ret, &testBank)
	require.NoError(t, err)
	require.Equal(t, booking.StatusCancelled, v.Status)
	require.False(t, v.CanCancel)

	// A1 departs in 46h (50%), B1 in more than 48h (100%).
	e.requireCancelled(t, out, "customer", 250_000, payment.RefundCustomerCancel)
	e.requireCancelled(t, ret, "customer", rideRetGross, payment.RefundCustomerCancel)
	rs := e.refunds(t)
	require.Len(t, rs, 2)
	for _, r := range rs {
		require.Equal(t, payment.RefundPending, r.Status)
		require.Equal(t, "970436", *r.BankBin)
		require.Equal(t, "TRAN THI BINH", *r.Holder)
	}
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
	e.requireSeatsConsistent(t)
	// (450.000 − 250.000) + (396.000 − 440.000), released at once.
	e.requireRideBalance(t, 0, 156_000)
	require.Equal(t, int64(50_000), e.platformNet(t, out), "the commission is kept")
	require.Equal(t, int64(44_000), e.platformNet(t, ret))
	require.Equal(t, before+2, e.emails(t))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM admin_audit_log WHERE action IN ('booking.cancel', 'ride_trip.cancel')`))

	// Nothing left to cancel.
	_, err = e.Svc.CancelByCustomer(context.Background(), e.Customer, out, nil)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
}

func TestCancelRide_AfterOutboundDeparted409(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)
	e.Clock.T = e.A1.DepartureAt.Add(time.Minute)

	for _, id := range []uuid.UUID{ret, out} {
		_, err := e.Svc.CancelByCustomer(context.Background(), e.Customer, id, nil)
		requireErr(t, err, http.StatusConflict, "invalid_booking_state")
		require.Contains(t, err.Error(), "Chỉ hủy được khi chưa đi chiều nào")
	}
	tok := e.token(t, e.Customer, auth.RoleCustomer)
	requireCode(t, e.do(t, "POST", "/v1/bookings/"+ret.String()+"/cancel", tok, map[string]any{}),
		http.StatusConflict, "invalid_booking_state")
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, out))
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, ret))
	require.Empty(t, e.refunds(t))
	e.requireRideBalance(t, 846_000, 0)
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
}

func TestCancelRide_CheckedInLeg409(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)
	e.checkIn(t, out)
	_, err := e.Svc.CancelByCustomer(context.Background(), e.Customer, ret, nil)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, ret))
}

func TestCancelRide_OneWay(t *testing.T) {
	e := newRideEnv(t)
	id := e.paidOneWay(t, e.Customer, "ow", e.A1.ID, 2)
	e.requireRideBalance(t, 450_000, 0)

	v, err := e.Svc.CancelByCustomer(context.Background(), e.Customer, id, nil)
	require.NoError(t, err)
	require.Equal(t, booking.StatusCancelled, v.Status)
	e.requireCancelled(t, id, "customer", 250_000, payment.RefundCustomerCancel)
	require.Equal(t, payment.RefundAwaitingBankInfo, e.refunds(t)[0].Status)
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
	e.requireSeatsConsistent(t)
	e.requireRideBalance(t, 0, 200_000)
}

func TestCancelRide_OtherCustomer404(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)
	other := e.customer(t, "khac@gmail.com")

	for _, id := range []uuid.UUID{out, ret} {
		_, err := e.Svc.CancelByCustomer(context.Background(), other, id, nil)
		requireErr(t, err, http.StatusNotFound, "not_found")
		// Ownership comes before the bank account.
		_, err = e.Svc.CancelByCustomer(context.Background(), other, id, &booking.BankAccount{BankBIN: "1"})
		requireErr(t, err, http.StatusNotFound, "not_found")
	}
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, out))
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, ret))
	require.Empty(t, e.refunds(t))
}

func TestCancelRideByAdmin_ForceMajeureBothLegs(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)

	v, err := e.Svc.CancelByAdmin(context.Background(), e.Admin, out, booking.CancelModeForceMajeure, "Đèo bị sạt lở")
	require.NoError(t, err)
	require.Equal(t, booking.StatusCancelled, v.Status)

	e.requireCancelled(t, out, "admin_force_majeure", rideOutGross, payment.RefundAdminCancel)
	e.requireCancelled(t, ret, "admin_force_majeure", rideRetGross, payment.RefundAdminCancel)
	e.requireRideBalance(t, 0, 0)
	require.Equal(t, int64(0), e.platformNet(t, out), "the commission is waived")
	require.Equal(t, int64(0), e.platformNet(t, ret))
	for _, id := range []uuid.UUID{out, ret} {
		require.Equal(t, 1, e.count(t, `SELECT count(*) FROM admin_audit_log
			WHERE action = 'booking.cancel' AND target_id = $1 AND actor_user_id = $2 AND reason = $3`,
			id, e.Admin, "Đèo bị sạt lở"))
	}
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
	e.requireSeatsConsistent(t)
}

// Admin policy mode: each leg by its own departure, the commission kept.
func TestCancelRideByAdmin_PolicyBothLegs(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)
	tok := e.token(t, e.Admin, auth.RoleSuperAdmin)
	rec := e.do(t, "POST", "/v1/admin/bookings/"+ret.String()+"/cancel", tok,
		map[string]any{"mode": "policy", "reason": "Khách gọi tổng đài hủy"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	e.requireCancelled(t, out, "admin_policy", 250_000, payment.RefundAdminCancel)
	e.requireCancelled(t, ret, "admin_policy", rideRetGross, payment.RefundAdminCancel)
	e.requireRideBalance(t, 0, 156_000)
	e.requireSeatsConsistent(t)
}

// Admin cancels the return leg while the outbound is checked in: only the
// return goes; the checked-in leg itself cannot be cancelled.
func TestCancelRideByAdmin_CheckedInLegStays(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)
	e.checkIn(t, out)

	_, err := e.Svc.CancelByAdmin(context.Background(), e.Admin, out, booking.CancelModeForceMajeure, "x")
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")

	_, err = e.Svc.CancelByAdmin(context.Background(), e.Admin, ret, booking.CancelModeForceMajeure, "Xe về hỏng")
	require.NoError(t, err)
	require.Equal(t, "checked_in", e.bookingStatus(t, out))
	e.requireCancelled(t, ret, "admin_force_majeure", rideRetGross, payment.RefundAdminCancel)
	require.Len(t, e.refunds(t), 1)
	e.requireRideBalance(t, 450_000, 0)
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
	e.requireSeatsConsistent(t)
}

// Review Focus 4: the trip of one leg is cancelled; both legs go.
func TestCancelTrip_RoundTripCancelsBothLegs(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)

	res := e.cancelTrip(t, e.B1.ID)
	require.Equal(t, e.B1.ID, res.TripID)
	require.ElementsMatch(t, []uuid.UUID{out, ret}, res.CancelledBookingIDs)
	require.Equal(t, rideTotal, res.RefundTotal)

	e.requireCancelled(t, out, "admin_force_majeure", rideOutGross, payment.RefundAdminCancel)
	e.requireCancelled(t, ret, "admin_force_majeure", rideRetGross, payment.RefundAdminCancel)
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
	e.requireSeatsConsistent(t)
	require.Equal(t, "cancelled", e.status(t, "ride_trips", e.B1.ID))
	require.Equal(t, "admin", e.str(t, `SELECT cancel_source FROM ride_trips WHERE id = $1`, e.B1.ID))
	require.Equal(t, "scheduled", e.status(t, "ride_trips", e.A1.ID))
	e.requireRideBalance(t, 0, 0)
	require.Equal(t, int64(0), e.platformNet(t, out))
	require.Equal(t, int64(0), e.platformNet(t, ret))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM admin_audit_log
		WHERE action = 'ride_trip.cancel' AND target_kind = 'ride_trip' AND target_id = $1 AND actor_user_id = $2 AND reason = $3
		  AND before->>'status' = 'scheduled' AND (before->>'seats_reserved')::int = 2
		  AND after->>'status' = 'cancelled' AND (after->>'seats_reserved')::int = 0
		  AND (after->>'refund_total')::bigint = 940000 AND jsonb_array_length(after->'cancelled_booking_ids') = 2`,
		e.B1.ID, e.Admin, tripCancelReason))
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM admin_audit_log WHERE action = 'booking.cancel'`))
}

// Review Focus 4: the outbound is checked in, so only the return goes.
func TestCancelTrip_OutboundCheckedInOnlyReturnCancelled(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)
	e.checkIn(t, out)

	res := e.cancelTrip(t, e.B1.ID)
	require.Equal(t, []uuid.UUID{ret}, res.CancelledBookingIDs)
	require.Equal(t, rideRetGross, res.RefundTotal)

	require.Equal(t, "checked_in", e.bookingStatus(t, out))
	e.requireCancelled(t, ret, "admin_force_majeure", rideRetGross, payment.RefundAdminCancel)
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
	e.requireSeatsConsistent(t)
	e.requireRideBalance(t, 450_000, 0)
	require.Equal(t, int64(450_000), e.bookingPending(t, out))
}

func TestCancelTrip_ManyCheckouts(t *testing.T) {
	e := newRideEnv(t)
	other := e.customer(t, "khac@gmail.com")
	b1 := e.paidOneWay(t, e.Customer, "c1", e.B1.ID, 2)
	b2 := e.paidOneWay(t, other, "c2", e.B1.ID, 1)
	pend := e.createRide(t, e.customer(t, "ba@gmail.com"), "c3", rideInput(e.B1.ID, nil, 1))
	require.Equal(t, 4, e.seatsReserved(t, e.B1.ID))

	res := e.cancelTrip(t, e.B1.ID)
	require.ElementsMatch(t, []uuid.UUID{b1, b2}, res.CancelledBookingIDs)
	require.Equal(t, int64(660_000), res.RefundTotal)

	e.requireCancelled(t, b1, "admin_force_majeure", 440_000, payment.RefundAdminCancel)
	e.requireCancelled(t, b2, "admin_force_majeure", 220_000, payment.RefundAdminCancel)
	require.Equal(t, booking.StatusPendingPayment, e.bookingStatus(t, pend.Bookings[0].ID))
	require.Equal(t, 1, e.seatsReserved(t, e.B1.ID), "the pending hold is left to the expiry job")
	e.requireSeatsConsistent(t)
	e.requireRideBalance(t, 0, 0)
}

func TestCancelTrip_AlreadyCancelled409(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)
	tok := e.token(t, e.Admin, auth.RoleSuperAdmin)
	path := "/v1/admin/ride-trips/" + e.B1.ID.String() + "/cancel"

	requireCode(t, e.do(t, "POST", path, tok, map[string]any{"reason": " "}), http.StatusUnprocessableEntity, "invalid_cancel")
	requireCode(t, e.do(t, "POST", path, tok, map[string]any{"reason": "x", "extra": 1}), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "POST", path, e.token(t, e.Customer, auth.RoleCustomer), map[string]any{"reason": "x"}),
		http.StatusForbidden, "forbidden")
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, ret))

	rec := e.do(t, "POST", path, tok, map[string]any{"reason": tripCancelReason})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := decode[booking.TripCancelResult](t, rec)
	require.Equal(t, e.B1.ID, res.TripID)
	require.ElementsMatch(t, []uuid.UUID{out, ret}, res.CancelledBookingIDs)
	require.Equal(t, rideTotal, res.RefundTotal)

	requireCode(t, e.do(t, "POST", path, tok, map[string]any{"reason": "lần 2"}), http.StatusConflict, "trip_not_scheduled")
	_, err := e.Svc.CancelTrip(context.Background(), e.Admin, e.B1.ID, "lần 2")
	requireErr(t, err, http.StatusConflict, "trip_not_scheduled")
	require.Len(t, e.refunds(t), 2)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM admin_audit_log WHERE action = 'ride_trip.cancel'`))

	// An empty trip is cancelled with no booking.
	rec = e.do(t, "POST", "/v1/admin/ride-trips/"+e.A1.ID.String()+"/cancel", tok, map[string]any{"reason": "Không có khách"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"trip_id":"`+e.A1.ID.String()+`","cancelled_booking_ids":[],"refund_total":0}`, rec.Body.String())
}

func TestCancelTrip_MalformedID404(t *testing.T) {
	e := newRideEnv(t)
	tok := e.token(t, e.Admin, auth.RoleSuperAdmin)
	requireCode(t, e.do(t, "POST", "/v1/admin/ride-trips/not-a-uuid/cancel", tok, map[string]any{"reason": "x"}),
		http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "POST", "/v1/admin/ride-trips/"+uuid.NewString()+"/cancel", tok, map[string]any{"reason": "x"}),
		http.StatusNotFound, "not_found")
	_, err := e.Svc.CancelTrip(context.Background(), e.Admin, uuid.New(), "x")
	requireErr(t, err, http.StatusNotFound, "not_found")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM admin_audit_log WHERE action IN ('booking.cancel', 'ride_trip.cancel')`))
}

func TestCanCancelView_Ride(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)
	view := func(id uuid.UUID) (booking.BookingView, booking.AdminBookingView) {
		t.Helper()
		mine, err := e.Svc.GetMine(context.Background(), e.Customer, id)
		require.NoError(t, err)
		adm, err := e.Svc.AdminGet(context.Background(), id)
		require.NoError(t, err)
		return mine, adm
	}
	for _, id := range []uuid.UUID{out, ret} {
		mine, adm := view(id)
		require.True(t, mine.CanCancel)
		require.True(t, adm.CanCancel)
	}
	e.Clock.T = e.A1.DepartureAt.Add(time.Minute)
	for _, id := range []uuid.UUID{out, ret} {
		mine, adm := view(id)
		require.False(t, mine.CanCancel, "after the outbound departs, no leg can be cancelled")
		require.False(t, adm.CanCancel)
	}
}

// Customer cancel and admin trip cancel race on the same checkout: no
// deadlock, each leg is cancelled and refunded exactly once.
func TestCancelRide_ConcurrentCustomerVsTripCancel(t *testing.T) {
	for range 5 {
		e := newRideEnv(t)
		_, out, ret := e.paidRoundTrip(t)

		var wg sync.WaitGroup
		var custErr, tripErr error
		var res booking.TripCancelResult
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, custErr = e.Svc.CancelByCustomer(context.Background(), e.Customer, ret, nil)
		}()
		go func() {
			defer wg.Done()
			res, tripErr = e.Svc.CancelTrip(context.Background(), e.Admin, e.A1.ID, tripCancelReason)
		}()
		wg.Wait()
		require.NoError(t, tripErr)
		require.Equal(t, "cancelled", e.status(t, "ride_trips", e.A1.ID))

		kind := "admin_force_majeure"
		if custErr == nil {
			kind = "customer"
			require.Empty(t, res.CancelledBookingIDs)
		} else {
			var ae *httpx.AppError
			require.True(t, errors.As(custErr, &ae), custErr)
			require.Equal(t, "invalid_booking_state", ae.Code)
			require.Len(t, res.CancelledBookingIDs, 2)
		}
		require.Equal(t, kind, e.str(t, `SELECT cancel_kind FROM bookings WHERE id = $1`, out))
		require.Equal(t, kind, e.str(t, `SELECT cancel_kind FROM bookings WHERE id = $1`, ret))
		require.Len(t, e.refunds(t), 2)
		require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
		require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
		e.requireSeatsConsistent(t)
		if kind == "customer" {
			e.requireRideBalance(t, 0, 156_000)
		} else {
			e.requireRideBalance(t, 0, 0)
		}
	}
}

// A late webhook for another checkout on the same trips races an admin trip
// cancel: both lock the trips first, so neither deadlocks.
func TestCancelTrip_ConcurrentWithPayment(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret := e.paidRoundTrip(t)
	other := e.customer(t, "khac@gmail.com")
	v := e.createRide(t, other, "o", rideInput(e.A1.ID, ptrID(e.B1.ID), 1))
	_, _, amount, oc := e.checkoutPayment(t, v.ID)

	var wg sync.WaitGroup
	var tripErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, tripErr = e.Svc.CancelTrip(context.Background(), e.Admin, e.B1.ID, tripCancelReason)
	}()
	go func() {
		defer wg.Done()
		e.pay(t, oc, amount)
	}()
	wg.Wait()
	require.NoError(t, tripErr)
	require.Equal(t, booking.StatusCancelled, e.bookingStatus(t, out))
	require.Equal(t, booking.StatusCancelled, e.bookingStatus(t, ret))
	// The other checkout was either confirmed first (then cancelled with the
	// trip) or paid after the trip was cancelled (then refunded in full).
	st := e.bookingStatus(t, v.Bookings[0].ID)
	require.Contains(t, []string{booking.StatusCancelled, "expired"}, st, st)
	require.Equal(t, st, e.bookingStatus(t, v.Bookings[1].ID))
	e.requireSeatsConsistent(t)
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
	e.requireRideBalance(t, 0, 0)
}
