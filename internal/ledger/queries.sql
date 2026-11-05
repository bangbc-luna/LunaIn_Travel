-- name: GetPartnerBalance :one
SELECT * FROM partner_balances WHERE partner_id = @partner_id;
