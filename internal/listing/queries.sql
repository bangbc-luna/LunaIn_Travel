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

-- name: GetAttraction :one
SELECT * FROM attractions WHERE listing_id = @listing_id;

-- name: UpsertAttraction :one
INSERT INTO attractions (listing_id, opening_note, checkin_instructions, created_at, updated_at)
VALUES (@listing_id, sqlc.narg(opening_note), sqlc.narg(checkin_instructions), @now, @now)
ON CONFLICT (listing_id) DO UPDATE
SET opening_note = EXCLUDED.opening_note,
    checkin_instructions = EXCLUDED.checkin_instructions,
    updated_at = EXCLUDED.updated_at
RETURNING *;

-- name: EnsureAttraction :exec
INSERT INTO attractions (listing_id, created_at, updated_at)
VALUES (@listing_id, @now, @now)
ON CONFLICT (listing_id) DO NOTHING;

-- name: InsertTicketType :one
INSERT INTO attraction_ticket_types (id, listing_id, name, description, audience, price, active, sort_order, created_at, updated_at)
VALUES (@id, @listing_id, @name, sqlc.narg(description), @audience, @price, @active, @sort_order, @now, @now)
RETURNING *;

-- name: GetTicketType :one
SELECT * FROM attraction_ticket_types WHERE id = @id AND listing_id = @listing_id;

-- name: UpdateTicketType :one
UPDATE attraction_ticket_types
SET name = @name, description = sqlc.narg(description), audience = @audience, price = @price,
    active = @active, sort_order = @sort_order, updated_at = @now
WHERE id = @id AND listing_id = @listing_id
RETURNING *;

-- name: ListTicketTypes :many
SELECT * FROM attraction_ticket_types
WHERE listing_id = @listing_id AND (@include_inactive::bool OR active)
ORDER BY sort_order, name, id;

-- name: ListActiveTicketTypesByIDs :many
SELECT * FROM attraction_ticket_types
WHERE listing_id = @listing_id AND active AND id = ANY(@ids::uuid[]);

-- name: InsertTimeSlot :one
INSERT INTO attraction_time_slots (id, listing_id, start_time, end_time, weekdays, active, created_at, updated_at)
VALUES (@id, @listing_id, @start_time, @end_time, @weekdays, @active, @now, @now)
RETURNING *;

-- name: GetTimeSlot :one
SELECT * FROM attraction_time_slots WHERE id = @id AND listing_id = @listing_id;

-- name: UpdateTimeSlot :one
UPDATE attraction_time_slots
SET start_time = @start_time, end_time = @end_time, weekdays = @weekdays, active = @active, updated_at = @now
WHERE id = @id AND listing_id = @listing_id
RETURNING *;

-- name: ListTimeSlots :many
SELECT * FROM attraction_time_slots
WHERE listing_id = @listing_id AND (@include_inactive::bool OR active)
ORDER BY start_time, end_time, id;

-- name: InsertClosedDate :execrows
INSERT INTO attraction_closed_dates (listing_id, date, reason, created_at, updated_at)
VALUES (@listing_id, @date, sqlc.narg(reason), @now, @now)
ON CONFLICT (listing_id, date) DO NOTHING;

-- name: GetClosedDate :one
SELECT * FROM attraction_closed_dates WHERE listing_id = @listing_id AND date = @date;

-- name: DeleteClosedDate :execrows
DELETE FROM attraction_closed_dates WHERE listing_id = @listing_id AND date = @date;

-- name: ListClosedDatesFrom :many
SELECT * FROM attraction_closed_dates WHERE listing_id = @listing_id AND date >= @from_date ORDER BY date;

-- Review workflow. Admin transitions lock the row by id only (DB §1.1:
-- users -> partners -> listings); callers check the state after locking.
-- name: LockListing :one
SELECT * FROM listings WHERE id = @id FOR NO KEY UPDATE;

-- name: GetListing :one
SELECT * FROM listings WHERE id = @id;

-- name: SubmitListing :one
UPDATE listings
SET review_status = 'pending_review', submitted_at = @now, rejection_reason = NULL, updated_at = @now
WHERE id = @id
RETURNING *;

-- name: ApproveListing :one
UPDATE listings
SET review_status = 'approved', reviewed_at = @now, reviewed_by = @actor, updated_at = @now
WHERE id = @id
RETURNING *;

-- name: RejectListing :one
UPDATE listings
SET review_status = 'rejected', rejection_reason = @reason, reviewed_at = @now, reviewed_by = @actor, updated_at = @now
WHERE id = @id
RETURNING *;

-- name: HideListing :one
UPDATE listings
SET visibility = 'hidden_by_admin', hidden_reason = @reason, hidden_at = @now, hidden_by = @actor, updated_at = @now
WHERE id = @id
RETURNING *;

-- name: UnhideListing :one
UPDATE listings
SET visibility = 'published', hidden_reason = NULL, hidden_at = NULL, hidden_by = NULL, updated_at = @now
WHERE id = @id
RETURNING *;

-- name: SetListingVisibility :one
UPDATE listings SET visibility = @visibility, updated_at = @now WHERE id = @id
RETURNING *;

-- name: CountListingImages :one
SELECT count(*) FROM listing_images WHERE listing_id = @listing_id;

-- name: CountActiveTicketTypes :one
SELECT count(*) FROM attraction_ticket_types WHERE listing_id = @listing_id AND active;

-- name: CountActiveTimeSlots :one
SELECT count(*) FROM attraction_time_slots WHERE listing_id = @listing_id AND active;

-- name: ListAdminListings :many
SELECT * FROM listings
WHERE (sqlc.narg(review_status)::text IS NULL OR review_status = sqlc.narg(review_status))
  AND (sqlc.narg(visibility)::text IS NULL OR visibility = sqlc.narg(visibility))
  AND (sqlc.narg(service_type)::text IS NULL OR service_type = sqlc.narg(service_type))
  AND (sqlc.narg(partner_id)::uuid IS NULL OR partner_id = sqlc.narg(partner_id))
  AND (sqlc.narg(cursor)::uuid IS NULL OR id < sqlc.narg(cursor))
ORDER BY id DESC
LIMIT @lim;

-- Review queue: oldest submission first; the cursor row's (submitted_at, id)
-- is the keyset position.
-- name: ListAdminPendingListings :many
SELECT * FROM listings
WHERE review_status = 'pending_review'
  AND (sqlc.narg(visibility)::text IS NULL OR visibility = sqlc.narg(visibility))
  AND (sqlc.narg(service_type)::text IS NULL OR service_type = sqlc.narg(service_type))
  AND (sqlc.narg(partner_id)::uuid IS NULL OR partner_id = sqlc.narg(partner_id))
  AND (sqlc.narg(cursor)::uuid IS NULL
       OR (submitted_at, id) > (SELECT c.submitted_at, c.id FROM listings c WHERE c.id = sqlc.narg(cursor)))
ORDER BY submitted_at, id
LIMIT @lim;

-- name: ListRecentAuditLog :many
SELECT * FROM listing_audit_log WHERE listing_id = @listing_id
ORDER BY created_at DESC, id DESC
LIMIT 20;
