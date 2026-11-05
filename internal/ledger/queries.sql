-- name: GetPartnerBalance :one
SELECT * FROM partner_balances WHERE partner_id = @partner_id;

-- Creates the balance row lazily; LockPartnerBalance follows in the same tx.
-- name: EnsurePartnerBalance :exec
INSERT INTO partner_balances (partner_id, created_at, updated_at)
VALUES (@partner_id, @now, @now)
ON CONFLICT (partner_id) DO NOTHING;

-- partner_balances is the LAST lock of the global order (DB §1.1): nothing
-- else may be locked after it in the same transaction.
-- name: LockPartnerBalance :one
SELECT * FROM partner_balances WHERE partner_id = @partner_id FOR UPDATE;

-- name: UpdatePartnerBalance :exec
UPDATE partner_balances
SET pending = @pending, available = @available, last_seq = @last_seq, updated_at = @now
WHERE partner_id = @partner_id;

-- name: InsertLedgerEntry :exec
INSERT INTO ledger_entries (id, partner_id, seq, available_after, booking_id, kind, bucket, amount, created_at, updated_at)
VALUES (@id, @partner_id, @seq, @available_after, @booking_id, @kind, @bucket, @amount, @now, @now);

-- name: InsertPlatformCommission :exec
INSERT INTO platform_commission_entries (id, booking_id, service_type, kind, amount, created_at, updated_at)
VALUES (@id, @booking_id, @service_type, @kind, @amount, @now, @now);

-- name: InsertSettlement :exec
INSERT INTO booking_settlements (booking_id, partner_id, net_pending, created_at, updated_at)
VALUES (@booking_id, @partner_id, @net_pending, @now, @now);

-- A booking child row: locked after bookings and before partner_balances.
-- name: LockSettlement :one
SELECT * FROM booking_settlements WHERE booking_id = @booking_id FOR UPDATE;

-- name: AddSettlementNetPending :exec
UPDATE booking_settlements
SET net_pending = net_pending + @delta, updated_at = @now
WHERE booking_id = @booking_id;

-- name: SetSettlementReleaseDue :exec
UPDATE booking_settlements
SET release_due_at = @release_due_at::timestamptz, updated_at = @now
WHERE booking_id = @booking_id AND release_due_at IS NULL;

-- name: MarkSettlementReleased :exec
UPDATE booking_settlements
SET net_pending = 0, released_at = @now::timestamptz, updated_at = @now
WHERE booking_id = @booking_id;

-- Totals Verify compares with the partner_balances cache.
-- name: LedgerTotals :one
SELECT
    COALESCE(SUM(amount) FILTER (WHERE bucket = 'pending'), 0)::bigint   AS pending,
    COALESCE(SUM(amount) FILTER (WHERE bucket = 'available'), 0)::bigint AS available,
    COUNT(*)::bigint                                                     AS entries,
    COALESCE(MAX(seq), 0)::bigint                                        AS max_seq
FROM ledger_entries
WHERE partner_id = @partner_id;

-- name: LastAvailableAfter :one
SELECT available_after FROM ledger_entries
WHERE partner_id = @partner_id
ORDER BY seq DESC
LIMIT 1;
