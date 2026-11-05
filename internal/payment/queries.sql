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
SELECT id FROM checkouts WHERE id = @id FOR UPDATE;

-- name: LockCheckoutBookingRows :many
SELECT id FROM bookings WHERE checkout_id = @checkout_id ORDER BY id FOR UPDATE;

-- name: CancelPayment :exec
UPDATE payments SET status = 'cancelled', updated_at = @now WHERE id = @id;

-- Money received on a payment: status paid or needs_review.
-- name: RecordPaymentReceived :exec
UPDATE payments
SET status = @status, paid_at = @paid_at::timestamptz, provider_reference = @provider_reference::text,
    raw = @raw, updated_at = @now
WHERE id = @id;

-- name: PaymentEventExists :one
SELECT EXISTS (SELECT 1 FROM payment_events WHERE dedupe_key = @dedupe_key);

-- A repeated delivery inserts nothing (0 rows). A concurrent insert of the
-- same key waits for the first tx and then inserts nothing if it committed.
-- name: InsertPaymentEvent :execrows
INSERT INTO payment_events (id, payment_id, provider, dedupe_key, signature_valid, payload, received_at,
                            processed_at, result, created_at, updated_at)
VALUES (@id, sqlc.narg(payment_id), 'payos', @dedupe_key, true, @payload, @now,
        sqlc.narg(processed_at), sqlc.narg(result), @now, @now)
ON CONFLICT (dedupe_key) DO NOTHING;

-- name: FinishPaymentEvent :exec
UPDATE payment_events SET processed_at = @now::timestamptz, result = @result::text, updated_at = @now
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
UPDATE payments SET status = @status, updated_at = @now WHERE id = @id;

-- name: GetPaymentEventReceivedAt :one
SELECT received_at FROM payment_events WHERE dedupe_key = @dedupe_key;

-- name: CheckoutHasBookingRefund :one
SELECT EXISTS (SELECT 1 FROM refunds WHERE checkout_id = @checkout_id AND booking_id IS NOT NULL);

-- The payment whose money confirmed the checkout: the paid one, else (an
-- overpayment left it needs_review) the latest that received money.
-- name: GetCapturedPayment :one
SELECT * FROM payments
WHERE checkout_id = @checkout_id AND provider_reference IS NOT NULL
  AND status IN ('paid', 'needs_review')
ORDER BY (status = 'paid') DESC, paid_at DESC, id DESC
LIMIT 1;

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
