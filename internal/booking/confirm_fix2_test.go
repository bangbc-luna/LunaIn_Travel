package booking_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/payment"
)

// Ruling 13: the provider's status keeps lagging (PENDING, nothing paid):
// 503 with a tracking row, then processed once it is 30 minutes old, with
// the signed webhook amount and the payment needs_review.
func TestWebhook_StatusLagBoundedRetry(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	body := e.Fake.MarkPaid(oc, gross)
	e.Fake.SetState(oc, payment.StatePending)
	e.Fake.SetAmountPaid(oc, 0)
	key := "payos:" + strconv.FormatInt(oc, 10) + ":" + ref(oc)

	requireCode(t, e.webhook(t, body), http.StatusServiceUnavailable, "payment_not_confirmed")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE dedupe_key = $1 AND result = 'awaiting_status'`,
		key+":awaiting"))
	e.Clock.Advance(29 * time.Minute)
	requireCode(t, e.webhook(t, body), http.StatusServiceUnavailable, "payment_not_confirmed")
	require.Equal(t, "pending_payment", e.bookingStatus(t, v.ID))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM payment_events WHERE dedupe_key = $1`, key))

	e.Clock.Advance(2 * time.Minute) // 31 minutes after the first sighting
	rec := e.webhook(t, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, "needs_review", e.paymentStatus(t, oc))
	require.Equal(t, ref(oc), e.str(t, `SELECT provider_reference FROM payments WHERE provider_order_code = $1`, oc))
	require.Empty(t, e.refunds(t))
	e.requireBalance(t, 342_000, 0)
}

// Ruling 13: a terminal status without money after a signed webhook is
// processed at once (never dropped): here Ruling 10 accepts it.
func TestWebhook_TerminalStatusWithoutMoneyAccepted(t *testing.T) {
	e := newEnv(t)
	id, oldOC, newOC := e.replacedLink(t)
	body := e.Fake.MarkPaid(oldOC, gross)
	e.Fake.SetState(oldOC, payment.StateCancelled)
	e.Fake.SetAmountPaid(oldOC, 0)

	rec := e.webhook(t, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "confirmed", e.str(t, `SELECT status FROM bookings WHERE id = $1`, id))
	require.Equal(t, "needs_review", e.paymentStatus(t, oldOC))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments
		WHERE provider_order_code = $1 AND provider_reference = $2 AND paid_at IS NOT NULL`, oldOC, ref(oldOC)))
	require.Equal(t, "cancelled", e.paymentStatus(t, newOC))
	e.requireBalance(t, 342_000, 0)
}

// Ruling 13: same, on an already paid checkout: a duplicate refund and the
// payment flagged.
func TestWebhook_TerminalStatusWithoutMoneyDuplicate(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, gross)
	body := e.Fake.MarkPaidRef(oc, gross, "R2")
	e.Fake.SetState(oc, payment.StateExpired)
	e.Fake.SetAmountPaid(oc, 0)

	rec := e.webhook(t, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "needs_review", e.paymentStatus(t, oc))
	r := "R2"
	require.Equal(t, []refundRow{{Amount: gross, Reason: "duplicate_payment", Status: "awaiting_bank_info", Reference: &r}},
		e.refunds(t))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries`))
}

// Ruling 13: transfer 1 (100k) was an underpayment, refunded; transfer 2
// (380k) arrives while the status still says UNDERPAID/100k. Transfer 2 is
// handled with its signed 380k: the booking is confirmed.
func TestWebhook_StaleStatusPartialThenFullPayment(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, 100_000)
	body := e.Fake.MarkPaidRef(oc, gross, "R2")
	e.Fake.SetState(oc, payment.StateUnderpaid)
	e.Fake.SetAmountPaid(oc, 100_000)

	rec := e.webhook(t, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
	r := ref(oc)
	require.Equal(t, []refundRow{{Amount: 100_000, Reason: "payment_mismatch", Status: "awaiting_bank_info", Reference: &r}},
		e.refunds(t), "only transfer 1 is refunded, never a 100k under-refund of transfer 2")
	e.requireBalance(t, 342_000, 0)
}

// Same, but the booking can no longer be served: the full 380k goes back.
func TestWebhook_StaleStatusPartialThenFullPaymentTooLate(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, 100_000)
	e.Clock.Advance(30 * time.Minute)
	_, err := e.Listing.Hide(context.Background(), e.Admin, e.L.ID, "Vi phạm nội dung")
	require.NoError(t, err)
	body := e.Fake.MarkPaidRef(oc, gross, "R2")
	e.Fake.SetState(oc, payment.StateUnderpaid)
	e.Fake.SetAmountPaid(oc, 100_000)

	rec := e.webhook(t, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "expired", e.bookingStatus(t, v.ID))
	rows := e.refunds(t)
	require.Len(t, rows, 2)
	require.Equal(t, int64(100_000), rows[0].Amount)
	require.Equal(t, gross, rows[1].Amount)
	require.Equal(t, "late_payment", rows[1].Reason)
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))
}

// Ruling 14: a refunded stray transfer on the old (replaced) link does not
// block the full payment on the replacement link.
func TestStrayOnOldLinkDoesNotBlockPayment(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id, oldOC, newOC := e.replacedLink(t)
	e.Clock.Advance(30 * time.Minute)
	_, err := e.Listing.Hide(ctx, e.Admin, e.L.ID, "Vi phạm nội dung")
	require.NoError(t, err)
	e.pay(t, oldOC, gross)
	require.Equal(t, "needs_review", e.paymentStatus(t, oldOC))
	require.Len(t, e.refunds(t), 1)

	_, err = e.Listing.Unhide(ctx, e.Admin, e.L.ID)
	require.NoError(t, err)
	e.pay(t, newOC, gross)
	require.Equal(t, "confirmed", e.str(t, `SELECT status FROM bookings WHERE id = $1`, id))
	require.Equal(t, "paid", e.paymentStatus(t, newOC))
	require.Len(t, e.refunds(t), 1)
	e.requireBalance(t, 342_000, 0)
}

// N4: a failing best-effort CancelLink of the replacement only logs.
func TestAcceptReplacedLink_CancelFailureOnlyLogs(t *testing.T) {
	e := newEnv(t)
	id, oldOC, newOC := e.replacedLink(t)
	newBody := e.Fake.MarkPaid(newOC, gross) // paid at the provider: CancelLink fails
	e.pay(t, oldOC, gross)
	require.Equal(t, "confirmed", e.str(t, `SELECT status FROM bookings WHERE id = $1`, id))
	require.Equal(t, "cancelled", e.paymentStatus(t, newOC))

	rec := e.webhook(t, newBody)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r := ref(newOC)
	require.Equal(t, []refundRow{{Amount: gross, Reason: "duplicate_payment", Status: "awaiting_bank_info", Reference: &r}},
		e.refunds(t))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries`))
}

// N4: what a lost link's provider state does to /pay.
func TestReplaceLostLink_ByProviderState(t *testing.T) {
	cases := []struct {
		state    string
		rotates  bool
		status   int
		code     string
		payments int
	}{
		{payment.StateCancelled, true, 0, "", 3},
		{payment.StateExpired, true, 0, "", 3},
		{payment.StateUnderpaid, false, http.StatusConflict, "payment_processing", 2},
		{payment.StateProcessing, false, http.StatusConflict, "payment_processing", 2},
		{payment.StateFailed, false, http.StatusBadGateway, "payment_provider_unavailable", 2},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			e := newEnv(t)
			// The first link and its inline replacement both lose their response.
			e.Fake.LoseNextCreateResponse()
			e.Fake.LoseNextCreateResponse()
			v := e.create(t, "k")
			oc := e.orderCode(t, v.ID)
			e.Fake.SetState(oc, tc.state)

			got, err := e.Svc.Pay(context.Background(), e.Customer, v.ID)
			if tc.rotates {
				require.NoError(t, err)
				require.NotNil(t, got.Payment.CheckoutURL)
				require.Equal(t, "cancelled", e.paymentStatus(t, oc))
				require.NotEqual(t, oc, e.orderCode(t, v.ID))
			} else {
				requireErr(t, err, tc.status, tc.code)
				require.Equal(t, "pending", e.paymentStatus(t, oc))
			}
			require.Equal(t, tc.payments, e.count(t, `SELECT count(*) FROM payments`))
		})
	}
}
