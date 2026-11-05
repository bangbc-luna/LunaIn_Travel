-- name: GetPayment :one
SELECT * FROM payments WHERE id = @id;
