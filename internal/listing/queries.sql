-- A slug collision (random suffix) inserts nothing and returns no row, so the
-- caller can retry with a new suffix without aborting its tx.
-- name: InsertListing :one
INSERT INTO listings (
    id, partner_id, service_type, title, slug, summary, description,
    province_code, ward_code, address, latitude, longitude, created_at, updated_at
) VALUES (
    @id, @partner_id, @service_type, @title, @slug, sqlc.narg(summary), sqlc.narg(description),
    sqlc.narg(province_code), sqlc.narg(ward_code), sqlc.narg(address),
    sqlc.narg(latitude), sqlc.narg(longitude), @now, @now
)
ON CONFLICT (slug) DO NOTHING
RETURNING *;

-- name: GetOwnedListing :one
SELECT * FROM listings WHERE id = @id AND partner_id = @partner_id;

-- Writers lock the listings row FOR NO KEY UPDATE (DB §1.1): rows with an FK
-- to listings (images, ticket types, later bookings) take KEY SHARE on it,
-- which FOR UPDATE would block. Lock order: users -> partners -> listings.
-- name: LockOwnedListing :one
SELECT * FROM listings WHERE id = @id AND partner_id = @partner_id FOR NO KEY UPDATE;

-- name: ListOwnListings :many
SELECT * FROM listings
WHERE partner_id = @partner_id
  AND (sqlc.narg(review_status)::text IS NULL OR review_status = sqlc.narg(review_status))
  AND (sqlc.narg(cursor)::uuid IS NULL OR id < sqlc.narg(cursor))
ORDER BY id DESC
LIMIT @lim;

-- name: UpdateListingContent :one
UPDATE listings
SET title = @title,
    summary = sqlc.narg(summary),
    description = sqlc.narg(description),
    address = sqlc.narg(address),
    province_code = sqlc.narg(province_code),
    ward_code = sqlc.narg(ward_code),
    latitude = sqlc.narg(latitude),
    longitude = sqlc.narg(longitude),
    updated_at = @now
WHERE id = @id AND partner_id = @partner_id
RETURNING *;

-- name: InsertAuditLog :exec
INSERT INTO listing_audit_log (id, listing_id, actor_user_id, action, changes, created_at, updated_at)
VALUES (@id, @listing_id, @actor_user_id, @action, @changes, @now, @now);

-- name: DeleteListingImages :exec
DELETE FROM listing_images WHERE listing_id = @listing_id;

-- name: InsertListingImage :exec
INSERT INTO listing_images (id, listing_id, file_id, sort_order, created_at, updated_at)
VALUES (@id, @listing_id, @file_id, @sort_order, @now, @now);

-- name: ListListingImages :many
SELECT * FROM listing_images WHERE listing_id = @listing_id ORDER BY sort_order;

-- name: SetListingCover :one
UPDATE listings SET cover_file_id = sqlc.narg(cover_file_id), updated_at = @now
WHERE id = @id AND partner_id = @partner_id
RETURNING *;

-- name: DeleteCancellationTiers :exec
DELETE FROM listing_cancellation_tiers WHERE listing_id = @listing_id;

-- name: InsertCancellationTier :exec
INSERT INTO listing_cancellation_tiers (id, listing_id, hours_before, refund_bps, created_at, updated_at)
VALUES (@id, @listing_id, @hours_before, @refund_bps, @now, @now);

-- name: ListCancellationTiers :many
SELECT * FROM listing_cancellation_tiers WHERE listing_id = @listing_id ORDER BY hours_before DESC;
