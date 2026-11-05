package booking_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// requireLateReconfirmed: the expired booking was re-confirmed by a late
// payment, with exactly one ledger posting.
func (e *env) requireLateReconfirmed(t *testing.T, id uuid.UUID) {
	t.Helper()
	require.Equal(t, "confirmed", e.bookingStatus(t, id))
	require.Equal(t, "paid", e.checkoutStatus(t, id))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'expired' AND to_status = 'confirmed'`, id))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM refunds WHERE booking_id IS NOT NULL`))
	e.requireBalance(t, 342_000, 0)
}

// Ruling 17: underpaid (refunded, payment needs_review) → hold expiry closes
// the checkout → the full amount while the booking is still bookable
// re-confirms it; the underpayment refund stays.
func TestUnderpaidThenExpiry_FullPaymentReconfirms(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, 300_000)
	require.Equal(t, "needs_review", e.paymentStatus(t, oc))
	e.expire(t, v.ID)
	require.Equal(t, "closed", e.checkoutStatus(t, v.ID))

	rec := e.webhook(t, e.Fake.MarkPaidRef(oc, gross, "R2"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	e.requireLateReconfirmed(t, v.ID)
	r := ref(oc)
	require.Equal(t, []refundRow{{Amount: 300_000, Reason: "payment_mismatch", Status: "awaiting_bank_info", Reference: &r}},
		e.refunds(t), "only the underpayment refund")
}

// Ruling 17: a refunded stray on the old (replaced) link → hold expiry →
// the full payment on the new link re-confirms.
func TestStrayOnOldLinkThenExpiry_FullPaymentReconfirms(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	idStr, oldOC, newOC := e.replacedLink(t)
	id := uuid.MustParse(idStr)
	e.Clock.Advance(30 * time.Minute)
	_, err := e.Listing.Hide(ctx, e.Admin, e.L.ID, "Vi phạm nội dung")
	require.NoError(t, err)
	e.pay(t, oldOC, gross)
	require.Equal(t, "needs_review", e.paymentStatus(t, oldOC))
	require.Len(t, e.refunds(t), 1)

	e.expire(t, id)
	require.Equal(t, "closed", e.checkoutStatus(t, id))
	require.Equal(t, "expired", e.paymentStatus(t, newOC))
	_, err = e.Listing.Unhide(ctx, e.Admin, e.L.ID)
	require.NoError(t, err)

	e.pay(t, newOC, gross)
	e.requireLateReconfirmed(t, id)
	require.Equal(t, "paid", e.paymentStatus(t, newOC))
	require.Len(t, e.refunds(t), 1, "only the stray refund")
}

// reformat re-serialises a webhook body: the "data" object with its keys in
// reverse order and indented, the envelope with other spacing. The signature
// still verifies (PayOS signs the canonical string, not the bytes).
func reformat(t *testing.T, body []byte) []byte {
	t.Helper()
	var w map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &w))
	dec := json.NewDecoder(bytes.NewReader(w["data"]))
	dec.UseNumber()
	var data map[string]any
	require.NoError(t, dec.Decode(&data))
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	var b bytes.Buffer
	b.WriteString("{\n    ")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(" ,\n    ")
		}
		kb, err := json.Marshal(k)
		require.NoError(t, err)
		vb, err := json.Marshal(data[k])
		require.NoError(t, err)
		b.Write(kb)
		b.WriteString(" :  ")
		b.Write(vb)
	}
	b.WriteString("\n}")
	require.NotEqual(t, []byte(w["data"]), b.Bytes())
	out := []byte(`{ "signature" : ` + string(w["signature"]) + `,  "data" : ` + b.String() +
		`, "desc": "success", "code" :"00", "success": true }`)
	require.False(t, slices.Equal(body, out))
	return out
}

// Ruling 17 (refined Ruling 16): a reference-less transfer redelivered with
// different JSON formatting is the same transfer: deduped.
func TestWebhook_EmptyReferenceRedeliveryReformattedDeduped(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	body := e.Fake.MarkPaidRef(oc, 100_000, "")
	again := reformat(t, body)
	require.Equal(t, rawReference(t, body), rawReference(t, again))

	rec := e.webhook(t, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, e.refunds(t), 1)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events`))

	rec = e.webhook(t, again)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, e.refunds(t), 1)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events`))
	require.Equal(t, "pending_payment", e.bookingStatus(t, v.ID))
}
