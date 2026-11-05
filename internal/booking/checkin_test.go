package booking_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

const checkInPath = "/v1/partner/check-ins"

// onVisitDay is 08:30 on the fixture booking's visit date (Vietnam).
var onVisitDay = time.Date(2026, 11, 7, 8, 30, 0, 0, clock.VN)

func (e *env) qrToken(t *testing.T, id uuid.UUID) string {
	t.Helper()
	return e.str(t, `SELECT qr_token FROM bookings WHERE id = $1`, id)
}

// paidBooking is a confirmed (really paid) fixture booking.
func (e *env) paidBooking(t *testing.T) booking.BookingView {
	t.Helper()
	v := e.create(t, "k")
	e.pay(t, e.orderCode(t, v.ID), gross)
	return v
}

func TestCheckIn_CompletesAndSchedulesRelease(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	e.Clock.T = onVisitDay

	res, err := e.Svc.CheckIn(context.Background(), e.P.ID, e.P.OwnerUserID, e.qrToken(t, v.ID))
	require.NoError(t, err)
	require.False(t, res.Already)
	require.True(t, res.CheckedInAt.Equal(onVisitDay))
	require.Equal(t, v.ID, res.Booking.ID)
	require.Equal(t, "completed", res.Booking.Status)

	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM bookings
		WHERE id = $1 AND status = 'completed' AND checked_in_at = $2 AND completed_at = $2`, v.ID, onVisitDay))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'confirmed' AND to_status = 'completed'
		AND actor_kind = 'partner' AND actor_user_id = $2`, v.ID, e.P.OwnerUserID))
	due := e.releaseDueAt(t, v.ID)
	require.NotNil(t, due)
	require.True(t, due.Equal(onVisitDay.Add(24*time.Hour)), "release_delay_days = 1: %v", due)
	e.requireBalance(t, 342_000, 0)
}

func TestCheckIn_Idempotent(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	e.Clock.T = onVisitDay
	tok := e.qrToken(t, v.ID)
	_, err := e.Svc.CheckIn(context.Background(), e.P.ID, e.P.OwnerUserID, tok)
	require.NoError(t, err)

	e.Clock.Advance(10 * time.Minute)
	res, err := e.Svc.CheckIn(context.Background(), e.P.ID, e.P.OwnerUserID, tok)
	require.NoError(t, err)
	require.True(t, res.Already)
	require.True(t, res.CheckedInAt.Equal(onVisitDay))

	rec := e.do(t, "POST", checkInPath, e.token(t, e.P.OwnerUserID, auth.RolePartner), map[string]string{"qr_token": tok})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decode[struct {
		Already     bool      `json:"already"`
		CheckedInAt time.Time `json:"checked_in_at"`
	}](t, rec)
	require.True(t, body.Already)
	require.True(t, body.CheckedInAt.Equal(onVisitDay))

	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history WHERE to_status = 'completed'`))
	due := e.releaseDueAt(t, v.ID)
	require.True(t, due.Equal(onVisitDay.Add(24*time.Hour)))
}

// Review Focus 4: partner A scanning partner B's QR gets 404.
func TestCheckIn_OtherPartner404(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := e.paidBooking(t)
	e.Clock.T = onVisitDay
	tok := e.qrToken(t, v.ID)
	other, err := e.Partner.Create(ctx, e.Admin, partner.CreateInput{
		LegalName: "Công ty Beta", DisplayName: "Beta",
		OwnerEmail: "owner@beta.vn", OwnerPassword: "matkhau123",
		ServiceTypes: []string{attraction},
	})
	require.NoError(t, err)

	_, err = e.Svc.CheckIn(ctx, other.ID, other.OwnerUserID, tok)
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.CheckInByCode(ctx, other.ID, other.OwnerUserID, v.Code)
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.CheckIn(ctx, e.P.ID, e.P.OwnerUserID, "khong-ton-tai")
	requireErr(t, err, http.StatusNotFound, "not_found")

	otok := e.token(t, other.OwnerUserID, auth.RolePartner)
	requireCode(t, e.do(t, "POST", checkInPath, otok, map[string]string{"qr_token": tok}), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "POST", checkInPath, otok, map[string]string{"code": v.Code}), http.StatusNotFound, "not_found")
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
}

func TestCheckIn_WrongDate422(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	tok := e.qrToken(t, v.ID)
	for _, at := range []time.Time{
		time.Date(2026, 11, 6, 23, 59, 0, 0, clock.VN),
		time.Date(2026, 11, 8, 0, 0, 0, 0, clock.VN),
	} {
		e.Clock.T = at
		_, err := e.Svc.CheckIn(context.Background(), e.P.ID, e.P.OwnerUserID, tok)
		requireErr(t, err, http.StatusUnprocessableEntity, "checkin_wrong_date")
	}
	// 2026-11-07 00:30 VN is still 2026-11-06 in UTC: the VN date counts.
	e.Clock.T = time.Date(2026, 11, 6, 17, 30, 0, 0, time.UTC)
	_, err := e.Svc.CheckIn(context.Background(), e.P.ID, e.P.OwnerUserID, tok)
	require.NoError(t, err)
	require.Equal(t, "completed", e.bookingStatus(t, v.ID))
}

func TestCheckIn_NotConfirmed409(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	e.Clock.T = onVisitDay
	_, err := e.Svc.CheckIn(context.Background(), e.P.ID, e.P.OwnerUserID, e.qrToken(t, v.ID))
	requireErr(t, err, http.StatusConflict, "checkin_not_allowed")
}

func TestCheckIn_ByCode(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	e.Clock.T = onVisitDay
	ptok := e.token(t, e.P.OwnerUserID, auth.RolePartner)

	// Exactly one of qr_token and code.
	requireCode(t, e.do(t, "POST", checkInPath, ptok, map[string]string{}), http.StatusUnprocessableEntity, "invalid_checkin")
	requireCode(t, e.do(t, "POST", checkInPath, ptok, map[string]string{"qr_token": e.qrToken(t, v.ID), "code": v.Code}),
		http.StatusUnprocessableEntity, "invalid_checkin")
	requireCode(t, e.do(t, "POST", checkInPath, ptok, `{"code":"`+v.Code+`","x":1}`), http.StatusBadRequest, "invalid_request")
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))

	rec := e.do(t, "POST", checkInPath, ptok, map[string]string{"code": v.Code})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := decode[booking.CheckInResult](t, rec)
	require.False(t, res.Already)
	require.True(t, res.CheckedInAt.Equal(onVisitDay))
	require.Equal(t, "completed", res.Booking.Status)
	require.Nil(t, res.Booking.QRToken)

	again, err := e.Svc.CheckInByCode(context.Background(), e.P.ID, e.P.OwnerUserID, v.Code)
	require.NoError(t, err)
	require.True(t, again.Already)
}
