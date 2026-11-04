-- name: ListServiceTypes :many
SELECT code, name, booking_mode, sort_order FROM service_types ORDER BY sort_order;

-- name: GetServiceType :one
SELECT code, name, booking_mode, sort_order FROM service_types WHERE code = @code;

-- name: InsertCommissionRate :execrows
INSERT INTO commission_rates (id, service_type, rate_bps, effective_from, created_by, created_at, updated_at)
VALUES (@id, @service_type, @rate_bps, @effective_from, @created_by, @now, @now)
ON CONFLICT (service_type, effective_from) DO NOTHING;

-- name: CurrentCommissionRate :one
SELECT rate_bps FROM commission_rates
WHERE service_type = @service_type AND effective_from <= @at
ORDER BY effective_from DESC
LIMIT 1;

-- name: ListCommissionRates :many
SELECT id, service_type, rate_bps, effective_from, created_by FROM commission_rates
WHERE service_type = @service_type
ORDER BY effective_from DESC;
