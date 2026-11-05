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
