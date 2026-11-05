package payout

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

// uploadProof uploads a payout_proof PDF as user (a Super Admin) and
// returns its file id.
func (e *env) uploadProof(t *testing.T, user uuid.UUID, purpose string) uuid.UUID {
	t.Helper()
	f, err := e.files.Upload(context.Background(), auth.Principal{UserID: user, Role: auth.RoleSuperAdmin}, purpose,
		strings.NewReader("%PDF-1.4\n"+strings.Repeat("x", 100)))
	require.NoError(t, err)
	return f.ID
}

func (e *env) newAdmin(t *testing.T, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	e.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = e.identity.CreateInternalUser(ctx, tx, email, "adminpass123", auth.RoleSuperAdmin)
		return err
	})
	return id
}

func emailCount(t *testing.T, e *env) int {
	t.Helper()
	return countWhere(t, e, `SELECT count(*) FROM river_job WHERE kind = 'send_email'`)
}

func auditCount(t *testing.T, e *env, action string, payoutID uuid.UUID) int {
	t.Helper()
	return countWhere(t, e, `SELECT count(*) FROM admin_audit_log WHERE action = $1 AND target_id = $2`, action, payoutID)
}

func reversalCount(t *testing.T, e *env, payoutID uuid.UUID) int {
	t.Helper()
	return countWhere(t, e, `SELECT count(*) FROM ledger_entries WHERE payout_id = $1 AND kind = 'payout_reversal'`, payoutID)
}

func TestApproveThenMarkPaid(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	e.requireAvailable(t, p, 600_000)
	entriesBefore := countWhere(t, e, `SELECT count(*) FROM ledger_entries WHERE partner_id = $1`, p.ID)

	ap, err := e.svc.Approve(ctx, e.admin, v.ID)
	require.NoError(t, err)
	require.Equal(t, StatusApproved, ap.Status)
	require.NotNil(t, ap.ApprovedAt)
	require.True(t, ap.ApprovedAt.Equal(testNow))
	require.Equal(t, 1, auditCount(t, e, "payout.approve", v.ID))
	require.Equal(t, 1, emailCount(t, e))
	require.Equal(t, 1, countWhere(t, e, `SELECT count(*) FROM river_job
		WHERE kind = 'send_email' AND args->>'to' = 'owner@alpha.vn' AND args->>'subject' = 'Yêu cầu rút tiền đã được duyệt'`))

	// Approving twice is a state error.
	_, err = e.svc.Approve(ctx, e.admin, v.ID)
	requireCode(t, err, http.StatusConflict, "invalid_payout_state")

	proof := e.uploadProof(t, e.admin, "payout_proof")
	paid, err := e.svc.MarkPaid(ctx, e.admin, v.ID, MarkPaidInput{TransferReference: "  FT2611050001  ", ProofFileID: proof})
	require.NoError(t, err)
	require.Equal(t, StatusPaid, paid.Status)
	require.NotNil(t, paid.PaidAt)
	require.Equal(t, "FT2611050001", *paid.TransferReference)

	var paidBy, approvedBy, proofID uuid.UUID
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT paid_by, approved_by, proof_file_id FROM payouts WHERE id = $1`, v.ID).
		Scan(&paidBy, &approvedBy, &proofID))
	require.Equal(t, e.admin, paidBy)
	require.Equal(t, e.admin, approvedBy)
	require.Equal(t, proof, proofID)

	require.Equal(t, 1, auditCount(t, e, "payout.mark_paid", v.ID))
	require.Equal(t, 2, countWhere(t, e, `SELECT count(*) FROM admin_audit_log WHERE target_id = $1`, v.ID))
	require.Equal(t, 1, countWhere(t, e, `SELECT count(*) FROM admin_audit_log
		WHERE action = 'payout.mark_paid' AND after->>'transfer_reference' = 'FT2611050001'
		  AND after->>'proof_file_id' = $1 AND after->'bank'->>'account_number' = '0011001234567'`, proof.String()))

	// No ledger entry: the money left available at hold time.
	require.Equal(t, entriesBefore, countWhere(t, e, `SELECT count(*) FROM ledger_entries WHERE partner_id = $1`, p.ID))
	e.requireAvailable(t, p, 600_000)

	require.Equal(t, 2, emailCount(t, e))
	require.Equal(t, 1, countWhere(t, e, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'owner@alpha.vn' AND args->>'text' LIKE '%300.000 đ%'
		AND args->>'text' LIKE '%FT2611050001%' AND args->>'text' LIKE '%0011001234567%'`))

	// Paid is final.
	_, err = e.svc.MarkPaid(ctx, e.admin, v.ID, MarkPaidInput{TransferReference: "FT2", ProofFileID: proof})
	requireCode(t, err, http.StatusConflict, "invalid_payout_state")
	_, err = e.svc.Approve(ctx, e.admin, v.ID)
	requireCode(t, err, http.StatusConflict, "invalid_payout_state")

	// The admin view shows the proof and the balance.
	av, err := e.svc.AdminGet(ctx, v.ID)
	require.NoError(t, err)
	require.Equal(t, StatusPaid, av.Status)
	require.NotNil(t, av.ProofFileID)
	require.Equal(t, proof, *av.ProofFileID)
	require.Equal(t, int64(600_000), av.Balance.Available)
	require.False(t, av.CurrentBankDiffers)
	requireBalanced(t, av.PayoutView)
}

func TestApprove_NotFound404(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.Approve(context.Background(), e.admin, uuid.Must(uuid.NewV7()))
	requireCode(t, err, http.StatusNotFound, "payout_not_found")
	_, err = e.svc.AdminGet(context.Background(), uuid.Must(uuid.NewV7()))
	requireCode(t, err, http.StatusNotFound, "payout_not_found")
}

func TestMarkPaid_RequiresApproved409(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	proof := e.uploadProof(t, e.admin, "payout_proof")

	_, err = e.svc.MarkPaid(ctx, e.admin, v.ID, MarkPaidInput{TransferReference: "FT1", ProofFileID: proof})
	requireCode(t, err, http.StatusConflict, "invalid_payout_state")
	require.Equal(t, StatusRequested, e.statusOf(t, v.ID))
	require.Equal(t, 0, emailCount(t, e))
	require.Equal(t, 0, auditCount(t, e, "payout.mark_paid", v.ID))

	// Validation comes first.
	_, err = e.svc.MarkPaid(ctx, e.admin, v.ID, MarkPaidInput{TransferReference: "   ", ProofFileID: proof})
	requireCode(t, err, http.StatusUnprocessableEntity, "invalid_transfer_reference")
	_, err = e.svc.MarkPaid(ctx, e.admin, v.ID, MarkPaidInput{TransferReference: strings.Repeat("Đ", 101), ProofFileID: proof})
	requireCode(t, err, http.StatusUnprocessableEntity, "invalid_transfer_reference")
	_, err = e.svc.MarkPaid(ctx, e.admin, v.ID, MarkPaidInput{TransferReference: "FT1"})
	requireCode(t, err, http.StatusUnprocessableEntity, "proof_file_required")

	// 100 runes are fine.
	_, err = e.svc.Approve(ctx, e.admin, v.ID)
	require.NoError(t, err)
	_, err = e.svc.MarkPaid(ctx, e.admin, v.ID, MarkPaidInput{TransferReference: strings.Repeat("Đ", 100), ProofFileID: proof})
	require.NoError(t, err)
}

func TestMarkPaid_ProofOfAnotherAdminOrPurpose422(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	_, err = e.svc.Approve(ctx, e.admin, v.ID)
	require.NoError(t, err)

	other := e.newAdmin(t, "admin2@lunain.vn")
	cases := map[string]uuid.UUID{
		"another admin":   e.uploadProof(t, other, "payout_proof"),
		"another purpose": e.uploadProof(t, e.admin, "refund_proof"),
		"unknown file":    uuid.Must(uuid.NewV7()),
	}
	for name, proof := range cases {
		_, err := e.svc.MarkPaid(ctx, e.admin, v.ID, MarkPaidInput{TransferReference: "FT1", ProofFileID: proof})
		requireCode(t, err, http.StatusUnprocessableEntity, "invalid_file")
		require.Equal(t, StatusApproved, e.statusOf(t, v.ID), name)
	}
	require.Equal(t, 0, auditCount(t, e, "payout.mark_paid", v.ID))
	require.Equal(t, 1, emailCount(t, e)) // the approval only
}

func (e *env) statusOf(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var st string
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT status FROM payouts WHERE id = $1`, id).Scan(&st))
	return st
}

func TestReject_FromRequestedAndApproved(t *testing.T) {
	for _, approveFirst := range []bool{false, true} {
		name := "requested"
		if approveFirst {
			name = "approved"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			p := e.fundedPartner(t, "alpha")
			v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
			require.NoError(t, err)
			if approveFirst {
				_, err = e.svc.Approve(ctx, e.admin, v.ID)
				require.NoError(t, err)
			}
			emails := emailCount(t, e)

			// Reason validation.
			_, err = e.svc.Reject(ctx, e.admin, v.ID, "   ")
			requireCode(t, err, http.StatusUnprocessableEntity, "invalid_reject_reason")
			_, err = e.svc.Reject(ctx, e.admin, v.ID, strings.Repeat("a", 501))
			requireCode(t, err, http.StatusUnprocessableEntity, "invalid_reject_reason")

			rj, err := e.svc.Reject(ctx, e.admin, v.ID, "  Sai thông tin tài khoản  ")
			require.NoError(t, err)
			require.Equal(t, StatusRejected, rj.Status)
			require.NotNil(t, rj.RejectedAt)
			require.Equal(t, "Sai thông tin tài khoản", *rj.RejectReason)
			e.requireAvailable(t, p, 900_000)
			require.Equal(t, 1, reversalCount(t, e, v.ID))
			require.Equal(t, 1, countWhere(t, e, `SELECT count(*) FROM admin_audit_log
				WHERE action = 'payout.reject' AND target_id = $1 AND reason = 'Sai thông tin tài khoản'`, v.ID))
			require.Equal(t, emails+1, emailCount(t, e))
			require.Equal(t, 1, countWhere(t, e, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
				AND args->>'to' = 'owner@alpha.vn' AND args->>'text' LIKE '%Sai thông tin tài khoản%'`))

			// A new payout can be requested; its statement starts after
			// the rejected one's.
			nv, err := e.svc.Request(ctx, p.ID, p.Owner, 900_000)
			require.NoError(t, err)
			require.Equal(t, v.StatementSeqTo+1, nv.StatementSeqFrom)
			requireBalanced(t, nv)
		})
	}
}

func TestReject_AfterPaid409(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	_, err = e.svc.Approve(ctx, e.admin, v.ID)
	require.NoError(t, err)
	_, err = e.svc.MarkPaid(ctx, e.admin, v.ID, MarkPaidInput{TransferReference: "FT1", ProofFileID: e.uploadProof(t, e.admin, "payout_proof")})
	require.NoError(t, err)

	_, err = e.svc.Reject(ctx, e.admin, v.ID, "nhầm")
	requireCode(t, err, http.StatusConflict, "invalid_payout_state")
	require.Equal(t, StatusPaid, e.statusOf(t, v.ID))
	require.Equal(t, 0, reversalCount(t, e, v.ID))
	e.requireAvailable(t, p, 600_000)
}

// Review Focus 3: rejecting after available went negative gives the whole
// amount back.
func TestReject_AfterBalanceWentNegative(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.newPartner(t, "alpha", true)
	e.fund(t, p, 500_000)
	e.requireAvailable(t, p, 500_000)
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 500_000)
	require.NoError(t, err)
	e.requireAvailable(t, p, 0)

	// Another booking (gross 1.000.000, commission 100.000) is cancelled
	// with a 100% refund: its net is −100.000, then released.
	b := e.confirmBookingWithCommission(t, p, 1_000_000, 100_000)
	e.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM bookings WHERE id = $1 FOR UPDATE`, b.BookingID); err != nil {
			return err
		}
		return e.ledger.PostRefund(ctx, tx, b.BookingID, p.ID, 1_000_000)
	})
	e.release(t, b)
	e.requireAvailable(t, p, -100_000)

	_, err = e.svc.Reject(ctx, e.admin, v.ID, "Đối tác yêu cầu hủy")
	require.NoError(t, err)
	var amount int64
	require.NoError(t, e.pool.QueryRow(ctx,
		`SELECT amount FROM ledger_entries WHERE payout_id = $1 AND kind = 'payout_reversal'`, v.ID).Scan(&amount))
	require.Equal(t, int64(500_000), amount)
	e.requireAvailable(t, p, 400_000)

	nv, err := e.svc.Request(ctx, p.ID, p.Owner, 400_000)
	require.NoError(t, err)
	requireBalanced(t, nv)
	e.requireAvailable(t, p, 0)
}

func TestReject_Twice409NoSecondReversal(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	_, err = e.svc.Reject(ctx, e.admin, v.ID, "lần 1")
	require.NoError(t, err)
	_, err = e.svc.Reject(ctx, e.admin, v.ID, "lần 2")
	requireCode(t, err, http.StatusConflict, "invalid_payout_state")
	_, err = e.svc.Approve(ctx, e.admin, v.ID)
	requireCode(t, err, http.StatusConflict, "invalid_payout_state")
	require.Equal(t, 1, reversalCount(t, e, v.ID))
	require.Equal(t, 1, auditCount(t, e, "payout.reject", v.ID))
	e.requireAvailable(t, p, 900_000)
}

// Concurrent rejects of one payout: one wins, the other is 409, one reversal.
func TestReject_Concurrent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := e.svc.Reject(ctx, e.admin, v.ID, "song song")
			errs <- err
		}()
	}
	var ok, conflict int
	for range 2 {
		if err := <-errs; err == nil {
			ok++
		} else {
			requireCode(t, err, http.StatusConflict, "invalid_payout_state")
			conflict++
		}
	}
	require.Equal(t, 1, ok)
	require.Equal(t, 1, conflict)
	require.Equal(t, 1, reversalCount(t, e, v.ID))
	e.requireAvailable(t, p, 900_000)
}

// Review Focus 5: the partner changes its bank account while a payout is
// open; the payout keeps (and is paid to) the snapshot.
func TestBankChangedWhileOpen(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)

	av, err := e.svc.AdminGet(ctx, v.ID)
	require.NoError(t, err)
	require.False(t, av.CurrentBankDiffers)

	auditsBefore := countWhere(t, e, `SELECT count(*) FROM admin_audit_log
		WHERE action = 'partner.bank_account.set' AND target_id = $1`, p.ID)
	_, err = e.partners.SetBankAccount(ctx, p.ID, p.Owner, partner.BankAccountInput{
		BankBIN: "970415", BankName: "VietinBank", AccountNumber: "1029384756", AccountHolder: "Trần Thị Bình",
	})
	require.NoError(t, err)
	require.Equal(t, auditsBefore+1, countWhere(t, e, `SELECT count(*) FROM admin_audit_log
		WHERE action = 'partner.bank_account.set' AND target_id = $1 AND actor_user_id = $2`, p.ID, p.Owner))

	av, err = e.svc.AdminGet(ctx, v.ID)
	require.NoError(t, err)
	require.True(t, av.CurrentBankDiffers)
	require.NotNil(t, av.CurrentBank)
	require.Equal(t, "1029384756", av.CurrentBank.AccountNumber)
	require.Equal(t, v.Bank, av.Bank)
	require.Equal(t, "0011001234567", av.Bank.AccountNumber)

	_, err = e.svc.Approve(ctx, e.admin, v.ID)
	require.NoError(t, err)
	paid, err := e.svc.MarkPaid(ctx, e.admin, v.ID, MarkPaidInput{TransferReference: "FT9", ProofFileID: e.uploadProof(t, e.admin, "payout_proof")})
	require.NoError(t, err)
	require.Equal(t, v.Bank, paid.Bank)
	require.Equal(t, 1, countWhere(t, e, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'text' LIKE '%FT9%' AND args->>'text' LIKE '%0011001234567%'
		AND args->>'text' NOT LIKE '%1029384756%'`))
}

// Only the BIN and the account number count; a new row with the same
// account is not a difference.
func TestCurrentBankDiffers_SameAccountNewRow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	_, err = e.partners.SetBankAccount(ctx, p.ID, p.Owner, partner.BankAccountInput{
		BankBIN: "970436", BankName: "Ngân hàng Vietcombank", AccountNumber: "0011001234567", AccountHolder: "Nguyễn Văn An",
	})
	require.NoError(t, err)
	av, err := e.svc.AdminGet(ctx, v.ID)
	require.NoError(t, err)
	require.False(t, av.CurrentBankDiffers)

	_, err = e.partners.SetBankAccount(ctx, p.ID, p.Owner, partner.BankAccountInput{
		BankBIN: "970415", BankName: "VietinBank", AccountNumber: "0011001234567", AccountHolder: "Nguyễn Văn An",
	})
	require.NoError(t, err)
	av, err = e.svc.AdminGet(ctx, v.ID)
	require.NoError(t, err)
	require.True(t, av.CurrentBankDiffers)
}

func TestSuspendedPartnerPayoutStillProcessable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	q := e.fundedPartner(t, "beta")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	w, err := e.svc.Request(ctx, q.ID, q.Owner, 200_000)
	require.NoError(t, err)
	require.NoError(t, e.partners.Suspend(ctx, e.admin, p.ID, "vi phạm"))
	require.NoError(t, e.partners.Suspend(ctx, e.admin, q.ID, "vi phạm"))

	_, err = e.svc.Approve(ctx, e.admin, v.ID)
	require.NoError(t, err)
	_, err = e.svc.MarkPaid(ctx, e.admin, v.ID, MarkPaidInput{TransferReference: "FT1", ProofFileID: e.uploadProof(t, e.admin, "payout_proof")})
	require.NoError(t, err)
	e.requireAvailable(t, p, 600_000)

	_, err = e.svc.Reject(ctx, e.admin, w.ID, "đình chỉ")
	require.NoError(t, err)
	e.requireAvailable(t, q, 900_000)
}

func TestAdminList_Filters(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	q := e.fundedPartner(t, "beta")
	v1, err := e.svc.Request(ctx, p.ID, p.Owner, 100_000)
	require.NoError(t, err)
	_, err = e.svc.Reject(ctx, e.admin, v1.ID, "x")
	require.NoError(t, err)
	v2, err := e.svc.Request(ctx, p.ID, p.Owner, 200_000)
	require.NoError(t, err)
	w, err := e.svc.Request(ctx, q.ID, q.Owner, 300_000)
	require.NoError(t, err)

	all, next, err := e.svc.AdminList(ctx, AdminFilter{})
	require.NoError(t, err)
	require.Nil(t, next)
	require.Equal(t, []uuid.UUID{w.ID, v2.ID, v1.ID}, ids(all))

	st := StatusRequested
	got, _, err := e.svc.AdminList(ctx, AdminFilter{Status: &st})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{w.ID, v2.ID}, ids(got))

	got, _, err = e.svc.AdminList(ctx, AdminFilter{PartnerID: &p.ID})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{v2.ID, v1.ID}, ids(got))

	got, next, err = e.svc.AdminList(ctx, AdminFilter{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{w.ID, v2.ID}, ids(got))
	require.NotNil(t, next)
	got, next, err = e.svc.AdminList(ctx, AdminFilter{Limit: 2, Cursor: next})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{v1.ID}, ids(got))
	require.Nil(t, next)

	bad := "bogus"
	_, _, err = e.svc.AdminList(ctx, AdminFilter{Status: &bad})
	requireCode(t, err, http.StatusBadRequest, "invalid_request")
}

func ids(ps []Payout) []uuid.UUID {
	out := make([]uuid.UUID, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

// The payout row is locked before partner_balances: a Reject waiting on a
// payout held by another tx does not hold the balance meanwhile.
func TestReject_LocksPayoutBeforeBalance(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)

	tx, err := e.pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT 1 FROM payouts WHERE id = $1 FOR UPDATE`, v.ID)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := e.svc.Reject(ctx, e.admin, v.ID, "chờ khóa")
		done <- err
	}()
	// While Reject waits on the payout, the balance row stays free.
	require.Eventually(t, func() bool {
		return countWhere(t, e, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%payouts%'`) == 1
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, db.InTx(ctx, e.pool, func(tx2 pgx.Tx) error {
		_, err := tx2.Exec(ctx, `SELECT 1 FROM partner_balances WHERE partner_id = $1 FOR UPDATE NOWAIT`, p.ID)
		return err
	}))
	require.NoError(t, tx.Rollback(ctx))
	require.NoError(t, <-done)
	e.requireAvailable(t, p, 900_000)
}
