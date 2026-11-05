-- name: GetPayout :one
SELECT * FROM payouts WHERE id = @id;

-- name: GetPartnerPayout :one
SELECT * FROM payouts WHERE id = @id AND partner_id = @partner_id;

-- The statement columns stay NULL until SetPayoutStatement, in the same tx
-- (the payout_hold entry needs this row first).
-- name: InsertPayout :exec
INSERT INTO payouts (
    id, partner_id, amount, status, bank_bin, bank_name, account_number, account_holder,
    bank_account_id, requested_by, requested_at, created_at, updated_at
) VALUES (
    @id, @partner_id, @amount, 'requested', @bank_bin, @bank_name, @account_number, @account_holder,
    @bank_account_id, @requested_by, @now, @now, @now
);

-- name: SetPayoutStatement :exec
UPDATE payouts
SET statement_seq_from = @seq_from::bigint, statement_seq_to = @seq_to::bigint,
    opening_available = @opening::bigint, closing_available = @closing::bigint, updated_at = @now
WHERE id = @id;

-- The last statement end of the partner's other payouts, whatever their
-- status; 0 before the first payout.
-- name: LastStatementSeqTo :one
SELECT COALESCE(MAX(statement_seq_to), 0)::bigint FROM payouts
WHERE partner_id = @partner_id AND id <> @exclude_id;

-- name: OpenPayoutID :one
SELECT id FROM payouts WHERE partner_id = @partner_id AND status IN ('requested','approved');

-- name: ListPartnerPayouts :many
SELECT * FROM payouts
WHERE partner_id = @partner_id
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(cursor)::uuid IS NULL OR id < sqlc.narg(cursor))
ORDER BY id DESC
LIMIT @lim;

-- name: ListAdminPayouts :many
SELECT * FROM payouts
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(partner_id)::uuid IS NULL OR partner_id = sqlc.narg(partner_id))
  AND (sqlc.narg(cursor)::uuid IS NULL OR id < sqlc.narg(cursor))
ORDER BY id DESC
LIMIT @lim;

-- Admin transitions hold this lock first (DB §1.1: payouts before
-- partner_balances).
-- name: LockPayout :one
SELECT * FROM payouts WHERE id = @id FOR UPDATE;

-- name: ApprovePayout :one
UPDATE payouts
SET status = 'approved', approved_by = @actor::uuid, approved_at = @now::timestamptz, updated_at = @now
WHERE id = @id AND status = 'requested'
RETURNING *;

-- name: MarkPayoutPaid :one
UPDATE payouts
SET status = 'paid', paid_by = @actor::uuid, paid_at = @now::timestamptz, transfer_reference = @transfer_reference::text,
    proof_file_id = @proof_file_id::uuid, updated_at = @now
WHERE id = @id AND status = 'approved'
RETURNING *;

-- name: RejectPayout :one
UPDATE payouts
SET status = 'rejected', rejected_by = @actor::uuid, rejected_at = @now::timestamptz, reject_reason = @reason::text, updated_at = @now
WHERE id = @id AND status IN ('requested','approved')
RETURNING *;
