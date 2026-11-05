-- name: GetPayment :one
SELECT * FROM payments WHERE id = @id;

-- provider_order_code comes from payment_order_code_seq (column default).
-- name: InsertPendingPayment :one
INSERT INTO payments (id, checkout_id, amount, status, expires_at, created_at, updated_at)
VALUES (@id, @checkout_id, @amount, 'pending', @expires_at, @now, @now)
RETURNING *;

-- name: GetPaymentWithCheckoutCode :one
SELECT sqlc.embed(p), c.code AS checkout_code
FROM payments p
JOIN checkouts c ON c.id = p.checkout_id
WHERE p.id = @id;

-- name: GetPendingPaymentByCheckout :one
SELECT * FROM payments WHERE checkout_id = @checkout_id AND status = 'pending';

-- Stores the link only while the payment is still pending and has none.
-- name: SetPaymentLink :execrows
UPDATE payments
SET checkout_url = @checkout_url, qr_code = sqlc.narg(qr_code), updated_at = @now
WHERE id = @id AND status = 'pending' AND checkout_url IS NULL;

-- name: GetPaymentByOrderCode :one
SELECT * FROM payments WHERE provider_order_code = @order_code;

-- payments come after checkouts and bookings in the lock order (DB §1.1).
-- FOR NO KEY UPDATE, not FOR UPDATE: a webhook inserts its payment_events
-- row (FK -> payments: FOR KEY SHARE on the payment) BEFORE it locks the
-- checkout; a FOR UPDATE here (after the checkout) would wait for that
-- webhook while it waits for the checkout: a deadlock. NO KEY UPDATE is still
-- exclusive between lockers and writers of the payment row.
-- name: LockPayment :one
SELECT * FROM payments WHERE id = @id FOR NO KEY UPDATE;

-- Lock helpers for rotating a payment (checkout -> bookings -> payment).
-- name: LockCheckoutRow :one
SELECT id, status, customer_id FROM checkouts WHERE id = @id FOR UPDATE;

-- name: LockCheckoutBookingRows :many
SELECT id FROM bookings WHERE checkout_id = @checkout_id ORDER BY id FOR UPDATE;

-- Every query writing payments.status clears the review columns (Ruling 1):
-- payments_review_status_check allows them only on needs_review, and a new
-- needs_review (new money) reopens the review from scratch.
-- name: CancelPayment :exec
UPDATE payments SET status = 'cancelled', review_seen_at = NULL, review_seen_by = NULL, review_resolved_at = NULL, review_resolved_by = NULL,
    review_note = NULL, updated_at = @now
WHERE id = @id;

-- Money received on a payment: status paid or needs_review.
-- name: RecordPaymentReceived :exec
UPDATE payments
SET status = @status, paid_at = @paid_at::timestamptz, provider_reference = @provider_reference::text,
    raw = @raw, review_seen_at = NULL, review_seen_by = NULL, review_resolved_at = NULL, review_resolved_by = NULL,
    review_note = NULL, updated_at = @now
WHERE id = @id;

-- name: PaymentEventExists :one
SELECT EXISTS (SELECT 1 FROM payment_events WHERE dedupe_key = @dedupe_key);

-- A repeated delivery inserts nothing (0 rows). A concurrent insert of the
-- same key waits for the first tx and then inserts nothing if it committed.
-- name: InsertPaymentEvent :execrows
INSERT INTO payment_events (id, payment_id, provider, dedupe_key, signature_valid, payload, received_at,
                            processed_at, result, amount, created_at, updated_at)
VALUES (@id, sqlc.narg(payment_id), 'payos', @dedupe_key, true, @payload, @now,
        sqlc.narg(processed_at), sqlc.narg(result), sqlc.narg(amount), @now, @now)
ON CONFLICT (dedupe_key) DO NOTHING;

-- An already_processed transfer brought no new money: its amount is cleared.
-- name: FinishPaymentEvent :exec
UPDATE payment_events
SET processed_at = @now::timestamptz, result = @result::text,
    amount = CASE WHEN @result::text = 'already_processed' THEN NULL ELSE amount END, updated_at = @now
WHERE dedupe_key = @dedupe_key;

-- name: GetRefundAccount :one
SELECT * FROM customer_refund_accounts WHERE user_id = @user_id;

-- name: InsertRefund :one
INSERT INTO refunds (id, payment_id, checkout_id, booking_id, provider_reference, amount, reason, status,
                     bank_bin, account_number, account_holder, created_at, updated_at)
VALUES (@id, @payment_id, @checkout_id, sqlc.narg(booking_id), sqlc.narg(provider_reference), @amount, @reason,
        @status, sqlc.narg(bank_bin), sqlc.narg(account_number), sqlc.narg(account_holder), @now, @now)
RETURNING *;

-- All payments of a checkout, locked in id order (after its bookings),
-- FOR NO KEY UPDATE (see LockPayment).
-- name: LockCheckoutPayments :many
SELECT * FROM payments WHERE checkout_id = @checkout_id ORDER BY id FOR NO KEY UPDATE;

-- name: SetPaymentStatus :exec
UPDATE payments SET status = @status, review_seen_at = NULL, review_seen_by = NULL, review_resolved_at = NULL, review_resolved_by = NULL,
    review_note = NULL, updated_at = @now
WHERE id = @id;

-- name: GetPaymentEventReceivedAt :one
SELECT received_at FROM payment_events WHERE dedupe_key = @dedupe_key;

-- name: CheckoutHasBookingRefund :one
SELECT EXISTS (SELECT 1 FROM refunds WHERE checkout_id = @checkout_id AND booking_id IS NOT NULL);

-- name: UpsertRefundAccount :one
INSERT INTO customer_refund_accounts (user_id, bank_bin, account_number, account_holder, created_at, updated_at)
VALUES (@user_id, @bank_bin, @account_number, @account_holder, @now, @now)
ON CONFLICT (user_id) DO UPDATE
SET bank_bin = EXCLUDED.bank_bin, account_number = EXCLUDED.account_number,
    account_holder = EXCLUDED.account_holder, updated_at = EXCLUDED.updated_at
RETURNING *;

-- A refund of the customer (through its checkout), locked.
-- name: LockCustomerRefund :one
SELECT r.* FROM refunds r
JOIN checkouts c ON c.id = r.checkout_id
WHERE r.id = @id AND c.customer_id = @customer_id
FOR UPDATE OF r;

-- name: SetRefundBankInfo :exec
UPDATE refunds
SET status = 'pending', bank_bin = @bank_bin, account_number = @account_number,
    account_holder = @account_holder, updated_at = @now
WHERE id = @id;

-- name: ListCustomerRefunds :many
SELECT r.* FROM refunds r
JOIN checkouts c ON c.id = r.checkout_id
WHERE c.customer_id = @customer_id
ORDER BY r.id DESC
LIMIT @lim;

-- name: GetRefund :one
SELECT * FROM refunds WHERE id = @id;

-- Only the refund row (Super Admin actions, reminders).
-- name: LockRefund :one
SELECT * FROM refunds WHERE id = @id FOR UPDATE;

-- name: CompleteRefund :one
UPDATE refunds
SET status = 'completed', transfer_reference = @transfer_reference::text, proof_file_id = sqlc.narg(proof_file_id),
    completed_by = @actor::uuid, completed_at = @now::timestamptz, updated_at = @now
WHERE id = @id
RETURNING *;

-- name: GetCheckoutCustomer :one
SELECT customer_id FROM checkouts WHERE id = @id;

-- Newest first; every filter optional; cursor = last id of the previous page.
-- name: ListAdminRefunds :many
SELECT * FROM refunds
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(reason)::text IS NULL OR reason = sqlc.narg(reason))
  AND (sqlc.narg(booking_id)::uuid IS NULL OR booking_id = sqlc.narg(booking_id))
  AND (sqlc.narg(payment_id)::uuid IS NULL OR payment_id = sqlc.narg(payment_id))
  AND (sqlc.narg(cursor)::uuid IS NULL OR id < sqlc.narg(cursor))
ORDER BY id DESC
LIMIT @lim;

-- Refunds still waiting for the customer's bank account, created and last
-- reminded at or before @cutoff, reminded fewer than @max_count times.
-- name: ListRefundsToRemind :many
SELECT id FROM refunds
WHERE status = 'awaiting_bank_info'
  AND created_at <= @cutoff
  AND (bank_info_reminded_at IS NULL OR bank_info_reminded_at <= @cutoff)
  AND bank_info_reminder_count < @max_count::int
ORDER BY created_at, id
LIMIT @lim;

-- name: MarkRefundReminded :exec
UPDATE refunds
SET bank_info_reminder_count = bank_info_reminder_count + 1, bank_info_reminded_at = @now::timestamptz, updated_at = @now
WHERE id = @id;

-- Newest first. open_review: needs_review and not resolved yet.
-- name: ListAdminPayments :many
SELECT * FROM payments
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (NOT @open_review::bool OR (status = 'needs_review' AND review_resolved_at IS NULL))
  AND (sqlc.narg(checkout_id)::uuid IS NULL OR checkout_id = sqlc.narg(checkout_id))
  AND (sqlc.narg(cursor)::uuid IS NULL OR id < sqlc.narg(cursor))
ORDER BY id DESC
LIMIT @lim;

-- name: ListPaymentEvents :many
SELECT dedupe_key, result, amount, received_at FROM payment_events
WHERE payment_id = @payment_id
ORDER BY received_at, id;

-- name: ListPaymentRefunds :many
SELECT * FROM refunds WHERE payment_id = @payment_id ORDER BY id;

-- name: SetReviewSeen :one
UPDATE payments SET review_seen_at = @now::timestamptz, review_seen_by = @actor::uuid, updated_at = @now
WHERE id = @id
RETURNING *;

-- Resolving implies seeing: the seen columns are filled when still empty.
-- name: ResolveReview :one
UPDATE payments
SET review_seen_at = COALESCE(review_seen_at, @now::timestamptz), review_seen_by = COALESCE(review_seen_by, @actor::uuid),
    review_resolved_at = @now::timestamptz, review_resolved_by = @actor::uuid, review_note = sqlc.narg(note),
    updated_at = @now::timestamptz
WHERE id = @id
RETURNING *;

-- awaiting_status tracking rows received at or before @cutoff whose transfer
-- has no main row (the dedupe key without the ":awaiting" suffix) yet.
-- name: ListStaleAwaitingEvents :many
SELECT e.dedupe_key, e.payment_id, e.payload FROM payment_events e
WHERE e.result = 'awaiting_status'
  AND e.received_at <= @cutoff
  AND NOT EXISTS (SELECT 1 FROM payment_events m WHERE m.dedupe_key = left(e.dedupe_key, -length(':awaiting')))
ORDER BY e.received_at, e.id
LIMIT @lim;

-- name: GetPaymentEventResult :one
SELECT result FROM payment_events WHERE dedupe_key = @dedupe_key;

-- Payments created at or after @since, plus every unresolved needs_review
-- payment: the unresolved needs_review ones first, then oldest first.
-- name: ListPaymentsToReconcile :many
SELECT * FROM payments
WHERE created_at >= @since OR (status = 'needs_review' AND review_resolved_at IS NULL)
ORDER BY (status = 'needs_review' AND review_resolved_at IS NULL) DESC, id
LIMIT @lim;

-- name: CountPaymentsToReconcile :one
SELECT count(*) FROM payments
WHERE created_at >= @since OR (status = 'needs_review' AND review_resolved_at IS NULL);

-- Money recorded on a payment (awaiting rows carry no amount) and when the
-- last transfer with an amount was received (epoch when none).
-- name: PaymentRecorded :one
SELECT COALESCE(sum(amount), 0)::bigint AS recorded,
       COALESCE(max(received_at) FILTER (WHERE amount IS NOT NULL), 'epoch'::timestamptz)::timestamptz AS last_received_at
FROM payment_events
WHERE payment_id = @payment_id AND dedupe_key NOT LIKE '%:awaiting';

-- name: UpsertReconciliation :one
INSERT INTO payment_reconciliations (id, run_date, status, checked_payments, mismatches, started_at, finished_at,
                                     run_by, created_at, updated_at)
VALUES (@id, @run_date, @status, @checked_payments, @mismatches, @started_at, @finished_at, sqlc.narg(run_by),
        @now, @now)
ON CONFLICT (run_date) DO UPDATE
SET status = EXCLUDED.status, checked_payments = EXCLUDED.checked_payments, mismatches = EXCLUDED.mismatches,
    started_at = EXCLUDED.started_at, finished_at = EXCLUDED.finished_at, run_by = EXCLUDED.run_by,
    updated_at = EXCLUDED.updated_at
RETURNING *;

-- Newest first; @before (exclusive) when given.
-- name: ListReconciliations :many
SELECT * FROM payment_reconciliations
WHERE (sqlc.narg(before)::date IS NULL OR run_date < sqlc.narg(before)::date)
ORDER BY run_date DESC
LIMIT @lim;

-- name: GetReconciliation :one
SELECT * FROM payment_reconciliations WHERE run_date = @run_date;
