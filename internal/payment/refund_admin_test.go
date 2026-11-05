package payment_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

// proof uploads a PNG with purpose by the admin.
func (e *opsEnv) proof(t *testing.T, purpose string) uuid.UUID {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	f, err := e.Files.Upload(context.Background(), auth.Principal{UserID: e.Admin, Role: auth.RoleSuperAdmin}, purpose, &buf)
	require.NoError(t, err)
	return f.ID
}

func TestCompleteRefund_Pending(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	r := e.cancelledRefund(t, "k", true)
	require.Equal(t, payment.RefundPending, r.Status)
	proof := e.proof(t, "refund_proof")
	before := e.emailsTo(t, customerEmail)

	_, err := e.Payments.CompleteRefund(ctx, e.Admin, r.ID, payment.CompleteRefundInput{TransferReference: "   "})
	requireAppErr(t, err, http.StatusUnprocessableEntity, "invalid_transfer_reference")
	_, err = e.Payments.CompleteRefund(ctx, e.Admin, r.ID, payment.CompleteRefundInput{TransferReference: strings.Repeat("x", 101)})
	requireAppErr(t, err, http.StatusUnprocessableEntity, "invalid_transfer_reference")

	e.Clock.Advance(time.Hour)
	out, err := e.Payments.CompleteRefund(ctx, e.Admin, r.ID, payment.CompleteRefundInput{
		TransferReference: "  FT26110512345  ", ProofFileID: &proof,
	})
	require.NoError(t, err)
	require.Equal(t, payment.RefundCompleted, out.Status)
	require.Equal(t, "FT26110512345", *out.TransferReference)
	require.Equal(t, e.Admin, *out.CompletedBy)
	require.Equal(t, proof, *out.ProofFileID)
	require.True(t, out.CompletedAt.Equal(opsNow.Add(time.Hour)))
	require.Equal(t, 1, e.audits(t, "refund.complete", r.ID))
	require.Equal(t, before+1, e.emailsTo(t, customerEmail))
	text := e.str(t, `SELECT args->>'text' FROM river_job WHERE kind = 'send_email' AND args->>'to' = $1
		ORDER BY id DESC LIMIT 1`, customerEmail)
	require.Contains(t, text, "Khoản hoàn 380.000đ đã được chuyển, mã giao dịch FT26110512345")

	got, err := e.Payments.AdminGetRefund(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, payment.RefundCompleted, got.Status)
}

func TestCompleteRefund_AwaitingBankInfo409(t *testing.T) {
	e := newOpsEnv(t)
	r := e.cancelledRefund(t, "k", false)
	require.Equal(t, payment.RefundAwaitingBankInfo, r.Status)
	_, err := e.Payments.CompleteRefund(context.Background(), e.Admin, r.ID, payment.CompleteRefundInput{TransferReference: "FT1"})
	requireAppErr(t, err, http.StatusConflict, "refund_awaiting_bank_info")
	require.Equal(t, 0, e.audits(t, "refund.complete", r.ID))
}

func TestCompleteRefund_Twice409(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	r := e.cancelledRefund(t, "k", true)
	_, err := e.Payments.CompleteRefund(ctx, e.Admin, r.ID, payment.CompleteRefundInput{TransferReference: "FT1"})
	require.NoError(t, err)
	_, err = e.Payments.CompleteRefund(ctx, e.Admin, r.ID, payment.CompleteRefundInput{TransferReference: "FT2"})
	requireAppErr(t, err, http.StatusConflict, "refund_already_completed")
	require.Equal(t, 1, e.audits(t, "refund.complete", r.ID))

	_, err = e.Payments.CompleteRefund(ctx, e.Admin, uuid.Must(uuid.NewV7()), payment.CompleteRefundInput{TransferReference: "FT3"})
	requireAppErr(t, err, http.StatusNotFound, "not_found")
}

func TestCompleteRefund_ProofWrongPurpose422(t *testing.T) {
	e := newOpsEnv(t)
	r := e.cancelledRefund(t, "k", true)
	wrong := e.proof(t, "payout_proof")
	_, err := e.Payments.CompleteRefund(context.Background(), e.Admin, r.ID, payment.CompleteRefundInput{
		TransferReference: "FT1", ProofFileID: &wrong,
	})
	requireAppErr(t, err, http.StatusUnprocessableEntity, "invalid_file")
	got, err := e.Payments.AdminGetRefund(context.Background(), r.ID)
	require.NoError(t, err)
	require.Equal(t, payment.RefundPending, got.Status)
}

func TestCompleteRefund_HTTP(t *testing.T) {
	e := newOpsEnv(t)
	r := e.cancelledRefund(t, "k", true)
	tok := e.token(t, e.Admin, auth.RoleSuperAdmin)
	path := "/v1/admin/refunds/" + r.ID.String() + "/complete"

	requireHTTPCode(t, e.do(t, "POST", path, e.token(t, e.Customer, auth.RoleCustomer), `{"transfer_reference":"FT1"}`),
		http.StatusForbidden, "forbidden")
	requireHTTPCode(t, e.do(t, "POST", "/v1/admin/refunds/not-a-uuid/complete", tok, `{"transfer_reference":"FT1"}`),
		http.StatusNotFound, "not_found")
	requireHTTPCode(t, e.do(t, "POST", path, tok, `{"transfer_reference":"FT1","x":1}`), http.StatusBadRequest, "invalid_request")
	requireHTTPCode(t, e.do(t, "POST", path, tok, `{"transfer_reference":""}`), http.StatusUnprocessableEntity, "invalid_transfer_reference")
	rec := e.do(t, "POST", path, tok, `{"transfer_reference":"FT1"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "completed", got["status"])
	require.Equal(t, e.Admin.String(), got["completed_by"])
	require.Contains(t, got, "checkout_id")

	rec = e.do(t, "GET", "/v1/admin/refunds/"+r.ID.String(), tok, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireHTTPCode(t, e.do(t, "GET", "/v1/admin/refunds/"+uuid.Nil.String(), tok, ""), http.StatusNotFound, "not_found")
}

func TestAdminListRefunds_FilterStatusReason(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	// An underpayment without saved account: payment_mismatch, awaiting.
	_, oc := e.book(t, "under", opsVisitOver48h)
	e.pay(t, oc, 100_000)
	// Then a cancellation with the account saved: customer_cancel, pending.
	cancelled := e.cancelledRefund(t, "k", true)

	all, next, err := e.Payments.AdminListRefunds(ctx, payment.RefundFilter{})
	require.NoError(t, err)
	require.Nil(t, next)
	require.Len(t, all, 2)
	require.Equal(t, cancelled.ID, all[0].ID, "newest first")

	str := func(s string) *string { return &s }
	got, _, err := e.Payments.AdminListRefunds(ctx, payment.RefundFilter{Status: str(payment.RefundPending)})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, payment.RefundCustomerCancel, got[0].Reason)

	got, _, err = e.Payments.AdminListRefunds(ctx, payment.RefundFilter{Reason: str(payment.RefundPaymentMismatch)})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, payment.RefundAwaitingBankInfo, got[0].Status)
	require.Equal(t, int64(100_000), got[0].Amount)
	require.Equal(t, "FAKE-REF-"+itoa(oc), *got[0].ProviderReference)

	got, _, err = e.Payments.AdminListRefunds(ctx, payment.RefundFilter{
		Status: str(payment.RefundAwaitingBankInfo), Reason: str(payment.RefundCustomerCancel),
	})
	require.NoError(t, err)
	require.Empty(t, got)

	pid := e.paymentID(t, oc)
	got, _, err = e.Payments.AdminListRefunds(ctx, payment.RefundFilter{PaymentID: &pid})
	require.NoError(t, err)
	require.Len(t, got, 1)
	got, _, err = e.Payments.AdminListRefunds(ctx, payment.RefundFilter{BookingID: cancelled.BookingID})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, cancelled.ID, got[0].ID)

	page, next, err := e.Payments.AdminListRefunds(ctx, payment.RefundFilter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.NotNil(t, next)
	page2, next2, err := e.Payments.AdminListRefunds(ctx, payment.RefundFilter{Limit: 1, Cursor: next})
	require.NoError(t, err)
	require.Nil(t, next2)
	require.Equal(t, all[1].ID, page2[0].ID)

	_, _, err = e.Payments.AdminListRefunds(ctx, payment.RefundFilter{Status: str("bogus")})
	requireAppErr(t, err, http.StatusBadRequest, "invalid_request")

	tok := e.token(t, e.Admin, auth.RoleSuperAdmin)
	rec := e.do(t, "GET", "/v1/admin/refunds?status=pending&reason=customer_cancel", tok, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Items      []map[string]any `json:"items"`
		NextCursor *uuid.UUID       `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	require.Equal(t, cancelled.ID.String(), body.Items[0]["id"])
	for _, k := range []string{"checkout_id", "provider_reference", "proof_file_id", "completed_by"} {
		require.Contains(t, body.Items[0], k)
	}
	requireHTTPCode(t, e.do(t, "GET", "/v1/admin/refunds?booking_id=nope", tok, ""), http.StatusBadRequest, "invalid_request")
	requireHTTPCode(t, e.do(t, "GET", "/v1/admin/refunds?limit=0", tok, ""), http.StatusBadRequest, "invalid_request")
}

// Every 3 days at most, at most 5 times, only while awaiting_bank_info.
func TestRemindAwaitingBankInfo_Cadence(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	r := e.cancelledRefund(t, "a", false)
	other := e.cancelledRefund(t, "b", false)
	require.Equal(t, payment.RefundAwaitingBankInfo, other.Status)
	require.NoError(t, e.Payments.SetRefundBankAccount(ctx, e.Customer, other.ID, payment.BankAccount{
		BankBIN: "970436", AccountNumber: "0011001234567", AccountHolder: "Nguyen Van An",
	}))

	base := e.emailsTo(t, customerEmail)
	day := 24 * time.Hour
	step := func(at time.Duration, want int) {
		t.Helper()
		e.Clock.T = opsNow.Add(at)
		before := e.emailsTo(t, customerEmail)
		n, err := e.Payments.RemindAwaitingBankInfo(ctx)
		require.NoError(t, err)
		require.Equal(t, want, n, "at +%s", at)
		require.Equal(t, before+want, e.emailsTo(t, customerEmail), "at +%s", at)
	}
	step(2*day, 0)
	step(3*day, 1)
	step(4*day, 0)
	step(6*day, 1)
	step(9*day, 1)
	step(12*day, 1)
	step(15*day, 1)
	step(18*day, 0) // five reminders sent
	step(30*day, 0)
	require.Equal(t, base+5, e.emailsTo(t, customerEmail))
	require.Equal(t, 5, e.count(t, `SELECT bank_info_reminder_count FROM refunds WHERE id = $1`, r.ID))
	require.Equal(t, 0, e.count(t, `SELECT bank_info_reminder_count FROM refunds WHERE id = $1`, other.ID))
}

// The daily job does not start at exactly the same second every day: a run
// a few seconds (or minutes) earlier than 3 days after the previous reminder
// still sends the next one, instead of pushing it to day 4. A run one day
// early still sends nothing.
func TestRemindAwaitingBankInfo_ToleratesJobJitter(t *testing.T) {
	e := newOpsEnv(t)
	ctx := context.Background()
	e.cancelledRefund(t, "a", false)
	day := 24 * time.Hour
	run := func(at time.Duration) int {
		t.Helper()
		e.Clock.T = opsNow.Add(at)
		n, err := e.Payments.RemindAwaitingBankInfo(ctx)
		require.NoError(t, err)
		return n
	}
	require.Equal(t, 0, run(2*day))
	require.Equal(t, 1, run(3*day+7*time.Second), "first reminder, the job started late")
	require.Equal(t, 0, run(4*day))
	require.Equal(t, 1, run(6*day+2*time.Second), "3 days minus 5s after the previous reminder")
	require.Equal(t, 1, run(9*day-10*time.Minute), "3 days minus ~10 minutes")
	require.Equal(t, 0, run(11*day), "one day early")
}
