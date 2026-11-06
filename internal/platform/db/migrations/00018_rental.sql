-- +goose Up
-- Self-drive rental schema (DB §5.7, §6). Cross-partner integrity uses composite FKs (DB §1.2).
-- The GiST index below mixes uuid and tstzrange, so it needs btree_gist (enabled in 00001).

-- Rental store of a self_drive_rental listing. The store location is the
-- listing's own province/ward/address.
CREATE TABLE rental_stores (
    listing_id         uuid PRIMARY KEY REFERENCES listings (id),
    -- Handover window in VN local time; no overnight window.
    handover_open      time NOT NULL,
    handover_close     time NOT NULL,
    delivery_available boolean NOT NULL DEFAULT false,
    deposit_terms      text NULL,
    documents_terms    text NULL,
    late_fee_terms     text NULL,
    allowed_area_terms text NULL,
    other_terms        text NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK (handover_close > handover_open)
);

CREATE TABLE rental_vehicle_models (
    id             uuid PRIMARY KEY,
    listing_id     uuid NOT NULL REFERENCES rental_stores (listing_id),
    vehicle_kind   text NOT NULL CHECK (vehicle_kind IN ('car','motorbike')),
    name           text NOT NULL CHECK (btrim(name) <> ''),
    model_year     smallint NOT NULL CHECK (model_year BETWEEN 1950 AND 2100),
    seats          int NOT NULL CHECK (seats BETWEEN 1 AND 50),
    included_items text NULL,
    tags           text[] NOT NULL DEFAULT '{}' CHECK (cardinality(tags) <= 10),
    price_per_day  bigint NOT NULL CHECK (price_per_day > 0),
    quantity       int NOT NULL CHECK (quantity >= 0),
    cover_file_id  uuid NULL REFERENCES files (id),
    active         boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    -- Target of the composite FK from booking_rental_details.
    UNIQUE (id, listing_id)
);

CREATE INDEX rental_vehicle_models_listing_idx ON rental_vehicle_models (listing_id);

CREATE TABLE rental_pickup_points (
    id            uuid PRIMARY KEY,
    listing_id    uuid NOT NULL REFERENCES rental_stores (listing_id),
    name          text NOT NULL CHECK (btrim(name) <> ''),
    address       text NOT NULL CHECK (btrim(address) <> ''),
    province_code text NOT NULL,
    ward_code     text NOT NULL,
    delivery_fee  bigint NOT NULL DEFAULT 0 CHECK (delivery_fee >= 0),
    active        boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (ward_code, province_code) REFERENCES wards (code, province_code),
    -- Target of the composite FK from booking_rental_details.
    UNIQUE (id, listing_id)
);

CREATE INDEX rental_pickup_points_listing_idx ON rental_pickup_points (listing_id);

-- Snapshot of a rental booking. period is generated and never written by the
-- app. holds_vehicle / hold_until mirror the booking status (set in the same
-- transaction as the status change).
CREATE TABLE booking_rental_details (
    booking_id                uuid PRIMARY KEY,
    listing_id                uuid NOT NULL,
    vehicle_model_id          uuid NOT NULL,
    pickup_point_id           uuid NOT NULL,
    pickup_at                 timestamptz NOT NULL,
    return_at                 timestamptz NOT NULL,
    period                    tstzrange GENERATED ALWAYS AS (tstzrange(pickup_at, return_at, '[)')) STORED,
    days                      int NOT NULL CHECK (days > 0),
    model_name                text NOT NULL,
    pickup_point_name         text NOT NULL,
    pickup_address            text NOT NULL,
    destination_province_code text NOT NULL REFERENCES provinces (code),
    holds_vehicle             boolean NOT NULL,
    hold_until                timestamptz NULL,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CHECK (return_at > pickup_at),
    CHECK (holds_vehicle OR hold_until IS NULL),
    FOREIGN KEY (booking_id, listing_id) REFERENCES bookings (id, listing_id),
    FOREIGN KEY (vehicle_model_id, listing_id) REFERENCES rental_vehicle_models (id, listing_id),
    FOREIGN KEY (pickup_point_id, listing_id) REFERENCES rental_pickup_points (id, listing_id)
);

CREATE INDEX booking_rental_details_hold_idx ON booking_rental_details USING gist (vehicle_model_id, period)
    WHERE holds_vehicle;

ALTER TABLE bookings
    ADD CONSTRAINT bookings_in_service_at_check CHECK (status <> 'in_service' OR in_service_at IS NOT NULL),
    ADD CONSTRAINT bookings_accepted_check CHECK (status <> 'awaiting_payment' OR (accepted_at IS NOT NULL AND payment_due_at IS NOT NULL)),
    ADD CONSTRAINT bookings_reject_reason_check CHECK (status <> 'rejected' OR reject_reason IS NOT NULL);

-- +goose Down
ALTER TABLE bookings
    DROP CONSTRAINT bookings_reject_reason_check,
    DROP CONSTRAINT bookings_accepted_check,
    DROP CONSTRAINT bookings_in_service_at_check;
DROP TABLE booking_rental_details;
DROP TABLE rental_pickup_points;
DROP TABLE rental_vehicle_models;
DROP TABLE rental_stores;
