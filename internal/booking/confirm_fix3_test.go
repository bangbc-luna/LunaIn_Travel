package booking_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// rawReference is the Ruling 16 fallback reference of a webhook body:
// "raw:" + hex sha256 of its "data" bytes as sent.
func rawReference(t *testing.T, body []byte) string {
	t.Helper()
	var w struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &w))
	sum := sha256.Sum256(w.Data)
	return "raw:" + hex.EncodeToString(sum[:])
}

// Ruling 15 (C1): an all-late checkout whose payment ended needs_review
// (late + overpaid) gets a second full transfer: it is a payment-level
// duplicate refund, never a second refund of the same booking (which would
// violate refunds_booking_idx and 500 forever).
func TestLateRefundedCheckout_SecondTransferIsDuplicate(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.expire(t, v.ID)
	_, err := e.Listing.Hide(context.Background(), e.Admin, e.L.ID, "Vi phạm nội dung")
	require.NoError(t, err)
	e.pay(t, oc, 400_000)
	require.Equal(t, "needs_review", e.paymentStatus(t, oc))
	require.Equal(t, "closed", e.checkoutStatus(t, v.ID))
	require.Len(t, e.refunds(t), 2)

	rec := e.webhook(t, e.Fake.MarkPaidRef(oc, gross, "R2"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events
		WHERE dedupe_key = $1 AND result = 'duplicate_refunded' AND processed_at IS NOT NULL`,
		"payos:"+strconv.FormatInt(oc, 10)+":R2"))
	rows := e.refunds(t)
	require.Len(t, rows, 3)
	r2 := "R2"
	require.Equal(t, refundRow{Amount: gross, Reason: "duplicate_payment", Status: "awaiting_bank_info", Reference: &r2}, rows[2])
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM refunds WHERE booking_id = $1`, v.ID), "no new booking refund")
	require.Equal(t, "expired", e.bookingStatus(t, v.ID))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))
}

// Ruling 15: a booking already refunded blocks even a late but reconfirmable
// payment on another delivery path (the checkout is not closed yet here:
// the refund row alone is enough).
func TestCheckoutWithBookingRefund_TakesNoMoney(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.expire(t, v.ID)
	// A late refund already exists for the booking (e.g. an earlier transfer),
	// while the payment itself has no reference.
	e.exec(t, `INSERT INTO refunds (id, payment_id, checkout_id, booking_id, amount, reason, status)
		SELECT gen_random_uuid(), p.id, p.checkout_id, $1, $2, 'late_payment', 'awaiting_bank_info'
		FROM payments p WHERE p.provider_order_code = $3`, v.ID, gross, oc)
	e.exec(t, `UPDATE checkouts SET status = 'open' WHERE id = (SELECT checkout_id FROM bookings WHERE id = $1)`, v.ID)
	e.pay(t, oc, gross)

	require.Equal(t, "expired", e.bookingStatus(t, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM refunds WHERE reason = 'duplicate_payment' AND booking_id IS NULL`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))
}

// Ruling 16: transfers without a bank reference are told apart by the hash
// of their signed data; a redelivery is deduped.
func TestWebhook_EmptyReferenceFallback(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	body1 := e.Fake.MarkPaidRef(oc, 100_000, "")
	e.Clock.Advance(time.Minute) // another transactionDateTime
	body2 := e.Fake.MarkPaidRef(oc, gross, "")
	ref1, ref2 := rawReference(t, body1), rawReference(t, body2)
	require.NotEqual(t, ref1, ref2)

	rec := e.webhook(t, body1)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []refundRow{{Amount: 100_000, Reason: "payment_mismatch", Status: "awaiting_bank_info", Reference: &ref1}},
		e.refunds(t))

	rec = e.webhook(t, body2)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, ref2, e.str(t, `SELECT provider_reference FROM payments WHERE provider_order_code = $1`, oc))
	ocs := strconv.FormatInt(oc, 10)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE dedupe_key = $1`, "payos:"+ocs+":"+ref1))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE dedupe_key = $1`, "payos:"+ocs+":"+ref2))

	// Redelivery of the first transfer: deduped, nothing new.
	rec = e.webhook(t, body1)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, e.refunds(t), 1)
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM payment_events`))
	e.requireBalance(t, 342_000, 0)
}
