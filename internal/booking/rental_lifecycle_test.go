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
)

// R (request_test.go) is Vios over [07/11 08:00, 09/11 10:00), 2.500.000,
// accepted at 10:00 and paid at 10:15 on 05/11 (paidR): confirmed, partner
// pending 2.250.000, commission 250.000.

const (
	reasonAutoComplete = "tự hoàn thành sau 24 giờ quá giờ trả"
	reasonAutoNoShow   = "khách không nhận xe trước giờ trả"
)

var (
	rPickup = vnAt(2026, 11, 7, 8, 0)
	rReturn = vnAt(2026, 11, 9, 10, 0)
)

// paidR is R really paid at 10:15 on 05/11.
func (r *rentalEnv) paidR(t *testing.T) booking.BookingView {
	t.Helper()
	v := r.acceptedR(t)
	r.Clock.T = vnAt(2026, 11, 5, 10, 15)
	r.pay(t, r.orderCode(t, v.ID), rentalGross)
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, v.ID))
	r.requireRentalBalance(t, rentalPartnerNet, 0)
	return v
}

// paidRental requests in at the current clock, accepts and pays it.
func (r *rentalEnv) paidRental(t *testing.T, key string, in booking.CreateRentalInput) booking.BookingView {
	t.Helper()
	v := r.createRental(t, r.Customer, key, in)
	r.accept(t, v.ID)
	r.pay(t, r.orderCode(t, v.ID), v.GrossAmount)
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, v.ID))
	return v
}

// paidR2 is Vision at "Cửa hàng" over R's period, paid right away:
// 3 days × 150.000 = 450.000.
func (r *rentalEnv) paidR2(t *testing.T) booking.BookingView {
	t.Helper()
	return r.paidRental(t, "r2", r.rentalInput(r.Vision.ID, r.P0.ID, rPickup, rReturn))
}

// handOver scans the booking's QR as the rental partner's owner.
func (r *rentalEnv) handOver(t *testing.T, id uuid.UUID) (booking.CheckInResult, error) {
	t.Helper()
	return r.Svc.CheckIn(context.Background(), r.RP.ID, r.RP.OwnerUserID, r.qrToken(t, id))
}

func (r *rentalEnv) handOverAt(t *testing.T, id uuid.UUID, at time.Time) {
	t.Helper()
	r.Clock.T = at
	res, err := r.handOver(t, id)
	require.NoError(t, err)
	require.Equal(t, booking.StatusInService, res.Booking.Status)
}

func (r *rentalEnv) returned(t *testing.T, id uuid.UUID) booking.BookingView {
	t.Helper()
	v, err := r.Svc.MarkRentalReturned(context.Background(), r.RP.ID, r.RP.OwnerUserID, id)
	require.NoError(t, err)
	return v
}

func (r *rentalEnv) finishRentals(t *testing.T) int {
	t.Helper()
	n, err := r.Svc.FinishRentals(context.Background())
	require.NoError(t, err)
	return n
}

func (r *rentalEnv) timeCol(t *testing.T, col string, id uuid.UUID) *time.Time {
	t.Helper()
	var at *time.Time
	require.NoError(t, r.Pool.QueryRow(context.Background(),
		`SELECT `+col+` FROM bookings WHERE id = $1`, id).Scan(&at))
	return at
}

func requireAt(t *testing.T, want time.Time, got *time.Time, msg string) {
	t.Helper()
	require.NotNil(t, got, msg)
	require.True(t, got.Equal(want), "%s: want %v, got %v", msg, want, *got)
}

// otherRequest is another customer's request for Vios over [pickup, ret) at
// the current clock.
func (r *rentalEnv) otherRequest(customer uuid.UUID, key string, pickup, ret time.Time) (booking.BookingView, error) {
	return r.Svc.CreateRental(context.Background(), customer, key, r.rentalInput(r.Vios.ID, r.P0.ID, pickup, ret))
}

func TestCancelRental_CustomerPolicy(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	// 45h30 before pickup (07/11 08:00): tier 24h..48h, 50%.
	r.Clock.T = vnAt(2026, 11, 5, 10, 30)
	acc := payment.BankAccount{BankBIN: "970436", AccountNumber: "0011001234567", AccountHolder: "NGUYEN VAN AN"}
	out, err := r.Svc.CancelByCustomer(context.Background(), r.Customer, v.ID, &acc)
	require.NoError(t, err)
	require.Equal(t, booking.StatusCancelled, out.Status)
	require.False(t, out.CanCancel)

	r.requireCancelled(t, v.ID, "customer", 1_250_000, payment.RefundCustomerCancel)
	require.Equal(t, payment.RefundPending, r.refunds(t)[0].Status)
	// 2.250.000 − 1.250.000, released at once.
	r.requireRentalBalance(t, 0, 1_000_000)
	require.Equal(t, rentalCommission, r.platformNet(t, v.ID), "the commission is kept")
	r.requireReleased(t, v.ID)

	// The vehicle is free again over R's period.
	other := r.customer(t, "khac@gmail.com")
	_, err = r.otherRequest(other, "again", rPickup, rReturn)
	require.NoError(t, err)
}

func TestCancelRental_AdminForceMajeure(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	r.Clock.T = vnAt(2026, 11, 5, 10, 30)
	out, err := r.Svc.CancelByAdmin(context.Background(), r.Admin, v.ID, "force_majeure", "Đường lên Đà Lạt bị sạt lở")
	require.NoError(t, err)
	require.Equal(t, booking.StatusCancelled, out.Status)

	r.requireCancelled(t, v.ID, "admin_force_majeure", rentalGross, payment.RefundAdminCancel)
	r.requireRentalBalance(t, 0, 0)
	require.Equal(t, int64(0), r.platformNet(t, v.ID), "the commission is waived")
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM admin_audit_log
		WHERE action = 'booking.cancel' AND target_id = $1 AND actor_user_id = $2 AND reason = $3`,
		v.ID, r.Admin, "Đường lên Đà Lạt bị sạt lở"))
	r.requireReleased(t, v.ID)
}

// Handed over, returned or no-show: nobody cancels any more.
func TestCancelRental_InService409(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	r.handOverAt(t, v.ID, rPickup)

	_, err := r.Svc.CancelByCustomer(context.Background(), r.Customer, v.ID, nil)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	_, err = r.Svc.CancelByAdmin(context.Background(), r.Admin, v.ID, "force_majeure", "Thử hủy")
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	mine, err := r.Svc.GetMine(context.Background(), r.Customer, v.ID)
	require.NoError(t, err)
	require.False(t, mine.CanCancel)

	r.Clock.T = vnAt(2026, 11, 8, 10, 0)
	r.returned(t, v.ID)
	_, err = r.Svc.CancelByCustomer(context.Background(), r.Customer, v.ID, nil)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	require.Empty(t, r.refunds(t))
	r.requireRentalBalance(t, rentalPartnerNet, 0)
}

func TestRentalHandover_Window(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	r2 := r.paidR2(t)
	ctx := context.Background()

	r.Clock.T = vnAt(2026, 11, 7, 5, 59)
	_, err := r.handOver(t, v.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "handover_outside_window")
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, v.ID))

	// Another partner scanning R's QR does not find it.
	r.Clock.T = vnAt(2026, 11, 7, 6, 0)
	_, err = r.Svc.CheckIn(ctx, r.P.ID, r.P.OwnerUserID, r.qrToken(t, v.ID))
	requireErr(t, err, http.StatusNotFound, "not_found")

	tok := r.token(t, r.RP.OwnerUserID, auth.RolePartner)
	rec := r.do(t, http.MethodPost, checkInPath, tok, map[string]string{"qr_token": r.qrToken(t, v.ID)})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := decode[booking.CheckInResult](t, rec)
	require.False(t, res.Already)
	require.True(t, res.CheckedInAt.Equal(r.Clock.T))
	require.Equal(t, booking.StatusInService, res.Booking.Status)
	requireAt(t, r.Clock.T, r.timeCol(t, "in_service_at", v.ID), "in_service_at")
	requireAt(t, r.Clock.T, r.timeCol(t, "checked_in_at", v.ID), "checked_in_at")
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'confirmed' AND to_status = 'in_service'
		AND actor_kind = 'partner' AND actor_user_id = $2`, v.ID, r.RP.OwnerUserID))
	// The vehicle stays held; nothing is released.
	holds, until := r.rentalHold(t, v.ID)
	require.True(t, holds)
	require.Nil(t, until)
	require.Nil(t, r.releaseDueAt(t, v.ID))

	// Scanned again: Already, with the first time.
	r.Clock.T = vnAt(2026, 11, 7, 6, 30)
	again, err := r.handOver(t, v.ID)
	require.NoError(t, err)
	require.True(t, again.Already)
	require.True(t, again.CheckedInAt.Equal(vnAt(2026, 11, 7, 6, 0)))

	// R2: at return_at it is too late, one minute before it is not.
	r.Clock.T = rReturn
	_, err = r.handOver(t, r2.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "handover_outside_window")
	r.Clock.T = rReturn.Add(-time.Minute)
	_, err = r.handOver(t, r2.ID)
	require.NoError(t, err)
	require.Equal(t, booking.StatusInService, r.bookingStatus(t, r2.ID))
}

func TestRentalHandover_NotConfirmed409(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	_, err := r.handOver(t, v.ID)
	requireErr(t, err, http.StatusConflict, "checkin_not_allowed")
	require.Equal(t, booking.StatusAwaitingPayment, r.bookingStatus(t, v.ID))
	require.Nil(t, r.timeCol(t, "checked_in_at", v.ID))
}

// Review Focus 5: returned early, the vehicle is free at once for the rest
// of R's period; no refund, the partner is paid in full a day after the
// return.
func TestRentalReturned_EarlyFreesVehicle(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	other := r.customer(t, "khac@gmail.com")
	r.handOverAt(t, v.ID, rPickup)

	// 08/11 10:00; a request needs pickup ≥ now + 3h.
	returnedAt := vnAt(2026, 11, 8, 10, 0)
	r.Clock.T = returnedAt
	_, err := r.otherRequest(other, "before", vnAt(2026, 11, 8, 14, 0), rReturn)
	requireErr(t, err, http.StatusConflict, "vehicle_unavailable")

	tok := r.token(t, r.RP.OwnerUserID, auth.RolePartner)
	rec := r.do(t, http.MethodPost, "/v1/partner/bookings/"+v.ID.String()+"/return", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, booking.StatusCompleted, decode[booking.BookingView](t, rec).Status)
	requireAt(t, returnedAt, r.timeCol(t, "completed_at", v.ID), "completed_at")
	r.requireReleased(t, v.ID)
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'in_service' AND to_status = 'completed'
		AND actor_kind = 'partner' AND actor_user_id = $2`, v.ID, r.RP.OwnerUserID))
	requireAt(t, vnAt(2026, 11, 9, 10, 0), r.releaseDueAt(t, v.ID), "release_due_at")
	// The period itself is never shrunk.
	var ret time.Time
	require.NoError(t, r.Pool.QueryRow(context.Background(),
		`SELECT return_at FROM booking_rental_details WHERE booking_id = $1`, v.ID).Scan(&ret))
	require.True(t, ret.Equal(rReturn))

	next, err := r.otherRequest(other, "after", vnAt(2026, 11, 8, 14, 0), rReturn)
	require.NoError(t, err)
	require.Equal(t, booking.StatusRequested, next.Status)

	require.Empty(t, r.refunds(t))
	r.Clock.T = vnAt(2026, 11, 9, 10, 0)
	require.Equal(t, 1, r.releaseDue(t))
	r.requireRentalBalance(t, 0, rentalPartnerNet)
}

// Review Focus 5: R forgotten in_service past return_at does not block a
// rental starting after return_at.
func TestRentalReturned_LateDoesNotBlockNext(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	other := r.customer(t, "khac@gmail.com")
	r.handOverAt(t, v.ID, rPickup)

	r.Clock.T = vnAt(2026, 11, 9, 11, 0)
	next, err := r.otherRequest(other, "next", vnAt(2026, 11, 9, 14, 0), vnAt(2026, 11, 10, 14, 0))
	require.NoError(t, err)
	require.Equal(t, booking.StatusRequested, next.Status)
	require.Equal(t, booking.StatusInService, r.bookingStatus(t, v.ID))
	holds, _ := r.rentalHold(t, v.ID)
	require.True(t, holds)
}

func TestRentalReturned_Idempotent(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	r.handOverAt(t, v.ID, rPickup)
	returnedAt := vnAt(2026, 11, 8, 10, 0)
	r.Clock.T = returnedAt
	r.returned(t, v.ID)

	r.Clock.T = vnAt(2026, 11, 8, 11, 0)
	out := r.returned(t, v.ID)
	require.Equal(t, booking.StatusCompleted, out.Status)
	requireAt(t, returnedAt, r.timeCol(t, "completed_at", v.ID), "completed_at unchanged")
	requireAt(t, vnAt(2026, 11, 9, 10, 0), r.releaseDueAt(t, v.ID), "release_due_at unchanged")
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND to_status = 'completed'`, v.ID))
}

func TestRentalReturned_NotInService409(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	r.Clock.T = rPickup
	_, err := r.Svc.MarkRentalReturned(context.Background(), r.RP.ID, r.RP.OwnerUserID, v.ID)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, v.ID))

	tok := r.token(t, r.RP.OwnerUserID, auth.RolePartner)
	requireCode(t, r.do(t, http.MethodPost, "/v1/partner/bookings/nope/return", tok, nil), http.StatusNotFound, "not_found")
	requireCode(t, r.do(t, http.MethodPost, "/v1/partner/bookings/"+uuid.NewString()+"/return", tok, nil), http.StatusNotFound, "not_found")
}

func TestRentalNoShow(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	path := "/v1/partner/bookings/" + v.ID.String() + "/no-show"

	r.Clock.T = rPickup.Add(-time.Minute)
	tok := r.token(t, r.RP.OwnerUserID, auth.RolePartner) // valid 15 minutes from the clock
	requireCode(t, r.do(t, http.MethodPost, path, tok, nil), http.StatusConflict, "no_show_too_early")
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, v.ID))

	r.Clock.T = rPickup
	rec := r.do(t, http.MethodPost, path, tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, booking.StatusNoShow, decode[booking.BookingView](t, rec).Status)
	requireAt(t, rPickup, r.timeCol(t, "no_show_at", v.ID), "no_show_at")
	r.requireReleased(t, v.ID)
	requireAt(t, vnAt(2026, 11, 8, 8, 0), r.releaseDueAt(t, v.ID), "release_due_at")
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'confirmed' AND to_status = 'no_show'
		AND actor_kind = 'partner' AND actor_user_id = $2`, v.ID, r.RP.OwnerUserID))
	require.Empty(t, r.refunds(t))
	r.requireRentalBalance(t, rentalPartnerNet, 0)

	// Again: no longer confirmed.
	requireCode(t, r.do(t, http.MethodPost, path, tok, nil), http.StatusConflict, "invalid_booking_state")
	_, err := r.handOver(t, v.ID)
	requireErr(t, err, http.StatusConflict, "checkin_not_allowed")

	r.Clock.T = vnAt(2026, 11, 8, 8, 0)
	require.Equal(t, 1, r.releaseDue(t))
	r.requireRentalBalance(t, 0, rentalPartnerNet)
}

func TestRentalNoShow_OtherPartner404(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	ctx := context.Background()
	r.Clock.T = rPickup
	_, err := r.Svc.MarkRentalNoShow(ctx, r.P.ID, r.P.OwnerUserID, v.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = r.Svc.MarkRentalReturned(ctx, r.P.ID, r.P.OwnerUserID, v.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")
	other := r.token(t, r.P.OwnerUserID, auth.RolePartner)
	requireCode(t, r.do(t, http.MethodPost, "/v1/partner/bookings/"+v.ID.String()+"/no-show", other, nil), http.StatusNotFound, "not_found")
	requireCode(t, r.do(t, http.MethodPost, "/v1/partner/bookings/nope/no-show", other, nil), http.StatusNotFound, "not_found")
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, v.ID))

	// Not a rental: no partner no-show or return.
	r.Clock.T = testNow
	a := r.create(t, "attraction")
	r.pay(t, r.orderCode(t, a.ID), gross)
	r.Clock.T = onVisitDay
	_, err = r.Svc.MarkRentalNoShow(ctx, r.P.ID, r.P.OwnerUserID, a.ID)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	_, err = r.Svc.MarkRentalReturned(ctx, r.P.ID, r.P.OwnerUserID, a.ID)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
}

func TestReschedule_RentalRejected409(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	_, err := r.Svc.Reschedule(context.Background(), r.Customer, v.ID, "2026-11-08", uuid.New())
	requireErr(t, err, http.StatusConflict, "reschedule_not_allowed")
	mine, err := r.Svc.GetMine(context.Background(), r.Customer, v.ID)
	require.NoError(t, err)
	require.False(t, mine.CanReschedule)
	require.True(t, mine.CanCancel)
}

// The attraction and ride jobs leave rentals alone; FinishRentals leaves
// attractions alone.
func TestAttractionAndRideUnchanged(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	v2 := r.paidR2(t)
	r.Clock.T = testNow
	a := r.create(t, "a1")
	r.pay(t, r.orderCode(t, a.ID), gross)
	r.handOverAt(t, v2.ID, rPickup)

	r.Clock.T = vnAt(2026, 11, 11, 12, 0)
	require.Equal(t, 0, r.completeRidesAll(t))
	require.Equal(t, 1, r.markNoShows(t), "the attraction only")
	require.Equal(t, booking.StatusNoShow, r.bookingStatus(t, a.ID))
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, v.ID))
	require.Equal(t, booking.StatusInService, r.bookingStatus(t, v2.ID))

	// A confirmed attraction past its end is not FinishRentals' business.
	r.Clock.T = testNow
	a2 := r.create(t, "a2")
	r.pay(t, r.orderCode(t, a2.ID), gross)
	r.Clock.T = vnAt(2026, 11, 11, 12, 0)
	require.Equal(t, 2, r.finishRentals(t))
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, a2.ID))
	require.Equal(t, booking.StatusNoShow, r.bookingStatus(t, v.ID))
	require.Equal(t, booking.StatusCompleted, r.bookingStatus(t, v2.ID))
}

func (r *rentalEnv) completeRidesAll(t *testing.T) int {
	t.Helper()
	n, err := r.Svc.CompleteRides(context.Background())
	require.NoError(t, err)
	return n
}

func TestFinishRentals_AutoCompleteBoundary(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	r.handOverAt(t, v.ID, rPickup)

	r.Clock.T = vnAt(2026, 11, 10, 9, 59)
	require.Equal(t, 0, r.finishRentals(t))
	require.Equal(t, booking.StatusInService, r.bookingStatus(t, v.ID))

	at := vnAt(2026, 11, 10, 10, 0)
	r.Clock.T = at
	require.Equal(t, 1, r.finishRentals(t))
	require.Equal(t, booking.StatusCompleted, r.bookingStatus(t, v.ID))
	requireAt(t, at, r.timeCol(t, "completed_at", v.ID), "completed_at")
	r.requireReleased(t, v.ID)
	requireAt(t, vnAt(2026, 11, 11, 10, 0), r.releaseDueAt(t, v.ID), "release_due_at")
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'in_service' AND to_status = 'completed'
		AND actor_kind = 'system' AND actor_user_id IS NULL AND reason = $2`, v.ID, reasonAutoComplete))
	require.Equal(t, 0, r.finishRentals(t), "nothing left")

	r.Clock.T = vnAt(2026, 11, 11, 10, 0)
	require.Equal(t, 1, r.releaseDue(t))
	r.requireRentalBalance(t, 0, rentalPartnerNet)
	require.Empty(t, r.refunds(t))
}

func TestFinishRentals_AutoNoShowBoundary(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)

	r.Clock.T = rReturn.Add(-time.Minute)
	require.Equal(t, 0, r.finishRentals(t))
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, v.ID))

	r.Clock.T = rReturn
	require.Equal(t, 1, r.finishRentals(t))
	require.Equal(t, booking.StatusNoShow, r.bookingStatus(t, v.ID))
	requireAt(t, rReturn, r.timeCol(t, "no_show_at", v.ID), "no_show_at")
	r.requireReleased(t, v.ID)
	requireAt(t, vnAt(2026, 11, 10, 10, 0), r.releaseDueAt(t, v.ID), "release_due_at")
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'confirmed' AND to_status = 'no_show'
		AND actor_kind = 'system' AND reason = $2`, v.ID, reasonAutoNoShow))
	require.Empty(t, r.refunds(t))
	r.requireRentalBalance(t, rentalPartnerNet, 0)
}

// The partner's own clicks come first: the job changes nothing afterwards.
func TestFinishRentals_ManualWins(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	v2 := r.paidR2(t)
	r.handOverAt(t, v.ID, rPickup)
	_, err := r.Svc.MarkRentalNoShow(context.Background(), r.RP.ID, r.RP.OwnerUserID, v2.ID)
	require.NoError(t, err)
	returnedAt := vnAt(2026, 11, 8, 10, 0)
	r.Clock.T = returnedAt
	r.returned(t, v.ID)

	r.Clock.T = vnAt(2026, 11, 10, 10, 0)
	require.Equal(t, 0, r.finishRentals(t))
	requireAt(t, returnedAt, r.timeCol(t, "completed_at", v.ID), "completed_at")
	requireAt(t, vnAt(2026, 11, 9, 10, 0), r.releaseDueAt(t, v.ID), "release_due_at of R")
	requireAt(t, rPickup, r.timeCol(t, "no_show_at", v2.ID), "no_show_at")
	requireAt(t, vnAt(2026, 11, 8, 8, 0), r.releaseDueAt(t, v2.ID), "release_due_at of R2")
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM booking_status_history WHERE actor_kind = 'system'
		AND to_status IN ('completed', 'no_show')`))
}

// The partner clicks while the job has listed the booking but not locked it
// yet: the job re-checks under the lock and leaves it.
func TestFinishRentals_ManualWinsUnderLock(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	v2 := r.paidR2(t)
	r.handOverAt(t, v.ID, rPickup)

	r.Clock.T = vnAt(2026, 11, 10, 10, 0)
	ctx := context.Background()
	var once sync.Map
	r.Svc.SetJobHooks(0, func(id uuid.UUID) error {
		if _, done := once.LoadOrStore(id, true); done {
			return nil
		}
		var err error
		switch id {
		case v.ID:
			_, err = r.Svc.MarkRentalReturned(ctx, r.RP.ID, r.RP.OwnerUserID, id)
		case v2.ID:
			_, err = r.Svc.MarkRentalNoShow(ctx, r.RP.ID, r.RP.OwnerUserID, id)
		default:
			err = errors.New("unexpected booking")
		}
		return err
	})
	require.Equal(t, 0, r.finishRentals(t))
	require.Equal(t, booking.StatusCompleted, r.bookingStatus(t, v.ID))
	require.Equal(t, booking.StatusNoShow, r.bookingStatus(t, v2.ID))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM booking_status_history WHERE actor_kind = 'system'
		AND to_status IN ('completed', 'no_show')`))
	require.Equal(t, 2, r.count(t, `SELECT count(*) FROM booking_status_history WHERE actor_kind = 'partner'
		AND to_status IN ('completed', 'no_show')`))
}

// A failing booking does not stop the others and is skipped while it backs
// off, then picked up again.
func TestFinishRentals_BackoffSkip(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	v2 := r.paidR2(t)
	r.handOverAt(t, v2.ID, rPickup)

	// R is due for no-show (return_at), R2 for completion (return_at + 24h).
	run1 := vnAt(2026, 11, 10, 10, 0)
	r.Clock.T = run1
	tried := r.failFor(0, v.ID)
	require.Equal(t, 1, r.finishRentals(t), "R fails, R2 is completed")
	require.ElementsMatch(t, []uuid.UUID{v.ID, v2.ID}, *tried)
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, v.ID))
	require.Equal(t, booking.StatusCompleted, r.bookingStatus(t, v2.ID))

	*tried = nil
	require.Equal(t, 0, r.finishRentals(t), "same now: R is skipped")
	require.Empty(t, *tried)

	r.Svc.SetJobHooks(0, nil)
	r.Clock.T = run1.Add(30 * time.Second)
	require.Equal(t, 0, r.finishRentals(t), "still backing off")
	r.Clock.T = run1.Add(time.Minute)
	require.Equal(t, 1, r.finishRentals(t), "after its wait")
	require.Equal(t, booking.StatusNoShow, r.bookingStatus(t, v.ID))
	requireAt(t, run1.Add(time.Minute), r.timeCol(t, "no_show_at", v.ID), "no_show_at")
}
