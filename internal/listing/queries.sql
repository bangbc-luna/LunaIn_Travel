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

-- Hide remembers the visibility it replaces (SET reads the old row), so
-- Unhide can restore the partner's choice.
-- name: HideListing :one
UPDATE listings
SET visibility = 'hidden_by_admin', hidden_from_visibility = visibility,
    hidden_reason = @reason, hidden_at = @now, hidden_by = @actor, updated_at = @now
WHERE id = @id
RETURNING *;

-- name: UnhideListing :one
UPDATE listings
SET visibility = hidden_from_visibility, hidden_from_visibility = NULL,
    hidden_reason = NULL, hidden_at = NULL, hidden_by = NULL, updated_at = @now
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

-- Public search over visible_listings (the single visibility rule, see
-- migration 00010). price_from is the cheapest active ticket type, 0 when
-- there is none (prices are > 0 by CHECK; sqlc cannot type a nullable
-- aggregate, so 0 means NULL). With q: ILIKE on the escaped pattern
-- (ESCAPE '\') or trigram similarity >= 0.3, best match first; without q:
-- newest first.
-- name: SearchPublicListings :many
SELECT sqlc.embed(l),
       (SELECT COALESCE(min(tt.price), 0) FROM attraction_ticket_types tt
        WHERE tt.listing_id = l.id AND tt.active)::bigint AS price_from
FROM visible_listings l
WHERE (sqlc.narg(service_type)::text IS NULL OR l.service_type = sqlc.narg(service_type))
  AND (sqlc.narg(province_code)::text IS NULL OR l.province_code = sqlc.narg(province_code))
  AND (sqlc.narg(q)::text IS NULL
       OR public.f_unaccent(lower(l.title)) ILIKE '%' || public.f_unaccent(lower(sqlc.narg(pattern)::text)) || '%' ESCAPE '\'
       OR similarity(public.f_unaccent(lower(l.title)), public.f_unaccent(lower(sqlc.narg(q)))) >= 0.3)
ORDER BY CASE WHEN sqlc.narg(q)::text IS NULL THEN 0
              ELSE similarity(public.f_unaccent(lower(l.title)), public.f_unaccent(lower(sqlc.narg(q)))) END DESC,
         l.id DESC
LIMIT @lim OFFSET @off;

-- name: GetPublicListingBySlug :one
SELECT sqlc.embed(l),
       (SELECT COALESCE(min(tt.price), 0) FROM attraction_ticket_types tt
        WHERE tt.listing_id = l.id AND tt.active)::bigint AS price_from
FROM visible_listings l
WHERE l.slug = @slug;

-- name: ListingPartnerID :one
SELECT partner_id FROM listings WHERE id = @id;

-- BookableListing: FOR SHARE blocks partner edits (FOR NO KEY UPDATE) until
-- the booking tx ends, but not other bookings. Taken after the partners row.
-- name: ShareListing :one
SELECT * FROM listings WHERE id = @id FOR SHARE;

-- name: IsListingVisible :one
SELECT EXISTS (SELECT 1 FROM visible_listings WHERE id = @id);

-- name: GetRideTrip :one
SELECT * FROM ride_trips WHERE id = @id;

-- Scheduled ride: route, schedules and trip generation.
-- name: GetRideRoute :one
SELECT * FROM ride_routes WHERE listing_id = @listing_id;

-- name: UpsertRideRoute :one
INSERT INTO ride_routes (listing_id, origin_province_code, origin_ward_code, origin_name, origin_address,
    dest_province_code, dest_ward_code, dest_name, dest_address, duration_minutes, vehicle_description,
    created_at, updated_at)
VALUES (@listing_id, @origin_province_code, @origin_ward_code, @origin_name, @origin_address,
    @dest_province_code, @dest_ward_code, @dest_name, @dest_address, @duration_minutes, @vehicle_description,
    @now, @now)
ON CONFLICT (listing_id) DO UPDATE
SET origin_province_code = EXCLUDED.origin_province_code, origin_ward_code = EXCLUDED.origin_ward_code,
    origin_name = EXCLUDED.origin_name, origin_address = EXCLUDED.origin_address,
    dest_province_code = EXCLUDED.dest_province_code, dest_ward_code = EXCLUDED.dest_ward_code,
    dest_name = EXCLUDED.dest_name, dest_address = EXCLUDED.dest_address,
    duration_minutes = EXCLUDED.duration_minutes, vehicle_description = EXCLUDED.vehicle_description,
    updated_at = EXCLUDED.updated_at
RETURNING *;

-- name: HasBookedFutureTrips :one
SELECT EXISTS (SELECT 1 FROM ride_trips
               WHERE listing_id = @listing_id AND departure_at > @now AND seats_reserved > 0);

-- name: InsertRideSchedule :one
INSERT INTO ride_schedules (id, listing_id, departure_time, weekdays, seat_capacity, price_per_seat,
    valid_from, valid_to, active, created_at, updated_at)
VALUES (@id, @listing_id, @departure_time, @weekdays, @seat_capacity, @price_per_seat,
    @valid_from, sqlc.narg(valid_to), true, @now, @now)
RETURNING *;

-- name: LockRideSchedule :one
SELECT * FROM ride_schedules WHERE id = @id AND listing_id = @listing_id FOR UPDATE;

-- name: LockRideScheduleByID :one
SELECT * FROM ride_schedules WHERE id = @id FOR UPDATE;

-- name: ListRideSchedules :many
SELECT * FROM ride_schedules
WHERE listing_id = @listing_id AND (@include_inactive::bool OR active)
ORDER BY departure_time, valid_from, id;

-- name: UpdateRideSchedule :one
UPDATE ride_schedules
SET seat_capacity = @seat_capacity, price_per_seat = @price_per_seat, valid_to = sqlc.narg(valid_to),
    active = @active, generated_until = sqlc.narg(generated_until), updated_at = @now
WHERE id = @id
RETURNING *;

-- name: SetScheduleGeneratedUntil :exec
UPDATE ride_schedules SET generated_until = @generated_until, updated_at = @now WHERE id = @id;

-- name: CountActiveRideSchedules :one
SELECT count(*) FROM ride_schedules
WHERE listing_id = @listing_id AND active AND (valid_to IS NULL OR valid_to >= @today::date);

-- Schedules the job has work for: active ones not expired and not yet
-- generated up to min(valid_to, horizon), and any schedule that still has an
-- out-of-range trip to sweep (see SweepScheduleTrips).
-- name: ListSchedulesToGenerate :many
SELECT s.id FROM ride_schedules s
WHERE (s.active
       AND (s.valid_to IS NULL OR s.valid_to >= @today::date)
       AND (s.generated_until IS NULL OR s.generated_until < LEAST(COALESCE(s.valid_to, @horizon::date), @horizon::date)))
   OR EXISTS (SELECT 1 FROM ride_trips t
              WHERE t.schedule_id = s.id AND t.departure_at > @now::timestamptz
                AND t.status = 'scheduled' AND t.seats_reserved = 0
                AND (NOT s.active
                     OR (s.valid_to IS NOT NULL AND (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::date > s.valid_to)))
ORDER BY s.id;

-- A trip that schedule @schedule_id no longer covers (it is inactive, or the
-- trip's VN date is after its valid_to) may be covered by another active
-- schedule of the listing: same VN departure time, the trip's VN weekday bit
-- set (bit 0 = Monday) and its VN date within [valid_from, valid_to]. That
-- happens when a partner replaces a schedule while a trip held seats (the
-- replacement's generation skips the existing row). The overlap rule allows
-- at most one such schedule per trip.
-- name: ListCoveringSchedules :many
SELECT DISTINCT b.id
FROM ride_trips t
JOIN ride_schedules s ON s.id = t.schedule_id
JOIN ride_schedules b ON b.listing_id = t.listing_id AND b.id <> s.id AND b.active
WHERE s.id = @schedule_id::uuid AND t.departure_at > @now::timestamptz
  AND t.status = 'scheduled' AND t.seats_reserved = 0
  AND (NOT s.active
       OR (s.valid_to IS NOT NULL AND (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::date > s.valid_to))
  AND b.departure_time = (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::time
  AND (b.weekdays::int & (1 << (extract(isodow FROM t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::int - 1))) <> 0
  AND (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::date >= b.valid_from
  AND (b.valid_to IS NULL OR (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::date <= b.valid_to)
ORDER BY b.id;

-- Hands the unbooked trips of @schedule_id that it no longer covers but
-- @cover_id (locked by the caller) does over to @cover_id, with its capacity
-- and price, so that its later edits apply to them.
-- name: ReassignCoveredTrips :execrows
UPDATE ride_trips t
SET schedule_id = b.id, seat_capacity = b.seat_capacity, price_per_seat = b.price_per_seat, updated_at = @now::timestamptz
FROM ride_schedules s, ride_schedules b
WHERE s.id = @schedule_id::uuid AND t.schedule_id = s.id AND b.id = @cover_id::uuid
  AND b.listing_id = t.listing_id AND b.id <> s.id AND b.active
  AND t.departure_at > @now::timestamptz AND t.status = 'scheduled' AND t.seats_reserved = 0
  AND (NOT s.active
       OR (s.valid_to IS NOT NULL AND (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::date > s.valid_to))
  AND b.departure_time = (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::time
  AND (b.weekdays::int & (1 << (extract(isodow FROM t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::int - 1))) <> 0
  AND (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::date >= b.valid_from
  AND (b.valid_to IS NULL OR (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::date <= b.valid_to);

-- Cancels the future, unbooked, still scheduled trips of a schedule that it
-- no longer covers (schedule inactive, or the trip's VN date after
-- valid_to): trips kept by a schedule edit because they had seats held, once
-- those seats are released. A trip another active schedule covers (see
-- ListCoveringSchedules) is never cancelled here. seats_reserved = 0 is
-- re-checked per row, so a concurrent reservation wins.
-- name: SweepScheduleTrips :execrows
UPDATE ride_trips t
SET status = 'cancelled', cancelled_reason = @reason::text, cancelled_at = @now::timestamptz,
    cancelled_by = NULL, cancel_source = 'schedule', updated_at = @now::timestamptz
FROM ride_schedules s
WHERE s.id = @schedule_id::uuid AND t.schedule_id = s.id AND t.departure_at > @now::timestamptz
  AND t.status = 'scheduled' AND t.seats_reserved = 0
  AND (NOT s.active
       OR (s.valid_to IS NOT NULL AND (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::date > s.valid_to))
  AND NOT EXISTS (
      SELECT 1 FROM ride_schedules b
      WHERE b.listing_id = t.listing_id AND b.id <> s.id AND b.active
        AND b.departure_time = (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::time
        AND (b.weekdays::int & (1 << (extract(isodow FROM t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::int - 1))) <> 0
        AND (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::date >= b.valid_from
        AND (b.valid_to IS NULL OR (t.departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::date <= b.valid_to));

-- Inserts the schedule's trips; a trip the schedule itself cancelled at the
-- same departure is reopened. Any other existing trip (one-off, running, or
-- cancelled by the partner or an admin) is left as it is.
-- name: InsertScheduleTrips :execrows
INSERT INTO ride_trips (id, listing_id, schedule_id, departure_at, seat_capacity, price_per_seat, created_at, updated_at)
-- The two unnest calls in the select list run in lockstep (equal lengths).
SELECT unnest(@ids::uuid[]), @listing_id::uuid, @schedule_id::uuid, unnest(@departures::timestamptz[]),
       @seat_capacity::int, @price_per_seat::bigint, @now::timestamptz, @now::timestamptz
ON CONFLICT (listing_id, departure_at) DO UPDATE
SET status = 'scheduled', schedule_id = EXCLUDED.schedule_id, seat_capacity = EXCLUDED.seat_capacity,
    price_per_seat = EXCLUDED.price_per_seat, cancelled_reason = NULL, cancelled_at = NULL,
    cancelled_by = NULL, cancel_source = NULL, updated_at = EXCLUDED.updated_at
WHERE ride_trips.status = 'cancelled' AND ride_trips.cancel_source = 'schedule';

-- Future, unbooked, still scheduled trips of a schedule take its new
-- capacity and price; trips with reserved seats keep theirs.
-- name: UpdateOpenScheduleTrips :execrows
UPDATE ride_trips
SET seat_capacity = @seat_capacity::int, price_per_seat = @price_per_seat::bigint, updated_at = @now::timestamptz
WHERE schedule_id = @schedule_id::uuid AND departure_at > @now::timestamptz AND status = 'scheduled' AND seats_reserved = 0;

-- Cancels the future, unbooked, still scheduled trips of a schedule that
-- depart at or after @from_at.
-- name: CancelOpenScheduleTrips :execrows
UPDATE ride_trips
SET status = 'cancelled', cancelled_reason = @reason::text, cancelled_at = @now::timestamptz,
    cancelled_by = sqlc.narg(actor)::uuid, cancel_source = 'schedule', updated_at = @now::timestamptz
WHERE schedule_id = @schedule_id::uuid AND departure_at > @now::timestamptz AND departure_at >= @from_at::timestamptz
  AND status = 'scheduled' AND seats_reserved = 0;
