package payout

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
)

// Schema tests for migrations 00015-00016: rows are inserted with raw SQL so
// the DB constraints themselves are exercised, independent of any service.

func requireViolation(t *testing.T, err error, code, constraint string) {
	t.Helper()
	require.Error(t, err)
	var e *pgconn.PgError
	require.True(t, errors.As(err, &e), "not a PgError: %v", err)
	require.Equal(t, code, e.Code, "message: %s", e.Message)
	if constraint != "" {
		require.Equal(t, constraint, e.ConstraintName)
	}
}

func newID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	require.NoError(t, err)
	return id
}

type fixture struct {
	pool    *pgxpool.Pool
	owner   uuid.UUID
	partner uuid.UUID
	file    uuid.UUID
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) error {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), sql, args...)
	return err
}

func (f *fixture) mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	require.NoError(t, f.exec(t, sql, args...))
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{pool: testutil.Postgres(t), owner: newID(t), partner: newID(t), file: newID(t)}
	f.mustExec(t, `INSERT INTO users (id, email, role) VALUES ($1, 'o@partner.test', 'partner')`, f.owner)
	f.mustExec(t, `INSERT INTO partners (id, legal_name, display_name, created_by) VALUES ($1, 'alpha', 'alpha', $2)`,
		f.partner, f.owner)
	f.mustExec(t, `INSERT INTO files (id, storage_key, purpose, visibility, content_type, size_bytes, uploaded_by)
		VALUES ($1, 'k-'||$3::text, 'payout_proof', 'private', 'image/png', 10, $2)`, f.file, f.owner, f.file.String())
	return f
}

func (f *fixture) bankAccount(t *testing.T) uuid.UUID {
	t.Helper()
	id := newID(t)
	f.mustExec(t, `INSERT INTO partner_bank_accounts (id, partner_id, bank_bin, bank_name, account_number, account_holder, is_current)
		VALUES ($1, $2, '970436', 'VCB', '0011001234567', 'NGUYEN VAN A', true)`, id, f.partner)
	return id
}

// insertPayout inserts a payout of the fixture partner in the given status
// with the columns that status requires, except those named in omit
// ("proof", "ref").
func (f *fixture) insertPayout(t *testing.T, id, bank uuid.UUID, status string, omit ...string) error {
	t.Helper()
	skip := map[string]bool{}
	for _, o := range omit {
		skip[o] = true
	}
	var proof, ref any
	if status == "paid" {
		if !skip["proof"] {
			proof = f.file
		}
		if !skip["ref"] {
			ref = "FT123"
		}
	}
	return f.exec(t, `INSERT INTO payouts (
		id, partner_id, amount, status, bank_bin, bank_name, account_number, account_holder, bank_account_id,
		requested_by, requested_at,
		approved_by, approved_at, paid_by, paid_at, transfer_reference, proof_file_id,
		rejected_by, rejected_at, reject_reason
	) VALUES (
		$1, $2, 500000, $3, '970436', 'VCB', '0011001234567', 'NGUYEN VAN A', $4,
		$5::uuid, now(),
		CASE WHEN $3 IN ('approved','paid') THEN $5::uuid END, CASE WHEN $3 IN ('approved','paid') THEN now() END,
		CASE WHEN $3 = 'paid' THEN $5::uuid END, CASE WHEN $3 = 'paid' THEN now() END, $6::text, $7::uuid,
		CASE WHEN $3 = 'rejected' THEN $5::uuid END, CASE WHEN $3 = 'rejected' THEN now() END,
		CASE WHEN $3 = 'rejected' THEN 'sai thong tin' END
	)`, id, f.partner, status, bank, f.owner, ref, proof)
}

func (f *fixture) insertLedger(t *testing.T, seq int64, kind, bucket string, payout any, amount int64, reason any) error {
	t.Helper()
	var by any
	if kind == "adjustment" {
		by = f.owner
	}
	return f.exec(t, `INSERT INTO ledger_entries (id, partner_id, seq, available_after, payout_id, kind, bucket, amount, reason, created_by)
		VALUES ($1, $2, $3, 0, $4, $5, $6, $7, $8, $9)`, newID(t), f.partner, seq, payout, kind, bucket, amount, reason, by)
}

func TestPayoutsOneOpenPerPartner(t *testing.T) {
	f := newFixture(t)
	bank := f.bankAccount(t)
	first := newID(t)
	require.NoError(t, f.insertPayout(t, first, bank, "requested"))

	requireViolation(t, f.insertPayout(t, newID(t), bank, "requested"), "23505", "payouts_one_open_idx")

	f.mustExec(t, `UPDATE payouts SET status = 'rejected', rejected_by = $2, rejected_at = now(), reject_reason = 'x' WHERE id = $1`,
		first, f.owner)
	require.NoError(t, f.insertPayout(t, newID(t), bank, "requested"))
}

func TestPayoutPaidNeedsReferenceAndProof(t *testing.T) {
	f := newFixture(t)
	bank := f.bankAccount(t)
	requireViolation(t, f.insertPayout(t, newID(t), bank, "paid", "proof"), "23514", "")
	requireViolation(t, f.insertPayout(t, newID(t), bank, "paid", "ref"), "23514", "")
	require.NoError(t, f.insertPayout(t, newID(t), bank, "paid"))
}

func TestPayoutStatementColumns(t *testing.T) {
	f := newFixture(t)
	bank := f.bankAccount(t)
	id := newID(t)
	require.NoError(t, f.insertPayout(t, id, bank, "requested"))
	// All four statement columns are set together, and from <= to.
	requireViolation(t, f.exec(t, `UPDATE payouts SET statement_seq_from = 1 WHERE id = $1`, id), "23514", "")
	requireViolation(t, f.exec(t, `UPDATE payouts SET statement_seq_from = 5, statement_seq_to = 3,
		opening_available = 0, closing_available = 0 WHERE id = $1`, id), "23514", "")
	f.mustExec(t, `UPDATE payouts SET statement_seq_from = 1, statement_seq_to = 1,
		opening_available = 500000, closing_available = 0 WHERE id = $1`, id)
}

func TestPayoutHolderCheck(t *testing.T) {
	f := newFixture(t)
	bank := f.bankAccount(t)
	id := newID(t)
	require.NoError(t, f.insertPayout(t, id, bank, "requested"))
	requireViolation(t, f.exec(t, `UPDATE payouts SET account_holder = 'nguyen' WHERE id = $1`, id), "23514", "")
}

func TestLedgerPayoutHoldSign(t *testing.T) {
	f := newFixture(t)
	p := newID(t)
	require.NoError(t, f.insertPayout(t, p, f.bankAccount(t), "requested"))

	requireViolation(t, f.insertLedger(t, 1, "payout_hold", "available", p, 1, nil), "23514", "ledger_entries_payout_hold_check")
	requireViolation(t, f.insertLedger(t, 1, "payout_hold", "pending", p, -1, nil), "23514", "ledger_entries_payout_hold_check")
	require.NoError(t, f.insertLedger(t, 1, "payout_hold", "available", p, -500000, nil))
}

func TestLedgerPayoutReversalSign(t *testing.T) {
	f := newFixture(t)
	p := newID(t)
	require.NoError(t, f.insertPayout(t, p, f.bankAccount(t), "requested"))

	requireViolation(t, f.insertLedger(t, 1, "payout_reversal", "available", p, -1, nil), "23514", "ledger_entries_payout_reversal_check")
	requireViolation(t, f.insertLedger(t, 1, "payout_reversal", "pending", p, 1, nil), "23514", "ledger_entries_payout_reversal_check")
	require.NoError(t, f.insertLedger(t, 1, "payout_reversal", "available", p, 500000, nil))
}

func TestLedgerAdjustmentAvailableOnly(t *testing.T) {
	f := newFixture(t)
	requireViolation(t, f.insertLedger(t, 1, "adjustment", "pending", nil, 100, "fix"), "23514", "ledger_entries_adjustment_check")
	requireViolation(t, f.insertLedger(t, 1, "adjustment", "available", nil, 100, nil), "23514", "")
	require.NoError(t, f.insertLedger(t, 1, "adjustment", "available", nil, -100, "fix"))
	require.NoError(t, f.insertLedger(t, 2, "adjustment", "available", nil, 100, "fix"))
	// An adjustment never belongs to a payout.
	p := newID(t)
	require.NoError(t, f.insertPayout(t, p, f.bankAccount(t), "requested"))
	requireViolation(t, f.insertLedger(t, 3, "adjustment", "available", p, 100, "fix"), "23514", "ledger_entries_adjustment_check")
}

func TestLedgerPayoutFK(t *testing.T) {
	f := newFixture(t)
	requireViolation(t, f.insertLedger(t, 1, "payout_hold", "available", newID(t), -100, nil), "23503", "ledger_entries_payout_fk")
}

func TestBankHolderCheck(t *testing.T) {
	f := newFixture(t)
	err := f.exec(t, `INSERT INTO partner_bank_accounts (id, partner_id, bank_bin, bank_name, account_number, account_holder, is_current)
		VALUES ($1, $2, '970436', 'VCB', '0011001234567', 'NGUYEN VAN A2', true)`, newID(t), f.partner)
	requireViolation(t, err, "23514", "partner_bank_accounts_holder_check")

	err = f.exec(t, `INSERT INTO customer_refund_accounts (user_id, bank_bin, account_number, account_holder)
		VALUES ($1, '970436', '0011001234567', 'O''NEIL')`, f.owner)
	requireViolation(t, err, "23514", "customer_refund_accounts_holder_check")
	require.NoError(t, f.exec(t, `INSERT INTO customer_refund_accounts (user_id, bank_bin, account_number, account_holder)
		VALUES ($1, '970436', '0011001234567', 'NGUYEN VAN A')`, f.owner))
}

func TestReviewResolvedOnlyWhenNeedsReview(t *testing.T) {
	f := newFixture(t)
	customer, checkout, payment := newID(t), newID(t), newID(t)
	f.mustExec(t, `INSERT INTO users (id, email, role) VALUES ($1, 'c@customer.test', 'customer')`, customer)
	f.mustExec(t, `INSERT INTO checkouts (id, code, customer_id, total_amount, status) VALUES ($1, 'CO00000001', $2, 200000, 'open')`,
		checkout, customer)
	f.mustExec(t, `INSERT INTO payments (id, checkout_id, amount, status, expires_at)
		VALUES ($1, $2, 200000, 'pending', now() + interval '15 minutes')`, payment, checkout)

	// Seen / resolved on a payment that is not needs_review.
	requireViolation(t, f.exec(t, `UPDATE payments SET review_seen_at = now(), review_seen_by = $2 WHERE id = $1`, payment, f.owner), "23514", "")
	requireViolation(t, f.exec(t, `UPDATE payments SET review_resolved_at = now(), review_resolved_by = $2 WHERE id = $1`, payment, f.owner), "23514", "")

	f.mustExec(t, `UPDATE payments SET status = 'needs_review' WHERE id = $1`, payment)
	// Resolved without seen.
	requireViolation(t, f.exec(t, `UPDATE payments SET review_resolved_at = now(), review_resolved_by = $2 WHERE id = $1`, payment, f.owner), "23514", "")
	// Half-filled pair.
	requireViolation(t, f.exec(t, `UPDATE payments SET review_seen_at = now() WHERE id = $1`, payment), "23514", "")
	f.mustExec(t, `UPDATE payments SET review_seen_at = now(), review_seen_by = $2 WHERE id = $1`, payment, f.owner)
	requireViolation(t, f.exec(t, `UPDATE payments SET review_resolved_by = $2 WHERE id = $1`, payment, f.owner), "23514", "")
	f.mustExec(t, `UPDATE payments SET review_resolved_at = now(), review_resolved_by = $2, review_note = 'ok' WHERE id = $1`, payment, f.owner)
	// Leaving needs_review while seen is set is rejected.
	requireViolation(t, f.exec(t, `UPDATE payments SET status = 'pending' WHERE id = $1`, payment), "23514", "")
}

func TestReconciliationRunDateUnique(t *testing.T) {
	f := newFixture(t)
	ins := `INSERT INTO payment_reconciliations (id, run_date, status, checked_payments, started_at, finished_at)
		VALUES ($1, '2026-11-05', 'ok', 3, now(), now())`
	f.mustExec(t, ins, newID(t))
	requireViolation(t, f.exec(t, ins, newID(t)), "23505", "payment_reconciliations_run_date_key")
	requireViolation(t, f.exec(t, `INSERT INTO payment_reconciliations (id, run_date, status, checked_payments, mismatches, started_at, finished_at)
		VALUES ($1, '2026-11-06', 'mismatch', 1, '{}', now(), now())`, newID(t)), "23514", "")
}

func TestPaymentEventsAmountBackfilled(t *testing.T) {
	f := newFixture(t)
	// The backfill expression of migration 00016 against a sample payload.
	var amount *int64
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT CASE WHEN jsonb_typeof(p->'data'->'amount') = 'number' THEN (p->'data'->>'amount')::bigint END
		 FROM (SELECT '{"data":{"amount":200000}}'::jsonb AS p) s`).Scan(&amount))
	require.NotNil(t, amount)
	require.EqualValues(t, 200000, *amount)

	// The column exists and rejects non-positive values.
	err := f.exec(t, `INSERT INTO payment_events (id, provider, dedupe_key, signature_valid, payload, received_at, amount)
		VALUES ($1, 'payos', 'k1', true, '{}', now(), 0)`, newID(t))
	requireViolation(t, err, "23514", "")
	f.mustExec(t, `INSERT INTO payment_events (id, provider, dedupe_key, signature_valid, payload, received_at, amount)
		VALUES ($1, 'payos', 'k2', true, '{}', now(), 5)`, newID(t))
}
