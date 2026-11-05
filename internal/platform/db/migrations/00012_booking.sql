-- +goose Up
-- Booking schema (DB §6). Cross-partner integrity uses composite FKs (DB §1.2).

-- One customer payment covering one or more bookings.
CREATE TABLE checkouts (
    id           uuid PRIMARY KEY,
    code         text NOT NULL UNIQUE CHECK (btrim(code) <> ''),
    customer_id  uuid NOT NULL REFERENCES users (id),
    total_amount bigint NOT NULL CHECK (total_amount >= 0),
    status       text NOT NULL DEFAULT 'open' CHECK (status IN ('open','paid','closed')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX checkouts_customer_idx ON checkouts (customer_id, created_at DESC);

CREATE TABLE bookings (
    id                           uuid PRIMARY KEY,
    -- "LT" + 8 Crockford base32 characters.
    code                         text NOT NULL UNIQUE CHECK (code ~ '^LT[0-9A-HJKMNP-TV-Z]{8}$'),
    checkout_id                  uuid NOT NULL REFERENCES checkouts (id),
    customer_id                  uuid NOT NULL REFERENCES users (id),
    partner_id                   uuid NOT NULL REFERENCES partners (id),
    listing_id                   uuid NOT NULL,
    -- Snapshots taken at booking time.
    service_type                 text NOT NULL REFERENCES service_types (code),
    booking_mode                 text NOT NULL CHECK (booking_mode IN ('instant','request')),
    status                       text NOT NULL CHECK (status IN (
                                     'requested','awaiting_payment','pending_payment','confirmed',
                                     'checked_in','in_service','completed','no_show',
                                     'cancelled','expired','rejected')),
    service_start_at             timestamptz NOT NULL,
    service_end_at               timestamptz NOT NULL,
    -- Refund % after a reschedule is computed against this one (DB §6).
    original_service_start_at    timestamptz NOT NULL,
    gross_amount                 bigint NOT NULL CHECK (gross_amount >= 0),
    commission_rate_bps          int    NOT NULL CHECK (commission_rate_bps BETWEEN 0 AND 10000),
    commission_amount            bigint NOT NULL,
    -- [{hours_before, refund_bps}]
    cancellation_policy_snapshot jsonb NOT NULL,
    contact_name                 text NOT NULL,
    contact_phone                text NOT NULL,
    contact_email                text NOT NULL,
    customer_note                text NULL,
    -- hold_expires_at is what the customer sees; hold_release_at (= expires +
    -- buffer) is when the hold is really released.
    hold_expires_at              timestamptz NULL,
    hold_release_at              timestamptz NULL,
    -- Request Booking only.
    response_due_at              timestamptz NULL,
    payment_due_at               timestamptz NULL,
    accepted_at                  timestamptz NULL,
    rejected_at                  timestamptz NULL,
    reject_reason                text NULL,
    confirmed_at                 timestamptz NULL,
    checked_in_at                timestamptz NULL,
    in_service_at                timestamptz NULL,
    completed_at                 timestamptz NULL,
    no_show_at                   timestamptz NULL,
    cancelled_at                 timestamptz NULL,
    cancel_kind                  text NULL CHECK (cancel_kind IN
                                     ('customer','customer_withdraw','admin_force_majeure','admin_policy')),
    cancel_reason                text NULL,
    cancelled_by                 uuid NULL REFERENCES users (id),
    -- 32 random bytes, base64url; stored verbatim so the customer can show the QR again.
    qr_token                     text NOT NULL UNIQUE,
    created_at                   timestamptz NOT NULL DEFAULT now(),
    updated_at                   timestamptz NOT NULL DEFAULT now(),
    -- The partner must own the listing (DB §1.2).
    FOREIGN KEY (listing_id, partner_id) REFERENCES listings (id, partner_id),
    -- Targets of composite FKs from booking children and ledger tables.
    UNIQUE (id, partner_id),
    UNIQUE (id, listing_id),
    CHECK (commission_amount BETWEEN 0 AND gross_amount),
    CHECK (service_end_at >= service_start_at),
    -- requested / awaiting_payment / rejected exist only for Request Booking;
    -- pending_payment / checked_in only for Instant Booking.
    CHECK (booking_mode = 'request' OR status NOT IN ('requested','awaiting_payment','rejected')),
    CHECK (booking_mode = 'instant' OR status NOT IN ('pending_payment','checked_in')),
    CHECK ((hold_expires_at IS NULL) = (hold_release_at IS NULL)),
    CHECK (hold_release_at IS NULL OR hold_release_at >= hold_expires_at),
    -- Cancellation fields are set together, exactly when cancelled.
    CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL)),
    CHECK ((cancelled_at IS NULL) = (cancel_kind IS NULL)),
    CHECK (status <> 'rejected' OR rejected_at IS NOT NULL)
);

-- Background jobs (partial indexes, DB §6).
CREATE INDEX bookings_hold_release_idx ON bookings (hold_release_at)
    WHERE status IN ('pending_payment','awaiting_payment');
CREATE INDEX bookings_response_due_idx ON bookings (response_due_at)
    WHERE status = 'requested';
CREATE INDEX bookings_service_end_idx ON bookings (service_end_at)
    WHERE status IN ('confirmed','checked_in','in_service');
CREATE INDEX bookings_ride_no_show_idx ON bookings (service_start_at)
    WHERE status = 'confirmed' AND service_type = 'scheduled_ride';
-- Booking lists.
CREATE INDEX bookings_customer_idx ON bookings (customer_id, created_at DESC);
CREATE INDEX bookings_partner_idx ON bookings (partner_id, status, service_start_at);
CREATE INDEX bookings_checkout_idx ON bookings (checkout_id);
CREATE INDEX bookings_listing_idx ON bookings (listing_id, service_start_at);

CREATE TABLE booking_items (
    id          uuid PRIMARY KEY,
    booking_id  uuid NOT NULL REFERENCES bookings (id),
    line_kind   text NOT NULL CHECK (line_kind IN
                    ('ticket','seat','charter_base','charter_distance','surcharge','rental_day','delivery_fee')),
    description text NOT NULL,
    unit_price  bigint NOT NULL CHECK (unit_price >= 0),
    quantity    numeric(10,1) NOT NULL CHECK (quantity > 0),
    -- Postgres round() rounds half away from zero, i.e. half up for these
    -- non-negative values, matching money.MulBps (DB §1).
    line_total  bigint NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (line_total = round(unit_price * quantity)),
    -- Target of the composite FK from booking_ticket_lines.
    UNIQUE (id, booking_id)
);

CREATE INDEX booking_items_booking_idx ON booking_items (booking_id);

-- Ticket type of each ticket line. booking_id ties the line to its item's
-- booking, and (booking_id, listing_id) ties the ticket type's listing to the
-- booking's listing, so a line can never use another listing's ticket type.
CREATE TABLE booking_ticket_lines (
    booking_item_id uuid PRIMARY KEY,
    booking_id      uuid NOT NULL,
    listing_id      uuid NOT NULL,
    ticket_type_id  uuid NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (booking_item_id, booking_id) REFERENCES booking_items (id, booking_id),
    FOREIGN KEY (booking_id, listing_id) REFERENCES bookings (id, listing_id),
    FOREIGN KEY (ticket_type_id, listing_id) REFERENCES attraction_ticket_types (id, listing_id)
);

CREATE INDEX booking_ticket_lines_ticket_type_idx ON booking_ticket_lines (ticket_type_id);

-- Snapshot of the visit date and the slot's times.
CREATE TABLE booking_attraction_details (
    booking_id   uuid PRIMARY KEY,
    listing_id   uuid NOT NULL,
    visit_date   date NOT NULL,
    time_slot_id uuid NOT NULL,
    slot_start   time NOT NULL,
    slot_end     time NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (slot_end > slot_start),
    FOREIGN KEY (booking_id, listing_id) REFERENCES bookings (id, listing_id),
    FOREIGN KEY (time_slot_id, listing_id) REFERENCES attraction_time_slots (id, listing_id)
);

-- BookingGuard: bookings on a listing's date / on a time slot.
CREATE INDEX booking_attraction_details_listing_date_idx ON booking_attraction_details (listing_id, visit_date);
CREATE INDEX booking_attraction_details_slot_idx ON booking_attraction_details (time_slot_id, visit_date);

CREATE TABLE booking_status_history (
    id            uuid PRIMARY KEY,
    booking_id    uuid NOT NULL REFERENCES bookings (id),
    from_status   text NULL,
    to_status     text NOT NULL,
    actor_kind    text NOT NULL CHECK (actor_kind IN ('customer','partner','admin','system')),
    actor_user_id uuid NULL REFERENCES users (id),
    reason        text NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (actor_kind = 'system' OR actor_user_id IS NOT NULL)
);

CREATE INDEX booking_status_history_booking_idx ON booking_status_history (booking_id, created_at);

-- booking_id UNIQUE: the DB itself allows at most one reschedule per booking.
-- Both slots must belong to the booking's listing.
CREATE TABLE booking_reschedules (
    id              uuid PRIMARY KEY,
    booking_id      uuid NOT NULL UNIQUE,
    listing_id      uuid NOT NULL,
    from_visit_date date NOT NULL,
    from_slot_id    uuid NOT NULL,
    to_visit_date   date NOT NULL,
    to_slot_id      uuid NOT NULL,
    created_by      uuid NOT NULL REFERENCES users (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (booking_id, listing_id) REFERENCES bookings (id, listing_id),
    FOREIGN KEY (from_slot_id, listing_id) REFERENCES attraction_time_slots (id, listing_id),
    FOREIGN KEY (to_slot_id, listing_id) REFERENCES attraction_time_slots (id, listing_id)
);

-- Idempotency-Key of booking creation (spec §7). The response is stored once
-- the request finished; a cleanup job deletes rows older than 24 hours.
CREATE TABLE idempotency_keys (
    user_id         uuid NOT NULL REFERENCES users (id),
    key             text NOT NULL CHECK (btrim(key) <> ''),
    endpoint        text NOT NULL,
    request_hash    bytea NOT NULL,
    response_status int NULL,
    response_body   jsonb NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key),
    CHECK ((response_status IS NULL) = (response_body IS NULL))
);

CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);

-- +goose Down
DROP TABLE idempotency_keys;
DROP TABLE booking_reschedules;
DROP TABLE booking_status_history;
DROP TABLE booking_attraction_details;
DROP TABLE booking_ticket_lines;
DROP TABLE booking_items;
DROP TABLE bookings;
DROP TABLE checkouts;
