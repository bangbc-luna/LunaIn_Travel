package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/payout"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

// statementSum is Σ amount of statement lines.
func statementSum(lines []ledger.StatementLine) int64 {
	var sum int64
	for _, l := range lines {
		sum += l.Amount
	}
	return sum
}

// TestE2E_Payout runs the payout flow through the real mux with
// payment.Fake: two completed and released bookings, a payout request with
// its statement, admin approve and mark-paid with a proof, a clean
// reconciliation, then a negative manual adjustment.
func TestE2E_Payout(t *testing.T) {
	// Thursday 2026-11-05 09:00 in Vietnam.
	a := newE2EApp(t, time.Date(2026, 11, 5, 9, 0, 0, 0, clock.VN))
	ctx := context.Background()

	// 1. Commission 10%, partner, and the partner's bank account.
	w := a.setupWorld(t)
	a.call(t, "PUT", "/v1/partner/bank-account", w.pt, map[string]any{
		"bank_bin": "970436", "bank_name": "Vietcombank",
		"account_number": "0123456789", "account_holder": "Nguyễn Văn Bình",
	}, http.StatusOK, nil)

	// 2. Two bookings of 380.000 each, paid, checked in, released. The visit
	// is the next day so the payments are still inside the reconciliation's
	// 72h window at step 8 (created 11-05 09:00, run 11-07 09:30).
	items := []map[string]any{{"ticket_type_id": w.adult.ID, "quantity": 2}, {"ticket_type_id": w.child.ID, "quantity": 1}}
	const gross, commission = int64(380_000), int64(38_000)
	var views [2]booking.BookingView
	for i, key := range []string{"payout-1", "payout-2"} {
		v := a.book(t, w, key, "2026-11-06", items)
		require.Equal(t, gross, v.GrossAmount)
		a.call(t, "POST", "/v1/dev/payments/"+orderCode(t, v)+"/simulate-paid", "", map[string]any{}, http.StatusOK, nil)
		a.call(t, "GET", "/v1/bookings/"+v.ID.String(), w.ct, nil, http.StatusOK, &views[i])
		require.Equal(t, booking.StatusConfirmed, views[i].Status)
	}
	a.clock.T = time.Date(2026, 11, 6, 8, 30, 0, 0, clock.VN)
	a.relogin(t, w)
	for _, v := range views {
		a.call(t, "POST", "/v1/partner/check-ins", w.pt, map[string]string{"qr_token": *v.QRToken}, http.StatusOK, nil)
	}
	a.clock.T = a.clock.T.Add(25 * time.Hour) // release_delay_days = 1
	a.relogin(t, w)
	require.NoError(t, a.release.Work(ctx, nil))
	const available = 2 * (gross - commission) // 684.000
	require.Equal(t, ledger.Balance{Available: available}, a.balance(t, w.pt))

	// 3. Preview: allowed, the statement starts at 0 and sums to 684.000.
	var pv payout.Preview
	a.call(t, "GET", "/v1/partner/payouts/preview", w.pt, nil, http.StatusOK, &pv)
	require.True(t, pv.CanRequest)
	require.Equal(t, int64(0), pv.OpeningAvailable)
	require.Equal(t, available, pv.Available)
	require.Equal(t, available, statementSum(pv.Statement))

	// 4. Request 500.000: the hold leaves 184.000.
	var po payout.PayoutView
	a.call(t, "POST", "/v1/partner/payouts", w.pt, map[string]any{"amount": 500_000}, http.StatusCreated, &po)
	require.Equal(t, "requested", po.Status)
	require.Equal(t, int64(500_000), po.Amount)
	require.Equal(t, int64(184_000), po.ClosingAvailable)
	require.Equal(t, "NGUYEN VAN BINH", po.Bank.AccountHolder)

	// 5. A second request while one is open is a conflict.
	var conflict struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	a.call(t, "POST", "/v1/partner/payouts", w.pt, map[string]any{"amount": 100_000}, http.StatusConflict, &conflict)
	require.Equal(t, "payout_already_open", conflict.Error.Code)

	// 6. Admin approves, uploads the transfer proof and marks it paid.
	pp := "/v1/admin/payouts/" + po.ID.String()
	a.call(t, "POST", pp+"/approve", w.admin, nil, http.StatusOK, nil)
	proof := a.uploadPNG(t, w.admin, "payout_proof")
	a.call(t, "POST", pp+"/mark-paid", w.admin, map[string]any{
		"transfer_reference": "FT26311000001", "proof_file_id": proof,
	}, http.StatusOK, nil)

	// 7. The partner sees it paid with a consistent statement.
	a.call(t, "GET", "/v1/partner/payouts/"+po.ID.String(), w.pt, nil, http.StatusOK, &po)
	require.Equal(t, "paid", po.Status)
	require.NotNil(t, po.PaidAt)
	require.Equal(t, po.ClosingAvailable, po.OpeningAvailable+statementSum(po.Statement))
	require.Equal(t, ledger.Balance{Available: 184_000}, a.balance(t, w.pt))

	// 8. The reconciliation checks both payments with the provider and
	// finds nothing wrong.
	var rec payment.Reconciliation
	a.call(t, "POST", "/v1/admin/reconciliations/run", w.admin, nil, http.StatusOK, &rec)
	require.Equal(t, "ok", rec.Status, "%+v", rec.Mismatches)
	require.Equal(t, 2, rec.CheckedPayments)
	require.Empty(t, rec.Mismatches)

	// 9. A −200.000 adjustment takes available below zero: no payout possible.
	a.call(t, "POST", "/v1/admin/partners/"+w.partner.ID.String()+"/adjustments", w.admin, map[string]any{
		"amount": -200_000, "reason": "Thu hồi khoản đối soát sai",
	}, http.StatusCreated, nil)
	require.Equal(t, ledger.Balance{Available: -16_000}, a.balance(t, w.pt))
	a.call(t, "GET", "/v1/partner/payouts/preview", w.pt, nil, http.StatusOK, &pv)
	require.False(t, pv.CanRequest)
	require.Equal(t, int64(-16_000), pv.Available)

	require.NoError(t, a.ledger.Verify(ctx, w.partner.ID))
}
