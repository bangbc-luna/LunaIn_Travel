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
INSERT INTO ledger_entries (id, partner_id, seq, available_after, booking_id, payout_id, kind, bucket, amount,
                            reason, created_by, created_at, updated_at)
VALUES (@id, @partner_id, @seq, @available_after, @booking_id, @payout_id, @kind, @bucket, @amount,
        @reason, @created_by, @now, @now);

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

-- The platform commission taken on a booking (no row: 0% rate).
-- name: GetPlatformCommission :one
SELECT amount FROM platform_commission_entries
WHERE booking_id = @booking_id AND kind = 'commission';

-- Entries whose available_after differs from the running sum of the
-- available bucket up to and including their seq.
-- name: AvailableAfterMismatches :one
SELECT COUNT(*)::bigint FROM (
    SELECT available_after,
           SUM(CASE WHEN bucket = 'available' THEN amount ELSE 0 END)
               OVER (ORDER BY seq ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)::bigint AS running
    FROM ledger_entries
    WHERE partner_id = @partner_id
) e
WHERE e.available_after <> e.running;

-- Release job: settlements due and not released (booking_settlements_release_due_idx).
-- name: ListDueReleases :many
SELECT booking_id FROM booking_settlements
WHERE released_at IS NULL AND release_due_at <= @now::timestamptz
  AND NOT (booking_id = ANY(@skip::uuid[]))
ORDER BY release_due_at, booking_id
LIMIT @lim;

-- available_after of one entry (statement opening / closing balances).
-- name: GetAvailableAfter :one
SELECT available_after FROM ledger_entries
WHERE partner_id = @partner_id AND seq = @seq;

-- name: LastSeq :one
SELECT COALESCE(MAX(seq), 0)::bigint FROM ledger_entries WHERE partner_id = @partner_id;

-- Statement lines: the available bucket between two seqs (inclusive), by seq.
-- name: ListStatementLines :many
SELECT seq, kind, amount, available_after, booking_id, payout_id, reason, created_at
FROM ledger_entries
WHERE partner_id = @partner_id AND bucket = 'available' AND seq BETWEEN @from_seq::bigint AND @to_seq::bigint
ORDER BY seq;

-- Admin ledger page: every entry after a seq, by seq.
-- name: ListLedgerEntries :many
SELECT seq, kind, bucket, amount, available_after, booking_id, payout_id, reason, created_at
FROM ledger_entries
WHERE partner_id = @partner_id AND seq > @after_seq::bigint
ORDER BY seq
LIMIT @lim;

-- Every partner with a balance row or a ledger entry (VerifyAll).
-- name: ListLedgerPartners :many
SELECT partner_id FROM partner_balances
UNION
SELECT DISTINCT partner_id FROM ledger_entries
ORDER BY partner_id;

-- Settlements whose pending entries disagree with them: an open settlement's
-- net_pending must equal the sum of the booking's pending entries, a released
-- one must have that sum at 0 (spec §9).
-- name: SettlementDrifts :many
SELECT s.booking_id, s.partner_id, s.net_pending, (s.released_at IS NOT NULL)::bool AS released,
       COALESCE(e.pending, 0)::bigint AS entries_pending
FROM booking_settlements s
LEFT JOIN (
    SELECT booking_id, SUM(amount) AS pending
    FROM ledger_entries
    WHERE bucket = 'pending' AND booking_id IS NOT NULL
    GROUP BY booking_id
) e ON e.booking_id = s.booking_id
WHERE (s.released_at IS NULL AND s.net_pending <> COALESCE(e.pending, 0))
   OR (s.released_at IS NOT NULL AND COALESCE(e.pending, 0) <> 0)
ORDER BY s.partner_id, s.booking_id;

-- Pending entries of bookings that have no settlement row at all (VerifyAll).
-- name: OrphanPendingEntries :many
SELECT e.booking_id::uuid AS booking_id, e.partner_id, SUM(e.amount)::bigint AS pending
FROM ledger_entries e
WHERE e.bucket = 'pending' AND e.booking_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM booking_settlements s WHERE s.booking_id = e.booking_id)
GROUP BY e.booking_id, e.partner_id
ORDER BY e.partner_id, e.booking_id;

-- FK guard for an adjustment tied to a booking: takes, in the global lock
-- order (bookings before partner_balances, DB §1.1), the FOR KEY SHARE lock
-- the ledger_entries FK checks would otherwise take after partner_balances.
-- Reads the booking module's table on purpose (like the payments FK guard).
-- name: KeyShareBookingOfPartner :one
SELECT id FROM bookings WHERE id = @booking_id AND partner_id = @partner_id FOR KEY SHARE;
