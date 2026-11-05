package booking_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

// M-4: after an underpayment (refunded in full, payment needs_review) the
// customer gets a new link from /pay, with the same deadline.
func TestPayAfterUnderpaid_IssuesNewLink(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, 300_000)
	require.Equal(t, "needs_review", e.paymentStatus(t, oc))

	rec := e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/pay", e.token(t, e.Customer, auth.RoleCustomer), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	out := decode[booking.BookingView](t, rec)
	require.NotNil(t, out.Payment)
	newOC := e.orderCode(t, v.ID)
	require.NotEqual(t, oc, newOC)
	require.NotNil(t, out.Payment.CheckoutURL)
	require.True(t, strings.HasSuffix(*out.Payment.CheckoutURL, "/"+strconv.FormatInt(newOC, 10)), *out.Payment.CheckoutURL)
	require.True(t, out.Payment.ExpiresAt.Equal(*v.HoldExpiresAt), "no hold extension")
	require.Equal(t, e.str(t, `SELECT expires_at::text FROM payments WHERE provider_order_code = $1`, oc),
		e.str(t, `SELECT expires_at::text FROM payments WHERE provider_order_code = $1`, newOC))
	require.Equal(t, gross, e.count64(t, `SELECT amount FROM payments WHERE provider_order_code = $1`, newOC))

	// A second /pay returns the same pending link.
	rec = e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/pay", e.token(t, e.Customer, auth.RoleCustomer), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, newOC, e.orderCode(t, v.ID))
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM payments`))

	e.pay(t, newOC, gross)
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, newOC))
	require.Equal(t, "needs_review", e.paymentStatus(t, oc))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries WHERE kind = 'booking_earning'`))
	e.requireBalance(t, 342_000, 0)
}

// The old link still takes the full amount: the new payment is cancelled
// (Ruling 10), one booking_earning only.
func TestPayAfterUnderpaid_FullOnOldLinkStillAccepted(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, 300_000)
	_, err := e.Svc.Pay(context.Background(), e.Customer, v.ID)
	require.NoError(t, err)
	newOC := e.orderCode(t, v.ID)

	rec := e.webhook(t, e.Fake.MarkPaidRef(oc, gross, "FULL-OLD"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE result LIKE 'accepted_replaced_link%'`))
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
	require.Equal(t, "cancelled", e.paymentStatus(t, newOC))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries`))
	e.requireBalance(t, 342_000, 0)
}

func TestPayAfterUnderpaid_HoldExpired409(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, 300_000)
	e.Clock.T = v.HoldExpiresAt.Add(time.Second)

	rec := e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/pay", e.token(t, e.Customer, auth.RoleCustomer), nil)
	requireCode(t, rec, http.StatusConflict, "hold_expired")
	var checkoutID = e.str(t, `SELECT checkout_id::text FROM bookings WHERE id = $1`, v.ID)
	_, err := e.Payments.ReissueAfterMismatch(context.Background(), mustUUID(t, checkoutID))
	require.ErrorIs(t, err, payment.ErrPaymentNotFound)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments`))
}

// Without any underpayment there is nothing to reissue: a payment that is
// expired (or paid) stays booking_not_payable / hold_expired as before.
func TestReissue_OnlyAfterReceivedMismatch(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	checkoutID := mustUUID(t, e.str(t, `SELECT checkout_id::text FROM bookings WHERE id = $1`, v.ID))
	oc := e.orderCode(t, v.ID)

	// A pending payment is returned as is.
	p, err := e.Payments.ReissueAfterMismatch(context.Background(), checkoutID)
	require.NoError(t, err)
	require.Equal(t, oc, p.OrderCode)

	// A needs_review payment without money received (cancelled stray flag)
	// is not a mismatch to reissue after.
	e.exec(t, `UPDATE payments SET status = 'needs_review' WHERE provider_order_code = $1`, oc)
	_, err = e.Payments.ReissueAfterMismatch(context.Background(), checkoutID)
	require.ErrorIs(t, err, payment.ErrPaymentNotFound)
	_, err = e.Svc.Pay(context.Background(), e.Customer, v.ID)
	requireErr(t, err, http.StatusConflict, "booking_not_payable")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments`))
}
