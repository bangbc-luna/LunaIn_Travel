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

func (e *env) paymentID(t *testing.T, oc int64) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT id FROM payments WHERE provider_order_code = $1`, oc).Scan(&id))
	return id
}

// I1 (Ruling 20): the booking refund points at the payment that confirmed
// the checkout, even when that payment is needs_review (overpaid) and a later
// duplicate transfer landed on the replaced link of the same checkout.
func TestCancel_RefundUsesConfirmingPayment(t *testing.T) {
	e := newEnv(t)
	id, oldOC, newOC := e.replacedLink(t)
	bookingID := uuid.MustParse(id)

	e.pay(t, newOC, gross+10_000) // overpaid: confirmed, payment needs_review
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, bookingID))
	require.Equal(t, payment.StatusNeedsReview, e.paymentStatus(t, newOC))
	confirming := e.paymentID(t, newOC)
	require.Equal(t, confirming.String(), e.str(t, `SELECT paid_payment_id::text FROM checkouts c
		JOIN bookings b ON b.checkout_id = c.id WHERE b.id = $1`, bookingID))

	// Later, a duplicate transfer on the stale (replaced) link.
	e.Clock.Advance(time.Hour)
	rec := e.webhook(t, e.Fake.MarkPaidRef(oldOC, gross, "LATE-DUP"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, payment.StatusNeedsReview, e.paymentStatus(t, oldOC))

	e.cancel(t, bookingID)
	var refundPayment uuid.UUID
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT payment_id FROM refunds WHERE booking_id = $1`, bookingID).Scan(&refundPayment))
	require.Equal(t, confirming, refundPayment, "refunded against the confirming payment, not the newest transfer")
	e.requireBalance(t, 0, gross-commission-190_000)
}

// Ruling 21: after the current service end the no-show path owns the
// booking; before it, a cancel after the original start is allowed at 0%.
func TestCancel_AfterServiceEnd409(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)

	e.Clock.T = serviceEnd
	got, err := e.Svc.GetMine(context.Background(), e.Customer, v.ID)
	require.NoError(t, err)
	require.False(t, got.CanCancel)
	_, err = e.Svc.CancelByCustomer(context.Background(), e.Customer, v.ID, nil)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	_, err = e.Svc.CancelByAdmin(context.Background(), e.Admin, v.ID, booking.CancelModeForceMajeure, "x")
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, v.ID))

	e.Clock.T = serviceEnd.Add(-time.Minute)
	got, err = e.Svc.GetMine(context.Background(), e.Customer, v.ID)
	require.NoError(t, err)
	require.True(t, got.CanCancel)
	e.cancel(t, v.ID)
	require.Empty(t, e.refunds(t), "original start passed: 0%")
	e.requireBalance(t, 0, gross-commission)
}

func TestCancel_ConcurrentDoubleCancel(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, dateOver48h)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i == 0 {
				_, errs[i] = e.Svc.CancelByCustomer(context.Background(), e.Customer, v.ID, nil)
			} else {
				_, errs[i] = e.Svc.CancelByAdmin(context.Background(), e.Admin, v.ID, booking.CancelModeForceMajeure, "sự cố")
			}
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		var ae *httpx.AppError
		require.True(t, errors.As(err, &ae), err)
		require.Equal(t, "invalid_booking_state", ae.Code)
	}
	require.Equal(t, 1, ok)
	require.Len(t, e.refunds(t), 1)
	require.Equal(t, bigGross, e.refunds(t)[0].Amount)
	require.Equal(t, int64(0), e.bookingPending(t, v.ID))
	require.NoError(t, e.Ledger.Verify(context.Background(), e.P.ID))
}

func TestCancel_CompletedOrNoShow409(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		e := newEnv(t)
		v := e.paidBooking(t)
		e.Clock.T = onVisitDay
		_, err := e.Svc.CheckIn(context.Background(), e.P.ID, e.P.OwnerUserID, e.qrToken(t, v.ID))
		require.NoError(t, err)
		_, err = e.Svc.CancelByCustomer(context.Background(), e.Customer, v.ID, nil)
		requireErr(t, err, http.StatusConflict, "invalid_booking_state")
		_, err = e.Svc.CancelByAdmin(context.Background(), e.Admin, v.ID, booking.CancelModePolicy, "x")
		requireErr(t, err, http.StatusConflict, "invalid_booking_state")
		require.Equal(t, booking.StatusCompleted, e.bookingStatus(t, v.ID))
	})
	t.Run("no_show", func(t *testing.T) {
		e := newEnv(t)
		v := e.paidBooking(t)
		e.Clock.T = serviceEnd.Add(time.Minute)
		require.Equal(t, 1, e.markNoShows(t))
		_, err := e.Svc.CancelByCustomer(context.Background(), e.Customer, v.ID, nil)
		requireErr(t, err, http.StatusConflict, "invalid_booking_state")
		require.Equal(t, booking.StatusNoShow, e.bookingStatus(t, v.ID))
		require.Empty(t, e.refunds(t))
	})
}

func TestCancelByAdmin_SavedRefundAccountPending(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, date47h)
	_, err := e.Payments.SaveRefundAccount(context.Background(), e.Customer,
		payment.BankAccount{BankBIN: "970436", AccountNumber: "0011001234567", AccountHolder: "Nguyễn Văn An"})
	require.NoError(t, err)

	_, err = e.Svc.CancelByAdmin(context.Background(), e.Admin, v.ID, booking.CancelModeForceMajeure, "Đóng cửa đột xuất")
	require.NoError(t, err)
	rs := e.refunds(t)
	require.Len(t, rs, 1)
	require.Equal(t, payment.RefundPending, rs[0].Status)
	require.Equal(t, payment.RefundAdminCancel, rs[0].Reason)
	require.Equal(t, "NGUYEN VAN AN", *rs[0].Holder)
}

// Money arriving after a cancellation is a duplicate payment, refunded in
// full at payment level; the ledger does not move.
func TestPaid_AfterCancelIsDuplicateRefund(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, date47h)
	var oc int64
	require.NoError(t, e.Pool.QueryRow(context.Background(), `SELECT p.provider_order_code FROM payments p
		JOIN bookings b ON b.checkout_id = p.checkout_id WHERE b.id = $1`, v.ID).Scan(&oc))
	e.cancel(t, v.ID)
	e.requireBalance(t, 0, 400_000)

	rec := e.webhook(t, e.Fake.MarkPaidRef(oc, bigGross, "AFTER-CANCEL"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rs := e.refunds(t)
	require.Len(t, rs, 2)
	require.Equal(t, int64(500_000), rs[0].Amount)
	require.Equal(t, payment.RefundCustomerCancel, rs[0].Reason)
	require.Equal(t, bigGross, rs[1].Amount)
	require.Equal(t, payment.RefundDuplicatePayment, rs[1].Reason)
	require.Nil(t, rs[1].BookingID)
	require.Equal(t, booking.StatusCancelled, e.bookingStatus(t, v.ID))
	e.requireBalance(t, 0, 400_000)
}

// A payment-level refund (booking_id NULL) is owned through its checkout.
func TestSetRefundBankAccount_PaymentLevelRefund(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	rec := e.webhook(t, e.Fake.MarkPaidRef(e.orderCode(t, v.ID), 100_000, "UNDER"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var refundID uuid.UUID
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT id FROM refunds WHERE booking_id IS NULL AND reason = 'payment_mismatch'`).Scan(&refundID))
	acc := payment.BankAccount{BankBIN: "970436", AccountNumber: "0011001234567", AccountHolder: "an"}

	other := e.customer(t, "khac@gmail.com")
	requireErr(t, e.Payments.SetRefundBankAccount(context.Background(), other, refundID, acc), http.StatusNotFound, "not_found")
	require.NoError(t, e.Payments.SetRefundBankAccount(context.Background(), e.Customer, refundID, acc))
	r := e.refunds(t)[0]
	require.Equal(t, payment.RefundPending, r.Status)
	require.Equal(t, "AN", *r.Holder)
}

// M5: another customer's booking is 404 even with an invalid bank account.
func TestCancel_OtherCustomerBadBank404(t *testing.T) {
	e := newEnv(t)
	v := e.bigBooking(t, dateOver48h)
	other := e.token(t, e.customer(t, "khac@gmail.com"), auth.RoleCustomer)
	bad := map[string]any{"bank_account": map[string]any{"bank_bin": "1", "account_number": "2", "account_holder": ""}}
	requireCode(t, e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/cancel", other, bad), http.StatusNotFound, "not_found")
	// The owner gets the 422; no body cancels without an account (chunked
	// empty bodies: TestDecodeOptionalJSON).
	own := e.token(t, e.Customer, auth.RoleCustomer)
	requireCode(t, e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/cancel", own, bad), http.StatusUnprocessableEntity, "invalid_bank_account")
	rec := e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/cancel", own, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, booking.StatusCancelled, decode[booking.BookingView](t, rec).Status)
}
