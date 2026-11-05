package payment_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/payment"
)

// kinds lists the kinds of ms, in order.
func kinds(ms []payment.Mismatch) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Kind
	}
	return out
}

func findKind(ms []payment.Mismatch, kind string) *payment.Mismatch {
	for i := range ms {
		if ms[i].Kind == kind {
			return &ms[i]
		}
	}
	return nil
}

func (e *opsEnv) bookingStatus(t *testing.T, id any) string {
	t.Helper()
	return e.str(t, `SELECT status FROM bookings WHERE id = $1`, id)
}

func (e *opsEnv) earnings(t *testing.T, bookingID any) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM ledger_entries WHERE kind = 'booking_earning' AND booking_id = $1`, bookingID)
}

func (e *opsEnv) reconcile(t *testing.T) (int, []payment.Mismatch) {
	t.Helper()
	n, ms, err := e.Payments.ReconcilePayments(context.Background(), e.Clock.Now(), e.Booking)
	require.NoError(t, err)
	return n, ms
}

// A transfer whose status lagged (503, awaiting_status row) and whose
// webhook never came back is processed by the reconciliation through the
// webhook path once StatusLagLimit passed, flagged needs_review, once.
func TestReconcile_ReplaysStaleAwaiting(t *testing.T) {
	e := newOpsEnv(t)
	v, oc := e.book(t, "a", opsVisitOver48h)
	body := e.Fake.MarkPaid(oc, opsGross)
	e.Fake.LagNextStatus()
	requireHTTPCode(t, e.webhook(t, body), http.StatusServiceUnavailable, "payment_not_confirmed")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE result = 'awaiting_status'`))

	// Not stale yet: nothing replayed.
	e.Clock.T = opsNow.Add(29 * time.Minute)
	e.Fake.LagNextStatus()
	_, ms := e.reconcile(t)
	require.Nil(t, findKind(ms, "stale_awaiting_processed"))
	require.Equal(t, "pending_payment", e.bookingStatus(t, v.ID))
	// The lag set above was consumed by the window check: provider shows
	// nothing, nothing recorded, no mismatch on money.
	require.Empty(t, ms)

	e.Clock.T = opsNow.Add(31 * time.Minute)
	e.Fake.LagNextStatus() // the provider's status still lags
	_, ms = e.reconcile(t)
	m := findKind(ms, "stale_awaiting_processed")
	require.NotNil(t, m, "%v", kinds(ms))
	require.Equal(t, "confirmed", m.Detail)
	require.Equal(t, oc, *m.OrderCode)
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, payment.StatusNeedsReview, e.str(t, `SELECT status FROM payments WHERE provider_order_code = $1`, oc))
	require.Equal(t, 1, e.earnings(t, v.ID))
	require.Equal(t, int(opsGross), e.count(t, `SELECT amount FROM payment_events WHERE result = 'confirmed'`))
	require.NotNil(t, findKind(ms, "needs_review_open"))

	_, ms = e.reconcile(t)
	require.Equal(t, []string{"needs_review_open"}, kinds(ms))
	require.Equal(t, 1, e.earnings(t, v.ID))
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM payment_events`))
	require.NoError(t, e.Ledger.Verify(context.Background(), e.PartnerID))
}

// The payload is stored as jsonb (keys reordered, whitespace dropped); it
// must still verify and give the same transfer reference, with or without a
// bank reference.
func TestReplayAwaitingPayloadStillVerifies(t *testing.T) {
	e := newOpsEnv(t)
	for i, ref := range []string{"BANK-REF-1", ""} {
		_, oc := e.book(t, "k"+itoa(int64(i)), opsVisitOver48h)
		body := e.Fake.MarkPaidRef(oc, opsGross, ref)
		e.Fake.LagNextStatus()
		requireHTTPCode(t, e.webhook(t, body), http.StatusServiceUnavailable, "payment_not_confirmed")
		stored := e.str(t, `SELECT payload::text FROM payment_events
			WHERE result = 'awaiting_status' AND payment_id = $1`, e.paymentID(t, oc))
		require.NotEqual(t, string(body), stored, "jsonb rewrites the body")

		orig, err := e.Fake.VerifyWebhook(body)
		require.NoError(t, err)
		again, err := e.Fake.VerifyWebhook([]byte(stored))
		require.NoError(t, err, "replayed payload must verify")
		require.Equal(t, payment.TransferReference(orig), payment.TransferReference(again))
		require.Equal(t, orig.Amount, again.Amount)
	}
}

// PayOS's docs do not establish that transactions[].reference of the status
// API is the webhook's data.reference, so money the provider shows without a
// webhook is NOT recorded by the reconciliation: it is reported as
// missing_transfer only. The webhook arriving later is processed normally,
// exactly once.
func TestReconcile_MissingTransferNotRecoveredWithoutWebhook(t *testing.T) {
	e := newOpsEnv(t)
	v, oc := e.book(t, "a", opsVisitOver48h)
	body := e.Fake.MarkPaidRef(oc, opsGross, "BANK-REF-9")

	e.Clock.T = opsNow.Add(time.Hour)
	_, ms := e.reconcile(t)
	require.Equal(t, []string{"missing_transfer"}, kinds(ms))
	m := ms[0]
	require.Equal(t, opsGross, *m.ProviderAmount)
	require.Equal(t, int64(0), *m.RecordedAmount)
	require.Equal(t, e.paymentID(t, oc), *m.PaymentID)
	require.Equal(t, "BANK-REF-9", *m.Reference)
	require.Equal(t, "pending_payment", e.bookingStatus(t, v.ID))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM payment_events`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))

	rec := e.webhook(t, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE dedupe_key = $1`,
		payment.DedupeKey(oc, "BANK-REF-9")))
	require.Equal(t, 1, e.earnings(t, v.ID))

	_, ms = e.reconcile(t)
	require.Empty(t, ms)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events`))
}

// Review Focus 4: the reconciliation and a live webhook of the same transfer
// run at once. Even iterations: the transfer left a stale awaiting_status row
// that the reconciliation replays while the provider redelivers it. Odd
// iterations: a fresh transfer. Always one main event, one earning, a clean
// ledger.
func TestReconcile_RacesLiveWebhook(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		e := newOpsEnv(t)
		v, oc := e.book(t, "race", opsVisitOver48h)
		ref := "RACE-REF-" + itoa(int64(i))
		body := e.Fake.MarkPaidRef(oc, opsGross, ref)
		if i%2 == 0 {
			e.Fake.LagNextStatus()
			requireHTTPCode(t, e.webhook(t, body), http.StatusServiceUnavailable, "payment_not_confirmed")
			e.Clock.T = opsNow.Add(31 * time.Minute)
		}
		var wg sync.WaitGroup
		var errA, errB error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errA = e.Payments.ProcessWebhook(ctx, body, e.Booking)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, errB = e.Payments.ReconcilePayments(ctx, e.Clock.Now(), e.Booking)
		}()
		close(start)
		wg.Wait()
		require.NoError(t, errA, "iteration %d", i)
		require.NoError(t, errB, "iteration %d", i)

		require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE dedupe_key = $1`,
			payment.DedupeKey(oc, ref)), "iteration %d", i)
		require.Equal(t, 1, e.earnings(t, v.ID), "iteration %d", i)
		require.Equal(t, "confirmed", e.bookingStatus(t, v.ID), "iteration %d", i)
		require.NoError(t, e.Ledger.Verify(ctx, e.PartnerID), "iteration %d", i)
		_, ms := e.reconcile(t)
		require.Nil(t, findKind(ms, "missing_transfer"), "iteration %d: %v", i, kinds(ms))
		require.Nil(t, findKind(ms, "recorded_exceeds_provider"), "iteration %d: %v", i, kinds(ms))
	}
}

// A transfer without bank reference (raw: key) already processed: the
// provider amount equals what was recorded, nothing to report.
func TestReconcile_EmptyReferenceNotDoubleCounted(t *testing.T) {
	e := newOpsEnv(t)
	v, oc := e.book(t, "a", opsVisitOver48h)
	rec := e.webhook(t, e.Fake.MarkPaidRef(oc, opsGross, ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))

	e.Clock.T = opsNow.Add(time.Hour)
	n, ms := e.reconcile(t)
	require.Equal(t, 1, n)
	require.Empty(t, ms)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events`))
	require.Equal(t, 1, e.earnings(t, v.ID))
}

// The provider reports more money than recorded but lists no transfer for
// it: missing_transfer, nothing written.
func TestReconcile_MissingTransferWhenNoTransactions(t *testing.T) {
	e := newOpsEnv(t)
	_, oc := e.book(t, "a", opsVisitOver48h)
	e.pay(t, oc, opsGross)
	e.Fake.SetAmountPaid(oc, opsGross+50_000)
	entries := e.count(t, `SELECT count(*) FROM ledger_entries`)

	e.Clock.T = opsNow.Add(time.Hour)
	_, ms := e.reconcile(t)
	require.Equal(t, []string{"missing_transfer"}, kinds(ms))
	require.Equal(t, opsGross+50_000, *ms[0].ProviderAmount)
	require.Equal(t, opsGross, *ms[0].RecordedAmount)
	require.Nil(t, ms[0].Reference)
	require.Equal(t, entries, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events`))
}

// Recorded money the provider no longer shows is reported once the last
// event is StatusLagLimit old.
func TestReconcile_RecordedExceedsProvider(t *testing.T) {
	e := newOpsEnv(t)
	_, oc := e.book(t, "a", opsVisitOver48h)
	e.pay(t, oc, opsGross)
	e.Fake.SetAmountPaid(oc, 100_000)

	e.Clock.T = opsNow.Add(10 * time.Minute)
	_, ms := e.reconcile(t)
	require.Empty(t, ms)

	e.Clock.T = opsNow.Add(31 * time.Minute)
	_, ms = e.reconcile(t)
	require.Equal(t, []string{"recorded_exceeds_provider"}, kinds(ms))
	require.Equal(t, int64(100_000), *ms[0].ProviderAmount)
	require.Equal(t, opsGross, *ms[0].RecordedAmount)
}

func TestReconcile_ProviderErrorRecorded(t *testing.T) {
	e := newOpsEnv(t)
	_, oc := e.book(t, "a", opsVisitOver48h)
	e.Fake.FailNextStatus(errors.New("payos down"))

	n, ms := e.reconcile(t)
	require.Equal(t, 1, n)
	require.Equal(t, []string{"provider_error"}, kinds(ms))
	require.Equal(t, oc, *ms[0].OrderCode)
	require.Contains(t, ms[0].Detail, "payos down")
}

// An order code the provider does not know and for which no link was ever
// stored is skipped silently.
func TestReconcile_UnknownOrderWithoutLinkSkipped(t *testing.T) {
	e := newOpsEnv(t)
	_, oc := e.book(t, "a", opsVisitOver48h)
	e.exec(t, `UPDATE payments SET checkout_url = NULL, qr_code = NULL, provider_order_code = provider_order_code + 1000000
		WHERE provider_order_code = $1`, oc)
	_, ms := e.reconcile(t)
	require.Empty(t, ms)

	// With a stored link, the provider not knowing it is an error.
	e.exec(t, `UPDATE payments SET checkout_url = 'http://x' WHERE provider_order_code = $1`, oc+1000000)
	_, ms = e.reconcile(t)
	require.Equal(t, []string{"provider_error"}, kinds(ms))
}

// Every unresolved needs_review payment is listed, even outside the 72h
// window; a resolved one is not.
func TestReconcile_NeedsReviewOpenListed(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	_, oc := e.book(t, "a", opsVisitOver48h)
	e.pay(t, oc, 400_000) // overpaid: needs_review
	id := e.paymentID(t, oc)

	e.Clock.T = opsNow.Add(5 * 24 * time.Hour)
	n, ms := e.reconcile(t)
	require.Equal(t, 1, n)
	require.Equal(t, []string{"needs_review_open"}, kinds(ms))
	require.Equal(t, id, *ms[0].PaymentID)

	_, err := e.Payments.ResolveReview(ctx, e.Admin, id, nil)
	require.NoError(t, err)
	n, ms = e.reconcile(t)
	require.Equal(t, 0, n)
	require.Empty(t, ms)
}

func TestReconcile_OutsideWindowSkipped(t *testing.T) {
	e := newOpsEnv(t)
	_, oc := e.book(t, "a", opsVisitOver48h)
	e.pay(t, oc, opsGross)
	e.Fake.SetAmountPaid(oc, 1) // would be a mismatch if it were checked

	e.Clock.T = opsNow.Add(4 * 24 * time.Hour)
	calls := e.Fake.StatusCalls()
	n, ms := e.reconcile(t)
	require.Equal(t, 0, n)
	require.Empty(t, ms)
	require.Equal(t, calls, e.Fake.StatusCalls(), "no GetStatus outside the window")
}
