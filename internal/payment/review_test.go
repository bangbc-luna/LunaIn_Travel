package payment_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

// overpaid books and pays 400.000 for 380.000: booking confirmed, checkout
// paid, payment needs_review (TestPaid_Overpaid). Returns the payment id
// and its order code.
func (e *opsEnv) overpaid(t *testing.T, key string) (uuid.UUID, int64) {
	t.Helper()
	_, oc := e.book(t, key, opsVisitOver48h)
	e.pay(t, oc, 400_000)
	require.Equal(t, payment.StatusNeedsReview, e.str(t, `SELECT status FROM payments WHERE provider_order_code = $1`, oc))
	return e.paymentID(t, oc), oc
}

// reviewState is the review_state of payment id in the admin list.
func (e *opsEnv) reviewState(t *testing.T, id uuid.UUID) string {
	t.Helper()
	ps, _, err := e.Payments.AdminListPayments(context.Background(), payment.PaymentFilter{Limit: 100})
	require.NoError(t, err)
	for _, p := range ps {
		if p.ID == id {
			return p.ReviewState
		}
	}
	t.Fatalf("payment %s not listed", id)
	return ""
}

func TestMarkReviewSeen_Idempotent(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	id, _ := e.overpaid(t, "k")
	require.Equal(t, "new", e.reviewState(t, id))

	p, err := e.Payments.MarkReviewSeen(ctx, e.Admin, id)
	require.NoError(t, err)
	require.Equal(t, "seen", p.ReviewState)
	require.True(t, p.ReviewSeenAt.Equal(opsNow))
	require.Equal(t, e.Admin, *p.ReviewSeenBy)
	require.Nil(t, p.ReviewResolvedAt)
	require.Equal(t, 1, e.audits(t, "payment.review_seen", id))
	require.Equal(t, "seen", e.reviewState(t, id))

	e.Clock.Advance(time.Hour)
	p, err = e.Payments.MarkReviewSeen(ctx, e.Admin, id)
	require.NoError(t, err)
	require.True(t, p.ReviewSeenAt.Equal(opsNow), "the first sighting is kept")
	require.Equal(t, 1, e.audits(t, "payment.review_seen", id))

	_, err = e.Payments.MarkReviewSeen(ctx, e.Admin, uuid.Must(uuid.NewV7()))
	requireAppErr(t, err, http.StatusNotFound, "not_found")
}

func TestResolveReview_Overpaid(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	id, _ := e.overpaid(t, "k")

	long := strings.Repeat("ă", 1001)
	_, err := e.Payments.ResolveReview(ctx, e.Admin, id, &long)
	requireAppErr(t, err, http.StatusUnprocessableEntity, "invalid_review_note")

	e.Clock.Advance(time.Hour)
	p, err := e.Payments.ResolveReview(ctx, e.Admin, id, nil)
	require.NoError(t, err)
	require.Equal(t, payment.StatusNeedsReview, p.Status)
	require.True(t, p.ReviewResolvedAt.Equal(opsNow.Add(time.Hour)))
	require.True(t, p.ReviewSeenAt.Equal(opsNow.Add(time.Hour)), "resolving implies seeing")
	require.Equal(t, e.Admin, *p.ReviewResolvedBy)
	require.Nil(t, p.ReviewNote)
	require.Equal(t, "resolved", p.ReviewState)
	require.Equal(t, "resolved", e.reviewState(t, id))
	require.Equal(t, 1, e.audits(t, "payment.review_resolve", id))
	require.Equal(t, "paid", e.str(t, `SELECT status FROM checkouts WHERE id = (SELECT checkout_id FROM payments WHERE id = $1)`, id))
	require.Equal(t, "needs_review", e.str(t, `SELECT status FROM payments WHERE id = $1`, id))

	d, err := e.Payments.AdminGetPayment(ctx, id)
	require.NoError(t, err)
	require.Len(t, d.Events, 1)
	require.Equal(t, "confirmed+overpaid", d.Events[0].Result)
	require.Len(t, d.Refunds, 1)
	require.Equal(t, int64(20_000), d.Refunds[0].Amount)
	require.NotNil(t, d.ReviewResolvedAt)
}

func TestResolveReview_WithNote(t *testing.T) {
	e := newOpsEnv(t)
	id, _ := e.overpaid(t, "k")
	note := "  Đã gọi khách, hoàn 20.000 "
	p, err := e.Payments.ResolveReview(context.Background(), e.Admin, id, &note)
	require.NoError(t, err)
	require.Equal(t, "Đã gọi khách, hoàn 20.000", *p.ReviewNote)
}

func TestResolveReview_NotInReview409(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	_, oc := e.book(t, "k", opsVisitOver48h)
	e.pay(t, oc, opsGross)
	id := e.paymentID(t, oc)
	_, err := e.Payments.ResolveReview(ctx, e.Admin, id, nil)
	requireAppErr(t, err, http.StatusConflict, "payment_not_in_review")
	_, err = e.Payments.MarkReviewSeen(ctx, e.Admin, id)
	requireAppErr(t, err, http.StatusConflict, "payment_not_in_review")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM admin_audit_log WHERE target_id = $1`, id))
}

func TestResolveReview_Twice409(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	id, _ := e.overpaid(t, "k")
	_, err := e.Payments.ResolveReview(ctx, e.Admin, id, nil)
	require.NoError(t, err)
	_, err = e.Payments.ResolveReview(ctx, e.Admin, id, nil)
	requireAppErr(t, err, http.StatusConflict, "payment_not_in_review")
	require.Equal(t, 1, e.audits(t, "payment.review_resolve", id))
	// Seeing a resolved payment again changes nothing.
	p, err := e.Payments.MarkReviewSeen(ctx, e.Admin, id)
	require.NoError(t, err)
	require.Equal(t, "resolved", p.ReviewState)
	require.Equal(t, 0, e.audits(t, "payment.review_seen", id))
}

// New money on a reviewed payment (a duplicate the provider's status does
// not confirm) reopens the review from scratch.
func TestNewTransferReopensReview(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	id, oc := e.overpaid(t, "k")
	_, err := e.Payments.MarkReviewSeen(ctx, e.Admin, id)
	require.NoError(t, err)
	_, err = e.Payments.ResolveReview(ctx, e.Admin, id, nil)
	require.NoError(t, err)

	body := e.Fake.MarkPaidRef(oc, opsGross, "SECOND-REF")
	e.Fake.SetState(oc, payment.StatePending)
	e.Fake.SetAmountPaid(oc, 0)
	requireHTTPCode(t, e.webhook(t, body), http.StatusServiceUnavailable, "payment_not_confirmed")
	e.Clock.Advance(31 * time.Minute)
	rec := e.webhook(t, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE result = 'duplicate_refunded'`))

	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments WHERE id = $1 AND status = 'needs_review'
		AND review_seen_at IS NULL AND review_seen_by IS NULL AND review_resolved_at IS NULL
		AND review_resolved_by IS NULL AND review_note IS NULL`, id))
	require.Equal(t, "new", e.reviewState(t, id))
}

// A plain duplicate transfer on a resolved needs_review payment (more money
// to refund) reopens the review too; the status stays needs_review.
func TestDuplicateOnResolvedReviewReopens(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	id, oc := e.overpaid(t, "k")
	_, err := e.Payments.ResolveReview(ctx, e.Admin, id, nil)
	require.NoError(t, err)

	rec := e.webhook(t, e.Fake.MarkPaidRef(oc, opsGross, "SECOND-REF"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE result = 'duplicate_refunded'`))
	require.Equal(t, "new", e.reviewState(t, id))
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM refunds WHERE payment_id = $1`, id))
}

// Ruling 1: a needs_review payment an admin has seen still takes a later
// full payment: it becomes paid (the review columns are cleared, no 23514).
func TestSeenUnderpaidThenFullPaymentConfirms(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	v, oc := e.book(t, "k", opsVisitOver48h)
	e.pay(t, oc, 100_000)
	id := e.paymentID(t, oc)
	_, err := e.Payments.MarkReviewSeen(ctx, e.Admin, id)
	require.NoError(t, err)

	rec := e.webhook(t, e.Fake.MarkPaidRef(oc, opsGross, "FULL-REF"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "confirmed", e.str(t, `SELECT status FROM bookings WHERE id = $1`, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments WHERE id = $1 AND status = 'paid'
		AND review_seen_at IS NULL AND review_resolved_at IS NULL`, id))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries WHERE kind = 'booking_earning'`))
	require.NoError(t, e.Ledger.Verify(ctx, e.PartnerID))
}

func TestAdminListPayments_OpenReview(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	review, _ := e.overpaid(t, "a")
	_, oc := e.book(t, "b", opsVisitOver48h)
	e.pay(t, oc, opsGross)
	paid := e.paymentID(t, oc)

	all, _, err := e.Payments.AdminListPayments(ctx, payment.PaymentFilter{})
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, paid, all[0].ID, "newest first")
	require.Empty(t, all[0].ReviewState, "only needs_review payments have a review state")

	open, _, err := e.Payments.AdminListPayments(ctx, payment.PaymentFilter{OpenReview: true})
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, review, open[0].ID)
	require.Equal(t, "new", open[0].ReviewState)

	st := payment.StatusPaid
	got, _, err := e.Payments.AdminListPayments(ctx, payment.PaymentFilter{Status: &st})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, paid, got[0].ID)

	tok := e.token(t, e.Admin, auth.RoleSuperAdmin)
	rec := e.do(t, "GET", "/v1/admin/payments?open_review=true", tok, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	require.Equal(t, "new", body.Items[0]["review_state"])
	require.Contains(t, body.Items[0], "checkout_id")

	// Opening the payment (FE) marks it seen; Hoàn thành closes it.
	rec = e.do(t, "POST", "/v1/admin/payments/"+review.String()+"/seen", tok, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = e.do(t, "GET", "/v1/admin/payments/"+review.String(), tok, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var d map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &d))
	require.Equal(t, "seen", d["review_state"])
	require.Len(t, d["events"], 1)
	require.Len(t, d["refunds"], 1)

	requireHTTPCode(t, e.do(t, "POST", "/v1/admin/payments/"+review.String()+"/resolve", tok, `{"bogus":1}`),
		http.StatusBadRequest, "invalid_request")
	rec = e.do(t, "POST", "/v1/admin/payments/"+review.String()+"/resolve", tok, `{"note":"ok"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	open, _, err = e.Payments.AdminListPayments(ctx, payment.PaymentFilter{OpenReview: true})
	require.NoError(t, err)
	require.Empty(t, open)

	requireHTTPCode(t, e.do(t, "POST", "/v1/admin/payments/xyz/seen", tok, ""), http.StatusNotFound, "not_found")
	requireHTTPCode(t, e.do(t, "GET", "/v1/admin/payments/"+uuid.Nil.String(), tok, ""), http.StatusNotFound, "not_found")
	requireHTTPCode(t, e.do(t, "GET", "/v1/admin/payments?open_review=maybe", tok, ""), http.StatusBadRequest, "invalid_request")
	requireHTTPCode(t, e.do(t, "GET", "/v1/admin/payments?status=bogus", tok, ""), http.StatusBadRequest, "invalid_request")
}
