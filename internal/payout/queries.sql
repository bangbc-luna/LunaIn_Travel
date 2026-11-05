-- name: GetPayout :one
SELECT * FROM payouts WHERE id = @id;
