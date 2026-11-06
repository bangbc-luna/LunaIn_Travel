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
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

// Times below are Vietnam time. R is the one-way charter request Xe 7 chỗ,
// airport -> Hoi An, pickup Wednesday 11/11 08:00, service_end_at 08:45
// (45 min drive), 563.600, sent, accepted and paid at 09:00 on 10/11
// (paidCR): confirmed, partner pending 507.240, commission 56.360. With
// charter_auto_complete_hours 24 the QR window is [11/11 06:00, 12/11
// 08:45) and FinishCharters ends it at 12/11 08:45.

var (
	crPickup     = wed0800
	crServiceEnd = vnAt(2026, 11, 11, 8, 45)
	crAutoFinish = vnAt(2026, 11, 12, 8, 45)
)

// paidCharter requests Xe 7 chỗ for customer with key at the current clock
// (one-way, 563.600 on a weekday morning), accepts and pays it.
func (c *charterEnv) paidCharter(t *testing.T, customer uuid.UUID, key string, pickup time.Time) booking.BookingView {
	t.Helper()
	v := c.createCharter(t, customer, key, c.charterInput(c.C7.ID, pickup, false, nil, charterGross))
	c.acceptC(t, v.ID)
	c.pay(t, c.orderCode(t, v.ID), charterGross)
	require.Equal(t, booking.StatusConfirmed, c.bookingStatus(t, v.ID))
	return v
}

// paidCR is R, paid at 09:00 on 10/11.
func (c *charterEnv) paidCR(t *testing.T) booking.BookingView {
	t.Helper()
	v := c.paidCharter(t, c.Customer, "r", crPickup)
	c.requireCharterBalance(t, charterPartnerNet)
	return v
}

// paidCR2 is another customer's request with R's pickup, paid at the
// current clock.
func (c *charterEnv) paidCR2(t *testing.T) booking.BookingView {
	t.Helper()
	return c.paidCharter(t, c.customer(t, "khach2@gmail.com"), "r2", crPickup)
}

// pickUp scans the booking's QR as the charter partner's owner at at.
func (c *charterEnv) pickUp(t *testing.T, id uuid.UUID, at time.Time) (booking.CheckInResult, error) {
	t.Helper()
	c.Clock.T = at
	return c.Svc.CheckIn(context.Background(), c.CP.ID, c.CP.OwnerUserID, c.qrToken(t, id))
}

func (c *charterEnv) pickedUp(t *testing.T, id uuid.UUID, at time.Time) {
	t.Helper()
	res, err := c.pickUp(t, id, at)
	require.NoError(t, err)
	require.False(t, res.Already)
	require.Equal(t, booking.StatusInService, res.Booking.Status)
}

func (c *charterEnv) completeC(t *testing.T, id uuid.UUID) booking.BookingView {
	t.Helper()
	v, err := c.Svc.CompleteCharter(context.Background(), c.CP.ID, c.CP.OwnerUserID, id)
	require.NoError(t, err)
	return v
}

func (c *charterEnv) finishCharters(t *testing.T) int {
	t.Helper()
	n, err := c.Svc.FinishCharters(context.Background())
	require.NoError(t, err)
	return n
}

func (c *charterEnv) requireCharterBalances(t *testing.T, pending, available int64) {
	t.Helper()
	b, err := c.Ledger.Balance(context.Background(), c.CP.ID)
	require.NoError(t, err)
	require.Equal(t, ledger.Balance{Pending: pending, Available: available}, b)
	require.NoError(t, c.Ledger.Verify(context.Background(), c.CP.ID))
}

func (c *charterEnv) systemEnds(t *testing.T) int {
	t.Helper()
	return c.count(t, `SELECT count(*) FROM booking_status_history WHERE actor_kind = 'system'
		AND to_status IN ('completed', 'no_show')`)
}

func TestCharterPickup_Window(t *testing.T) {
	c := newCharterEnv(t)
	v := c.paidCR(t)
	x := c.paidCR2(t)

	_, err := c.pickUp(t, v.ID, vnAt(2026, 11, 11, 5, 59))
	requireErr(t, err, http.StatusUnprocessableEntity, "pickup_outside_window")
	require.Equal(t, booking.StatusConfirmed, c.bookingStatus(t, v.ID))

	at := vnAt(2026, 11, 11, 6, 0)
	c.pickedUp(t, v.ID, at)
	requireAt(t, at, c.timeCol(t, "in_service_at", v.ID), "in_service_at")
	requireAt(t, at, c.timeCol(t, "checked_in_at", v.ID), "checked_in_at")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'confirmed' AND to_status = 'in_service'
		AND actor_kind = 'partner' AND actor_user_id = $2`, v.ID, c.CP.OwnerUserID))
	require.Nil(t, c.releaseDueAt(t, v.ID), "nothing is released at pickup")

	res, err := c.pickUp(t, v.ID, vnAt(2026, 11, 11, 7, 0))
	require.NoError(t, err)
	require.True(t, res.Already)
	require.True(t, at.Equal(res.CheckedInAt))

	// The other booking, never scanned: up to service_end_at + 24h excluded.
	_, err = c.pickUp(t, x.ID, crAutoFinish)
	requireErr(t, err, http.StatusUnprocessableEntity, "pickup_outside_window")
	require.Equal(t, booking.StatusConfirmed, c.bookingStatus(t, x.ID))
	c.pickedUp(t, x.ID, crAutoFinish.Add(-time.Minute))

	// Another partner's QR is unknown.
	_, err = c.Svc.CheckIn(context.Background(), c.RP.ID, c.RP.OwnerUserID, c.qrToken(t, v.ID))
	requireErr(t, err, http.StatusNotFound, "not_found")
	c.requireCharterBalances(t, 2*charterPartnerNet, 0)
}

// The window follows charter_auto_complete_hours.
func TestCharterPickup_WindowFollowsSetting(t *testing.T) {
	c := newCharterEnv(t)
	v := c.paidCR(t)
	c.exec(t, `UPDATE platform_settings SET charter_auto_complete_hours = 48`)
	c.pickedUp(t, v.ID, vnAt(2026, 11, 13, 8, 44))
}

func TestCharterPickup_NotConfirmed409(t *testing.T) {
	c := newCharterEnv(t)
	v, _ := c.acceptedCR(t)
	require.Equal(t, booking.StatusAwaitingPayment, c.bookingStatus(t, v.ID))
	_, err := c.pickUp(t, v.ID, vnAt(2026, 11, 11, 7, 0))
	requireErr(t, err, http.StatusConflict, "checkin_not_allowed")
	require.Equal(t, booking.StatusAwaitingPayment, c.bookingStatus(t, v.ID))
}

func TestCompleteCharter(t *testing.T) {
	c := newCharterEnv(t)
	ctx := context.Background()
	v := c.paidCR(t)
	x := c.paidCR2(t)
	// A paid rental of the rental partner, picked up 12/11 08:00.
	rv := c.paidRental(t, "rental", c.rentalInput(c.Vios.ID, c.P1.ID, vnAt(2026, 11, 12, 8, 0), vnAt(2026, 11, 13, 8, 0)))
	c.pickedUp(t, v.ID, vnAt(2026, 11, 11, 7, 50))

	doneAt := vnAt(2026, 11, 11, 8, 30)
	c.Clock.T = doneAt
	path := "/v1/partner/bookings/" + v.ID.String() + "/complete"
	tok := c.token(t, c.CP.OwnerUserID, auth.RolePartner)
	rec := c.do(t, http.MethodPost, path, tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, booking.StatusCompleted, decode[booking.BookingView](t, rec).Status)
	requireAt(t, doneAt, c.timeCol(t, "completed_at", v.ID), "completed_at")
	requireAt(t, doneAt.Add(24*time.Hour), c.releaseDueAt(t, v.ID), "release_due_at")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'in_service' AND to_status = 'completed'
		AND actor_kind = 'partner' AND actor_user_id = $2`, v.ID, c.CP.OwnerUserID))

	// Again, later: 200 unchanged.
	c.Clock.T = doneAt.Add(time.Hour)
	again := c.completeC(t, v.ID)
	require.Equal(t, booking.StatusCompleted, again.Status)
	requireAt(t, doneAt, c.timeCol(t, "completed_at", v.ID), "completed_at")
	requireAt(t, doneAt.Add(24*time.Hour), c.releaseDueAt(t, v.ID), "release_due_at")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND to_status = 'completed'`, v.ID))

	// Not picked up yet, a rental, another partner, a malformed id.
	_, err := c.Svc.CompleteCharter(ctx, c.CP.ID, c.CP.OwnerUserID, x.ID)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	require.Equal(t, booking.StatusConfirmed, c.bookingStatus(t, x.ID))
	_, err = c.Svc.CompleteCharter(ctx, c.RP.ID, c.RP.OwnerUserID, rv.ID)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	require.Equal(t, booking.StatusConfirmed, c.bookingStatus(t, rv.ID))
	_, err = c.Svc.CompleteCharter(ctx, c.RP.ID, c.RP.OwnerUserID, v.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")
	tok = c.token(t, c.CP.OwnerUserID, auth.RolePartner) // valid 15 minutes from the clock
	other := c.token(t, c.RP.OwnerUserID, auth.RolePartner)
	requireCode(t, c.do(t, http.MethodPost, path, other, nil), http.StatusNotFound, "not_found")
	requireCode(t, c.do(t, http.MethodPost, "/v1/partner/bookings/nope/complete", tok, nil), http.StatusNotFound, "not_found")
	requireCode(t, c.do(t, http.MethodPost, "/v1/partner/bookings/"+uuid.New().String()+"/complete", tok, nil), http.StatusNotFound, "not_found")

	c.requireCharterBalances(t, 2*charterPartnerNet, 0)
	c.Clock.T = doneAt.Add(24 * time.Hour)
	require.Equal(t, 1, c.releaseDue(t))
	c.requireCharterBalances(t, charterPartnerNet, charterPartnerNet)
	require.Empty(t, c.refunds(t))
}

func TestCharterNoShow(t *testing.T) {
	c := newCharterEnv(t)
	v := c.paidCR(t)
	x := c.paidCR2(t)
	path := "/v1/partner/bookings/" + v.ID.String() + "/no-show"

	c.Clock.T = crPickup.Add(-time.Minute)
	tok := c.token(t, c.CP.OwnerUserID, auth.RolePartner)
	rec := c.do(t, http.MethodPost, path, tok, nil)
	requireCode(t, rec, http.StatusConflict, "no_show_too_early")
	require.Contains(t, rec.Body.String(), "Chỉ đánh dấu khách không đến sau giờ đón/nhận xe")
	require.Equal(t, booking.StatusConfirmed, c.bookingStatus(t, v.ID))

	c.Clock.T = crPickup
	rec = c.do(t, http.MethodPost, path, tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, booking.StatusNoShow, decode[booking.BookingView](t, rec).Status)
	requireAt(t, crPickup, c.timeCol(t, "no_show_at", v.ID), "no_show_at")
	requireAt(t, crPickup.Add(24*time.Hour), c.releaseDueAt(t, v.ID), "release_due_at")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'confirmed' AND to_status = 'no_show'
		AND actor_kind = 'partner' AND actor_user_id = $2`, v.ID, c.CP.OwnerUserID))
	require.Empty(t, c.refunds(t))
	c.requireCharterBalance(t, 2*charterPartnerNet)

	// Again: no longer confirmed; no pickup either.
	requireCode(t, c.do(t, http.MethodPost, path, tok, nil), http.StatusConflict, "invalid_booking_state")
	_, err := c.pickUp(t, v.ID, crPickup)
	requireErr(t, err, http.StatusConflict, "checkin_not_allowed")

	// Picked up: no no-show; another partner: 404.
	c.pickedUp(t, x.ID, crPickup)
	_, err = c.Svc.MarkNoShow(context.Background(), c.CP.ID, c.CP.OwnerUserID, x.ID)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	_, err = c.Svc.MarkNoShow(context.Background(), c.RP.ID, c.RP.OwnerUserID, x.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")

	c.Clock.T = crPickup.Add(24 * time.Hour)
	require.Equal(t, 1, c.releaseDue(t))
	c.requireCharterBalances(t, charterPartnerNet, charterPartnerNet)
}

// MarkNoShow of another service type (an attraction ticket) is 409.
func TestMarkNoShow_OtherServiceType409(t *testing.T) {
	r := newRentalEnv(t)
	r.Clock.T = testNow
	a := r.create(t, "attraction")
	r.pay(t, r.orderCode(t, a.ID), gross)
	r.Clock.T = onVisitDay
	_, err := r.Svc.MarkNoShow(context.Background(), r.P.ID, r.P.OwnerUserID, a.ID)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, a.ID))
}

func TestFinishCharters_Boundary(t *testing.T) {
	c := newCharterEnv(t)
	v := c.paidCR(t)
	x := c.paidCR2(t)
	c.pickedUp(t, v.ID, vnAt(2026, 11, 11, 7, 55))

	c.Clock.T = crAutoFinish.Add(-time.Minute)
	require.Equal(t, 0, c.finishCharters(t))
	require.Equal(t, booking.StatusInService, c.bookingStatus(t, v.ID))
	require.Equal(t, booking.StatusConfirmed, c.bookingStatus(t, x.ID))

	c.Clock.T = crAutoFinish
	require.Equal(t, 2, c.finishCharters(t))
	require.Equal(t, booking.StatusCompleted, c.bookingStatus(t, v.ID))
	require.Equal(t, booking.StatusNoShow, c.bookingStatus(t, x.ID))
	requireAt(t, crAutoFinish, c.timeCol(t, "completed_at", v.ID), "completed_at")
	requireAt(t, crAutoFinish, c.timeCol(t, "no_show_at", x.ID), "no_show_at")
	requireAt(t, vnAt(2026, 11, 13, 8, 45), c.releaseDueAt(t, v.ID), "release_due_at")
	requireAt(t, vnAt(2026, 11, 13, 8, 45), c.releaseDueAt(t, x.ID), "release_due_at")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'in_service' AND to_status = 'completed'
		AND actor_kind = 'system' AND actor_user_id IS NULL AND reason = $2`,
		v.ID, "tự hoàn thành sau 24 giờ quá giờ kết thúc chuyến"))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'confirmed' AND to_status = 'no_show'
		AND actor_kind = 'system' AND actor_user_id IS NULL AND reason = $2`,
		x.ID, "nhà xe không xác nhận đón khách trước khi chuyến tự kết thúc"))
	require.Empty(t, c.refunds(t))

	// Nothing left to do.
	c.Clock.T = crAutoFinish.Add(time.Hour)
	require.Equal(t, 0, c.finishCharters(t))

	c.Clock.T = vnAt(2026, 11, 13, 8, 45)
	require.Equal(t, 2, c.releaseDue(t))
	c.requireCharterBalances(t, 0, 2*charterPartnerNet)
}

func TestFinishCharters_BoundaryFollowsSetting(t *testing.T) {
	c := newCharterEnv(t)
	v := c.paidCR(t)
	x := c.paidCR2(t)
	c.pickedUp(t, v.ID, vnAt(2026, 11, 11, 7, 55))
	c.exec(t, `UPDATE platform_settings SET charter_auto_complete_hours = 48`)

	c.Clock.T = crAutoFinish
	require.Equal(t, 0, c.finishCharters(t))
	c.Clock.T = vnAt(2026, 11, 13, 8, 44)
	require.Equal(t, 0, c.finishCharters(t))
	require.Equal(t, booking.StatusInService, c.bookingStatus(t, v.ID))
	require.Equal(t, booking.StatusConfirmed, c.bookingStatus(t, x.ID))

	end := vnAt(2026, 11, 13, 8, 45)
	c.Clock.T = end
	require.Equal(t, 2, c.finishCharters(t))
	require.Equal(t, booking.StatusCompleted, c.bookingStatus(t, v.ID))
	require.Equal(t, booking.StatusNoShow, c.bookingStatus(t, x.ID))
	requireAt(t, end.Add(24*time.Hour), c.releaseDueAt(t, v.ID), "release_due_at")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND to_status = 'completed' AND reason = $2`,
		v.ID, "tự hoàn thành sau 48 giờ quá giờ kết thúc chuyến"))
}

// The partner's own clicks come first: the job changes nothing afterwards.
func TestFinishCharters_ManualWins(t *testing.T) {
	c := newCharterEnv(t)
	v := c.paidCR(t)
	x := c.paidCR2(t)
	c.pickedUp(t, v.ID, vnAt(2026, 11, 11, 7, 55))
	doneAt := vnAt(2026, 11, 11, 9, 0)
	c.Clock.T = doneAt
	c.completeC(t, v.ID)
	_, err := c.Svc.MarkNoShow(context.Background(), c.CP.ID, c.CP.OwnerUserID, x.ID)
	require.NoError(t, err)

	c.Clock.T = crAutoFinish.Add(time.Hour)
	require.Equal(t, 0, c.finishCharters(t))
	requireAt(t, doneAt, c.timeCol(t, "completed_at", v.ID), "completed_at")
	requireAt(t, doneAt.Add(24*time.Hour), c.releaseDueAt(t, v.ID), "release_due_at")
	requireAt(t, doneAt, c.timeCol(t, "no_show_at", x.ID), "no_show_at")
	require.Equal(t, 0, c.systemEnds(t))
}

// The partner clicks while the job has listed the booking but not locked it
// yet: the job re-checks under the lock and leaves it.
func TestFinishCharters_ManualWinsUnderLock(t *testing.T) {
	c := newCharterEnv(t)
	v := c.paidCR(t)
	x := c.paidCR2(t)
	c.pickedUp(t, v.ID, vnAt(2026, 11, 11, 7, 55))

	c.Clock.T = crAutoFinish
	ctx := context.Background()
	var once sync.Map
	c.Svc.SetJobHooks(0, func(id uuid.UUID) error {
		if _, done := once.LoadOrStore(id, true); done {
			return nil
		}
		var err error
		switch id {
		case v.ID:
			_, err = c.Svc.CompleteCharter(ctx, c.CP.ID, c.CP.OwnerUserID, id)
		case x.ID:
			_, err = c.Svc.MarkNoShow(ctx, c.CP.ID, c.CP.OwnerUserID, id)
		default:
			err = errors.New("unexpected booking")
		}
		return err
	})
	require.Equal(t, 0, c.finishCharters(t))
	require.Equal(t, booking.StatusCompleted, c.bookingStatus(t, v.ID))
	require.Equal(t, booking.StatusNoShow, c.bookingStatus(t, x.ID))
	require.Equal(t, 0, c.systemEnds(t))
	require.Equal(t, 2, c.count(t, `SELECT count(*) FROM booking_status_history WHERE actor_kind = 'partner'
		AND to_status IN ('completed', 'no_show')`))
	require.Equal(t, 2, c.count(t, `SELECT count(*) FROM booking_settlements WHERE release_due_at IS NOT NULL`))
}

// A failing booking does not stop the others and is skipped while it backs
// off, then picked up again.
func TestFinishCharters_BackoffSkip(t *testing.T) {
	c := newCharterEnv(t)
	v := c.paidCR(t)
	x := c.paidCR2(t)
	c.pickedUp(t, x.ID, vnAt(2026, 11, 11, 7, 55))

	run1 := crAutoFinish.Add(15 * time.Minute)
	c.Clock.T = run1
	tried := c.failFor(0, v.ID)
	require.Equal(t, 1, c.finishCharters(t), "R fails, R2 is completed")
	require.ElementsMatch(t, []uuid.UUID{v.ID, x.ID}, *tried)
	require.Equal(t, booking.StatusConfirmed, c.bookingStatus(t, v.ID))
	require.Equal(t, booking.StatusCompleted, c.bookingStatus(t, x.ID))

	*tried = nil
	require.Equal(t, 0, c.finishCharters(t), "same now: R is skipped")
	require.Empty(t, *tried)

	c.Svc.SetJobHooks(0, nil)
	c.Clock.T = run1.Add(30 * time.Second)
	require.Equal(t, 0, c.finishCharters(t), "still backing off")
	c.Clock.T = run1.Add(time.Minute)
	require.Equal(t, 1, c.finishCharters(t), "after its wait")
	require.Equal(t, booking.StatusNoShow, c.bookingStatus(t, v.ID))
	requireAt(t, run1.Add(time.Minute), c.timeCol(t, "no_show_at", v.ID), "no_show_at")
}

// FinishRentals leaves charters alone, even long past their end.
func TestFinishRentals_IgnoresCharters(t *testing.T) {
	c := newCharterEnv(t)
	v := c.paidCR(t)
	x := c.paidCR2(t)
	c.pickedUp(t, x.ID, vnAt(2026, 11, 11, 7, 55))

	c.Clock.T = vnAt(2026, 11, 20, 9, 0)
	require.Equal(t, 0, c.finishRentals(t))
	require.Equal(t, booking.StatusConfirmed, c.bookingStatus(t, v.ID))
	require.Equal(t, booking.StatusInService, c.bookingStatus(t, x.ID))
	require.Equal(t, 2, c.finishCharters(t))
}

// FinishCharters leaves rentals alone, even long past their end.
func TestFinishCharters_IgnoresRentals(t *testing.T) {
	r := newRentalEnv(t)
	v := r.paidR(t)
	v2 := r.paidR2(t)
	r.handOverAt(t, v2.ID, rPickup)

	r.Clock.T = vnAt(2026, 11, 20, 9, 0)
	n, err := r.Svc.FinishCharters(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, v.ID))
	require.Equal(t, booking.StatusInService, r.bookingStatus(t, v2.ID))
	require.Equal(t, 2, r.finishRentals(t))
}

// Friday 13/11 08:00 pickup (563.600, no surcharge), cancelled by the
// customer at 20:00 on 11/11, 36 h before: tier 24 h..48 h, 50%.
func TestCancelCharter_CustomerPolicy(t *testing.T) {
	c := newCharterEnv(t)
	v := c.paidCharter(t, c.Customer, "fri", vnAt(2026, 11, 13, 8, 0))
	c.requireCharterBalance(t, charterPartnerNet)

	c.Clock.T = vnAt(2026, 11, 11, 20, 0)
	acc := payment.BankAccount{BankBIN: "970436", AccountNumber: "0011001234567", AccountHolder: "TRAN THI BINH"}
	out, err := c.Svc.CancelByCustomer(context.Background(), c.Customer, v.ID, &acc)
	require.NoError(t, err)
	require.Equal(t, booking.StatusCancelled, out.Status)
	require.False(t, out.CanCancel)

	c.requireCancelled(t, v.ID, "customer", 281_800, payment.RefundCustomerCancel)
	require.Equal(t, payment.RefundPending, c.refunds(t)[0].Status)
	// 563.600 − 56.360 − 281.800, released at once.
	c.requireCharterBalances(t, 0, 225_440)
	require.Equal(t, charterCommission, c.platformNet(t, v.ID), "the commission is kept")
}

func TestCancelCharter_AdminForceMajeure(t *testing.T) {
	c := newCharterEnv(t)
	v := c.paidCharter(t, c.Customer, "fri", vnAt(2026, 11, 13, 8, 0))
	c.Clock.T = vnAt(2026, 11, 11, 20, 0)
	out, err := c.Svc.CancelByAdmin(context.Background(), c.Admin, v.ID, "force_majeure", "Bão đổ bộ Đà Nẵng")
	require.NoError(t, err)
	require.Equal(t, booking.StatusCancelled, out.Status)

	c.requireCancelled(t, v.ID, "admin_force_majeure", charterGross, payment.RefundAdminCancel)
	c.requireCharterBalances(t, 0, 0)
	require.Equal(t, int64(0), c.platformNet(t, v.ID), "the commission is waived")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM admin_audit_log
		WHERE action = 'booking.cancel' AND target_id = $1 AND actor_user_id = $2 AND reason = $3`,
		v.ID, c.Admin, "Bão đổ bộ Đà Nẵng"))
}

// Picked up, completed or no-show: nobody cancels any more.
func TestCancelCharter_InService409(t *testing.T) {
	c := newCharterEnv(t)
	ctx := context.Background()
	v := c.paidCR(t)
	x := c.paidCR2(t)
	c.pickedUp(t, v.ID, vnAt(2026, 11, 11, 7, 55))

	_, err := c.Svc.CancelByCustomer(ctx, c.Customer, v.ID, nil)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	_, err = c.Svc.CancelByAdmin(ctx, c.Admin, v.ID, "force_majeure", "Thử hủy")
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	mine, err := c.Svc.GetMine(ctx, c.Customer, v.ID)
	require.NoError(t, err)
	require.False(t, mine.CanCancel)

	c.completeC(t, v.ID)
	_, err = c.Svc.CancelByCustomer(ctx, c.Customer, v.ID, nil)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")

	c.Clock.T = crPickup
	_, err = c.Svc.MarkNoShow(ctx, c.CP.ID, c.CP.OwnerUserID, x.ID)
	require.NoError(t, err)
	_, err = c.Svc.CancelByAdmin(ctx, c.Admin, x.ID, "force_majeure", "Thử hủy")
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	require.Empty(t, c.refunds(t))
	c.requireCharterBalance(t, 2*charterPartnerNet)
}

func TestReschedule_CharterRejected409(t *testing.T) {
	c := newCharterEnv(t)
	v := c.paidCR(t)
	_, err := c.Svc.Reschedule(context.Background(), c.Customer, v.ID, "2026-11-12", uuid.New())
	requireErr(t, err, http.StatusConflict, "reschedule_not_allowed")
	mine, err := c.Svc.GetMine(context.Background(), c.Customer, v.ID)
	require.NoError(t, err)
	require.False(t, mine.CanReschedule)
	require.True(t, mine.CanCancel)
}
