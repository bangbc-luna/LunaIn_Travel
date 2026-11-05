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
