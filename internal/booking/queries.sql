-- name: GetBooking :one
SELECT * FROM bookings WHERE id = @id;

-- Idempotency-Key (spec §7). ON CONFLICT DO NOTHING waits for an in-flight
-- insert of the same key: if that tx commits, nothing is inserted (0 rows);
-- if it rolls back, this insert goes through.
-- name: ReserveIdempotencyKey :execrows
INSERT INTO idempotency_keys (user_id, key, endpoint, request_hash, created_at, updated_at)
VALUES (@user_id, @key, @endpoint, @request_hash, @now, @now)
ON CONFLICT (user_id, key) DO NOTHING;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys WHERE user_id = @user_id AND key = @key;

-- name: StoreIdempotencyResponse :exec
UPDATE idempotency_keys
SET response_status = @response_status, response_body = @response_body, updated_at = @now
WHERE user_id = @user_id AND key = @key;

-- A code collision inserts nothing (0 rows) without aborting the tx.
-- name: InsertCheckout :execrows
INSERT INTO checkouts (id, code, customer_id, total_amount, status, created_at, updated_at)
VALUES (@id, @code, @customer_id, @total_amount, 'open', @now, @now)
ON CONFLICT (code) DO NOTHING;

-- name: InsertPendingBooking :execrows
INSERT INTO bookings (
    id, code, checkout_id, customer_id, partner_id, listing_id, service_type, booking_mode, status,
    service_start_at, service_end_at, original_service_start_at,
    gross_amount, commission_rate_bps, commission_amount, cancellation_policy_snapshot,
    contact_name, contact_phone, contact_email, customer_note,
    hold_expires_at, hold_release_at, qr_token, created_at, updated_at
) VALUES (
    @id, @code, @checkout_id, @customer_id, @partner_id, @listing_id, @service_type, 'instant', 'pending_payment',
    @service_start_at, @service_end_at, @service_start_at,
    @gross_amount, @commission_rate_bps, @commission_amount, @cancellation_policy_snapshot,
    @contact_name, @contact_phone, @contact_email, sqlc.narg(customer_note),
    @hold_expires_at, @hold_release_at, @qr_token, @now, @now
)
ON CONFLICT (code) DO NOTHING;

-- name: InsertBookingItem :exec
INSERT INTO booking_items (id, booking_id, line_kind, description, unit_price, quantity, line_total, created_at, updated_at)
VALUES (@id, @booking_id, @line_kind, @description, @unit_price, @quantity::int, @line_total, @now, @now);

-- name: InsertTicketLine :exec
INSERT INTO booking_ticket_lines (booking_item_id, booking_id, listing_id, ticket_type_id, created_at, updated_at)
VALUES (@booking_item_id, @booking_id, @listing_id, @ticket_type_id, @now, @now);

-- name: InsertAttractionDetail :exec
INSERT INTO booking_attraction_details (booking_id, listing_id, visit_date, time_slot_id, slot_start, slot_end, created_at, updated_at)
VALUES (@booking_id, @listing_id, (@visit_date::text)::date, @time_slot_id,
        (@slot_start::text)::time, (@slot_end::text)::time, @now, @now);

-- name: InsertStatusHistory :exec
INSERT INTO booking_status_history (id, booking_id, from_status, to_status, actor_kind, actor_user_id, reason, created_at, updated_at)
VALUES (@id, @booking_id, sqlc.narg(from_status), @to_status, @actor_kind, sqlc.narg(actor_user_id), sqlc.narg(reason), @now, @now);

-- name: GetBookingDetail :one
SELECT sqlc.embed(b), l.title AS listing_title, l.slug AS listing_slug
FROM bookings b
JOIN listings l ON l.id = b.listing_id
WHERE b.id = @id;

-- name: GetAttractionDetail :one
SELECT to_char(visit_date, 'YYYY-MM-DD')::text AS visit_date, time_slot_id,
       to_char(slot_start, 'HH24:MI')::text AS slot_start, to_char(slot_end, 'HH24:MI')::text AS slot_end
FROM booking_attraction_details
WHERE booking_id = @booking_id;

-- name: ListBookingItems :many
SELECT i.id, i.description, i.unit_price, i.quantity::float8 AS quantity, i.line_total, t.ticket_type_id
FROM booking_items i
LEFT JOIN booking_ticket_lines t ON t.booking_item_id = i.id
WHERE i.booking_id = @booking_id
ORDER BY i.id;

-- name: ListStatusHistory :many
SELECT * FROM booking_status_history WHERE booking_id = @booking_id ORDER BY created_at, id;

-- Booking lists: newest first (uuid v7 ids), keyset on id.
-- name: ListBookings :many
SELECT b.id, b.code, b.status, b.service_type, b.listing_id, l.title AS listing_title, l.slug AS listing_slug,
       b.service_start_at, b.service_end_at, b.gross_amount, b.hold_expires_at, b.created_at
FROM bookings b
JOIN listings l ON l.id = b.listing_id
WHERE (sqlc.narg(customer_id)::uuid IS NULL OR b.customer_id = sqlc.narg(customer_id))
  AND (sqlc.narg(partner_id)::uuid IS NULL OR b.partner_id = sqlc.narg(partner_id))
  AND (sqlc.narg(status)::text IS NULL OR b.status = sqlc.narg(status))
  AND (sqlc.narg(code)::text IS NULL OR b.code = sqlc.narg(code))
  AND (sqlc.narg(cursor)::uuid IS NULL OR b.id < sqlc.narg(cursor))
ORDER BY b.id DESC
LIMIT @lim;

-- name: GetCheckout :one
SELECT * FROM checkouts WHERE id = @id;

-- checkouts come before bookings in the lock order (DB §1.1).
-- name: LockCheckout :one
SELECT * FROM checkouts WHERE id = @id FOR UPDATE;

-- Unlocked read of a checkout's bookings with what re-confirmation needs.
-- name: ListCheckoutBookings :many
SELECT sqlc.embed(b),
       COALESCE(to_char(d.visit_date, 'YYYY-MM-DD'), '')::text AS visit_date,
       d.time_slot_id,
       COALESCE(to_char(d.slot_start, 'HH24:MI'), '')::text AS slot_start,
       COALESCE(to_char(d.slot_end, 'HH24:MI'), '')::text AS slot_end
FROM bookings b
LEFT JOIN booking_attraction_details d ON d.booking_id = b.id
WHERE b.checkout_id = @checkout_id
ORDER BY b.id;

-- Bookings of a checkout locked in id order (DB §1.1).
-- name: LockCheckoutBookings :many
SELECT * FROM bookings WHERE checkout_id = @checkout_id ORDER BY id FOR UPDATE;

-- name: ConfirmBooking :exec
UPDATE bookings SET status = 'confirmed', confirmed_at = @now::timestamptz, updated_at = @now WHERE id = @id;

-- The checkout becomes paid by the payment that confirmed it.
-- name: SetCheckoutPaid :exec
UPDATE checkouts SET status = 'paid', paid_payment_id = @paid_payment_id, updated_at = @now WHERE id = @id;

-- name: SetCheckoutStatus :exec
UPDATE checkouts SET status = @status, updated_at = @now WHERE id = @id;

-- name: ExpireBooking :exec
UPDATE bookings SET status = 'expired', updated_at = @now WHERE id = @id;

-- Hold expiry job: bookings past their hold release (bookings_hold_release_idx).
-- name: ListHoldsToRelease :many
SELECT id, checkout_id FROM bookings
WHERE status = 'pending_payment' AND hold_release_at <= @now::timestamptz
ORDER BY hold_release_at, id
LIMIT @lim;

-- No-show job: confirmed attraction bookings whose service ended
-- (bookings_service_end_idx).
-- name: ListNoShowCandidates :many
SELECT id FROM bookings
WHERE status = 'confirmed' AND service_type = 'attraction_ticket' AND service_end_at < @now
ORDER BY service_end_at, id
LIMIT @lim;

-- name: LockBooking :one
SELECT * FROM bookings WHERE id = @id FOR UPDATE;

-- Check-in: the partner's booking with this QR token, locked.
-- name: LockPartnerBookingByQR :one
SELECT * FROM bookings WHERE qr_token = @qr_token AND partner_id = @partner_id FOR UPDATE;

-- Check-in by hand: the partner's booking with this code, locked.
-- name: LockPartnerBookingByCode :one
SELECT * FROM bookings WHERE code = @code AND partner_id = @partner_id FOR UPDATE;

-- name: MarkNoShow :exec
UPDATE bookings SET status = 'no_show', no_show_at = @now::timestamptz, updated_at = @now WHERE id = @id;

-- name: CancelBooking :exec
UPDATE bookings
SET status = 'cancelled', cancelled_at = @now::timestamptz, cancel_kind = @cancel_kind::text,
    cancel_reason = sqlc.narg(cancel_reason), cancelled_by = sqlc.narg(cancelled_by), updated_at = @now
WHERE id = @id;

-- name: HasReschedule :one
SELECT EXISTS (SELECT 1 FROM booking_reschedules WHERE booking_id = @booking_id);

-- name: InsertReschedule :exec
INSERT INTO booking_reschedules (id, booking_id, listing_id, from_visit_date, from_slot_id, to_visit_date, to_slot_id,
                                 created_by, created_at, updated_at)
VALUES (@id, @booking_id, @listing_id, (@from_visit_date::text)::date, @from_slot_id, (@to_visit_date::text)::date,
        @to_slot_id, @created_by, @now, @now);

-- A reschedule moves the service; original_service_start_at never changes.
-- name: RescheduleBooking :exec
UPDATE bookings SET service_start_at = @service_start_at, service_end_at = @service_end_at, updated_at = @now
WHERE id = @id;

-- name: UpdateAttractionDetail :exec
UPDATE booking_attraction_details
SET visit_date = (@visit_date::text)::date, time_slot_id = @time_slot_id,
    slot_start = (@slot_start::text)::time, slot_end = (@slot_end::text)::time, updated_at = @now
WHERE booking_id = @booking_id;

-- Attraction check-in completes the booking at once.
-- name: CompleteCheckIn :exec
UPDATE bookings
SET status = 'completed', checked_in_at = @now::timestamptz, completed_at = @now::timestamptz, updated_at = @now
WHERE id = @id;

-- BookingGuard: active (pending_payment, confirmed) bookings of a listing on
-- a Vietnam calendar date.
-- name: CountActiveBookingsOnDate :one
SELECT count(*)::int FROM booking_attraction_details d
JOIN bookings b ON b.id = d.booking_id
WHERE d.listing_id = @listing_id AND d.visit_date = (@visit_date::text)::date
  AND b.status IN ('pending_payment', 'confirmed');

-- BookingGuard: active bookings on a time slot from a Vietnam date on whose
-- visit date's weekday is in the mask (isodow 1 = Monday = bit 0, as
-- listing.weekdayBit).
-- name: CountActiveBookingsForSlot :one
SELECT count(*)::int FROM booking_attraction_details d
JOIN bookings b ON b.id = d.booking_id
WHERE d.time_slot_id = @time_slot_id AND d.visit_date >= (@from_date::text)::date
  AND (@weekdays::int & (1 << (extract(isodow FROM d.visit_date)::int - 1))) <> 0
  AND b.status IN ('pending_payment', 'confirmed');
