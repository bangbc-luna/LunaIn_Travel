package booking_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

// The cancellation fixture: 4 adult + 5 child tickets = 1.000.000 at
// 1000 bps, i.e. commission 100.000 and partner 900.000 (spec §6.3 example).
const (
	bigGross      = int64(1_000_000)
	bigCommission = int64(100_000)
	// testNow + 71h (refund 100%), + 47h (50%), + 23h (0%).
	dateOver48h = "2026-11-08"
	date47h     = "2026-11-07"
	dateUnder24 = "2026-11-06"
)

// bigBooking is a really paid (confirmed) 1.000.000 booking on visit 08:00.
func (e *env) bigBooking(t *testing.T, visit string) booking.BookingView {
	t.Helper()
	in := e.input()
	in.VisitDate = visit
	in.Items = []booking.TicketLine{{TicketTypeID: e.Adult.ID, Quantity: 4}, {TicketTypeID: e.Child.ID, Quantity: 5}}
	v, err := e.Svc.CreateAttraction(context.Background(), e.Customer, "big-"+visit, in)
	require.NoError(t, err)
	require.Equal(t, bigGross, v.GrossAmount)
	e.pay(t, e.orderCode(t, v.ID), bigGross)
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, v.ID))
	e.requireBalance(t, bigGross-bigCommission, 0)
	return v
}

// bookingPending is what the booking still holds in the partner's pending
// bucket (sum of its pending entries).
func (e *env) bookingPending(t *testing.T, id uuid.UUID) int64 {
	t.Helper()
	var n int64
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(amount), 0)::bigint FROM ledger_entries WHERE booking_id = $1 AND bucket = 'pending'`, id).Scan(&n))
	return n
}

func (e *env) platformNet(t *testing.T, id uuid.UUID) int64 {
	t.Helper()
	var n int64
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(amount), 0)::bigint FROM platform_commission_entries WHERE booking_id = $1`, id).Scan(&n))
	return n
}

// requireCancelled checks the booking row, its history and its refund.
func (e *env) requireCancelled(t *testing.T, id uuid.UUID, kind string, refund int64, reason string) {
	t.Helper()
	require.Equal(t, booking.StatusCancelled, e.bookingStatus(t, id))
	require.Equal(t, kind, e.str(t, `SELECT cancel_kind FROM bookings WHERE id = $1`, id))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'confirmed' AND to_status = 'cancelled'`, id))
	require.Equal(t, int64(0), e.bookingPending(t, id), "the booking's pending is back to 0")
	require.NotNil(t, e.str(t, `SELECT released_at::text FROM booking_settlements WHERE booking_id = $1`, id))
	var bookingRefunds []refundRow
	for _, r := range e.refunds(t) {
		if r.BookingID != nil && *r.BookingID == id {
			bookingRefunds = append(bookingRefunds, r)
		}
	}
	if refund == 0 {
		require.Empty(t, bookingRefunds)
		return
	}
	require.Len(t, bookingRefunds, 1)
	require.Equal(t, refund, bookingRefunds[0].Amount)
	require.Equal(t, reason, bookingRefunds[0].Reason)
}

func (e *env) cancel(t *testing.T, id uuid.UUID) booking.BookingView {
	t.Helper()
	v, err := e.Svc.CancelByCustomer(context.Background(), e.Customer, id, nil)
	require.NoError(t, err)
	return v
}

func TestCancel_Over48hFullRefund(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, dateOver48h)
	before := e.emails(t)

	out := e.cancel(t, v.ID)
	require.Equal(t, booking.StatusCancelled, out.Status)
	require.False(t, out.CanCancel)
	require.False(t, out.CanReschedule)
	require.NotNil(t, out.CancelledAt)

	e.requireCancelled(t, v.ID, "customer", bigGross, payment.RefundCustomerCancel)
	require.Equal(t, payment.RefundAwaitingBankInfo, e.refunds(t)[0].Status)
	// 1.000.000 − 100.000 − 1.000.000: the partner is −100.000, released at once.
	e.requireBalance(t, 0, -bigCommission)
	require.Equal(t, bigCommission, e.platformNet(t, v.ID), "the commission is kept")
	require.Equal(t, before+1, e.emails(t))
}

func TestCancel_Between24and48HalfRefund(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, date47h)
	e.cancel(t, v.ID)
	e.requireCancelled(t, v.ID, "customer", 500_000, payment.RefundCustomerCancel)
	e.requireBalance(t, 0, 400_000)
	require.Equal(t, bigCommission, e.platformNet(t, v.ID))
}

func TestCancel_Under24hNoRefund(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, dateUnder24)
	e.cancel(t, v.ID)
	e.requireCancelled(t, v.ID, "customer", 0, "")
	e.requireBalance(t, 0, 900_000)
}

// Review Focus 3: moving the visit far away does not raise the refund.
func TestCancel_AfterRescheduleUsesOriginalStart(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, date47h)
	_, err := e.Svc.Reschedule(context.Background(), e.Customer, v.ID, "2026-11-20", e.Slot.ID)
	require.NoError(t, err)

	e.cancel(t, v.ID)
	e.requireCancelled(t, v.ID, "customer", 500_000, payment.RefundCustomerCancel)
	e.requireBalance(t, 0, 400_000)
}

func TestCancelByAdmin_ForceMajeureWaivesCommission(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, date47h)

	out, err := e.Svc.CancelByAdmin(context.Background(), e.Admin, v.ID, "force_majeure", "Khu du lịch đóng cửa đột xuất")
	require.NoError(t, err)
	require.Equal(t, booking.StatusCancelled, out.Status)

	e.requireCancelled(t, v.ID, "admin_force_majeure", bigGross, payment.RefundAdminCancel)
	e.requireBalance(t, 0, 0)
	require.Equal(t, int64(0), e.platformNet(t, v.ID), "the commission is waived")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM admin_audit_log
		WHERE action = 'booking.cancel' AND target_id = $1 AND actor_user_id = $2 AND reason = $3`,
		v.ID, e.Admin, "Khu du lịch đóng cửa đột xuất"))
	require.Equal(t, e.Admin.String(), e.str(t, `SELECT cancelled_by::text FROM bookings WHERE id = $1`, v.ID))
}

func TestCancelByAdmin_PolicyMode(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, date47h)

	// Through the admin route.
	tok := e.token(t, e.Admin, auth.RoleSuperAdmin)
	path := "/v1/admin/bookings/" + v.ID.String() + "/cancel"
	requireCode(t, e.do(t, "POST", path, tok, map[string]any{"mode": "oops", "reason": "x"}), http.StatusUnprocessableEntity, "invalid_cancel")
	requireCode(t, e.do(t, "POST", path, tok, map[string]any{"mode": "policy", "reason": " "}), http.StatusUnprocessableEntity, "invalid_cancel")
	requireCode(t, e.do(t, "POST", path, tok, map[string]any{"mode": "policy", "reason": "x", "extra": 1}), http.StatusBadRequest, "invalid_request")
	rec := e.do(t, "POST", path, tok, map[string]any{"mode": "policy", "reason": "Đối tác báo hủy"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, booking.StatusCancelled, decode[booking.AdminBookingView](t, rec).Status)

	e.requireCancelled(t, v.ID, "admin_policy", 500_000, payment.RefundAdminCancel)
	e.requireBalance(t, 0, 400_000)
	require.Equal(t, bigCommission, e.platformNet(t, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM admin_audit_log WHERE action = 'booking.cancel' AND target_id = $1`, v.ID))

	// Only confirmed bookings.
	requireCode(t, e.do(t, "POST", path, tok, map[string]any{"mode": "policy", "reason": "lần 2"}), http.StatusConflict, "invalid_booking_state")
	requireCode(t, e.do(t, "POST", "/v1/admin/bookings/"+uuid.NewString()+"/cancel", tok,
		map[string]any{"mode": "policy", "reason": "x"}), http.StatusNotFound, "not_found")
}

// Review Focus 4: another customer's booking is 404 for every action.
func TestCancel_OtherCustomer404(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, dateOver48h)
	other := e.customer(t, "khac@gmail.com")

	_, err := e.Svc.CancelByCustomer(context.Background(), other, v.ID, nil)
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.Reschedule(context.Background(), other, v.ID, "2026-11-20", e.Slot.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")

	tok := e.token(t, other, auth.RoleCustomer)
	base := "/v1/bookings/" + v.ID.String()
	requireCode(t, e.do(t, "POST", base+"/cancel", tok, map[string]any{}), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "POST", base+"/reschedule", tok,
		map[string]any{"visit_date": "2026-11-20", "time_slot_id": e.Slot.ID}), http.StatusNotFound, "not_found")
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, v.ID))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM booking_reschedules`))
}

func TestCancel_NotConfirmed409(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	_, err := e.Svc.CancelByCustomer(context.Background(), e.Customer, v.ID, nil)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	_, err = e.Svc.CancelByAdmin(context.Background(), e.Admin, v.ID, "force_majeure", "x")
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	require.Equal(t, booking.StatusPendingPayment, e.bookingStatus(t, v.ID))
	got, err := e.Svc.GetMine(context.Background(), e.Customer, v.ID)
	require.NoError(t, err)
	require.False(t, got.CanCancel)
	require.False(t, got.CanReschedule)
}

func TestCancel_WithBankAccountPendingAndSaved(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, dateOver48h)
	tok := e.token(t, e.Customer, auth.RoleCustomer)
	path := "/v1/bookings/" + v.ID.String() + "/cancel"

	requireCode(t, e.do(t, "GET", "/v1/customer/refund-account", tok, nil), http.StatusNotFound, "not_found")
	bad := map[string]any{"bank_account": map[string]any{"bank_bin": "97043", "account_number": "0011001234567", "account_holder": "An"}}
	requireCode(t, e.do(t, "POST", path, tok, bad), http.StatusUnprocessableEntity, "invalid_bank_account")
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, v.ID))

	rec := e.do(t, "POST", path, tok, map[string]any{"bank_account": map[string]any{
		"bank_bin": "970436", "account_number": "0011001234567", "account_holder": "Nguyễn Văn An",
	}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	out := decode[booking.BookingView](t, rec)
	require.Equal(t, booking.StatusCancelled, out.Status)
	require.Nil(t, out.QRToken)

	rs := e.refunds(t)
	require.Len(t, rs, 1)
	require.Equal(t, payment.RefundPending, rs[0].Status)
	require.Equal(t, "970436", *rs[0].BankBin)
	require.Equal(t, "0011001234567", *rs[0].Account)
	require.Equal(t, "NGUYEN VAN AN", *rs[0].Holder)

	// Saved as the default refund account.
	rec = e.do(t, "GET", "/v1/customer/refund-account", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, payment.BankAccount{BankBIN: "970436", AccountNumber: "0011001234567", AccountHolder: "NGUYEN VAN AN"},
		decode[payment.BankAccount](t, rec))
	rec = e.do(t, "PUT", "/v1/customer/refund-account", tok,
		map[string]any{"bank_bin": "970415", "account_number": "123456", "account_holder": "tran thi b"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "TRAN THI B", decode[payment.BankAccount](t, rec).AccountHolder)

	// The customer's refunds.
	rec = e.do(t, "GET", "/v1/refunds", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	list := decode[struct {
		Items []payment.Refund `json:"items"`
	}](t, rec)
	require.Len(t, list.Items, 1)
	require.Equal(t, bigGross, list.Items[0].Amount)
	require.Equal(t, v.ID, *list.Items[0].BookingID)
	other := e.token(t, e.customer(t, "khac@gmail.com"), auth.RoleCustomer)
	rec = e.do(t, "GET", "/v1/refunds", other, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, decode[struct {
		Items []payment.Refund `json:"items"`
	}](t, rec).Items)
}

// Review Focus 4: a refund of another customer is 404.
func TestSetRefundBankAccount_OtherCustomer404(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, dateOver48h)
	e.cancel(t, v.ID)
	var refundID uuid.UUID
	require.NoError(t, e.Pool.QueryRow(context.Background(), `SELECT id FROM refunds WHERE booking_id = $1`, v.ID).Scan(&refundID))
	acc := payment.BankAccount{BankBIN: "970436", AccountNumber: "0011001234567", AccountHolder: "Nguyen Van An"}

	other := e.customer(t, "khac@gmail.com")
	requireErr(t, e.Payments.SetRefundBankAccount(context.Background(), other, refundID, acc), http.StatusNotFound, "not_found")
	path := "/v1/refunds/" + refundID.String() + "/bank-account"
	requireCode(t, e.do(t, "PUT", path, e.token(t, other, auth.RoleCustomer), acc), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "PUT", "/v1/refunds/nope/bank-account", e.token(t, other, auth.RoleCustomer), acc), http.StatusNotFound, "not_found")
	require.Equal(t, payment.RefundAwaitingBankInfo, e.refunds(t)[0].Status)

	tok := e.token(t, e.Customer, auth.RoleCustomer)
	bad := acc
	bad.AccountNumber = "12"
	requireCode(t, e.do(t, "PUT", path, tok, bad), http.StatusUnprocessableEntity, "invalid_bank_account")
	rec := e.do(t, "PUT", path, tok, acc)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	r := e.refunds(t)[0]
	require.Equal(t, payment.RefundPending, r.Status)
	require.Equal(t, "NGUYEN VAN AN", *r.Holder)
	// Only while awaiting bank info.
	requireCode(t, e.do(t, "PUT", path, tok, acc), http.StatusConflict, "refund_not_awaiting_bank_info")
}

// The ledger stays consistent when a cancellation and the release job race.
func TestCancel_ThenReleaseJobDoesNothing(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, date47h)
	e.cancel(t, v.ID)
	e.Clock.Advance(30 * 24 * time.Hour)
	require.Equal(t, 0, e.releaseDue(t))
	e.requireBalance(t, 0, 400_000)
}
