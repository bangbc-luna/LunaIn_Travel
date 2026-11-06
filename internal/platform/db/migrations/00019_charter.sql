-- +goose Up
-- Private charter schema (DB §5.6, §6). Cross-partner integrity uses composite FKs (DB §1.2).

-- Charter service of a private_charter listing. The night window is in VN
-- local time and may cross midnight (night_start > night_end).
CREATE TABLE charter_services (
    listing_id      uuid PRIMARY KEY REFERENCES listings (id),
    max_distance_km numeric(7,1) NULL CHECK (max_distance_km > 0),
    night_start     time NULL,
    night_end       time NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((night_start IS NULL) = (night_end IS NULL)),
    CHECK (night_start <> night_end)
);

-- Provinces where pickup is served.
CREATE TABLE charter_service_areas (
    listing_id    uuid NOT NULL REFERENCES charter_services (listing_id),
    province_code text NOT NULL REFERENCES provinces (code),
    PRIMARY KEY (listing_id, province_code)
);

CREATE TABLE charter_vehicle_classes (
    id           uuid PRIMARY KEY,
    listing_id   uuid NOT NULL REFERENCES charter_services (listing_id),
    name         text NOT NULL CHECK (btrim(name) <> ''),
    seats        int NOT NULL CHECK (seats BETWEEN 1 AND 50),
    base_fare    bigint NOT NULL CHECK (base_fare >= 0),
    price_per_km bigint NOT NULL CHECK (price_per_km > 0),
    active       boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    -- Target of the composite FK from booking_charter_details.
    UNIQUE (id, listing_id)
);

CREATE INDEX charter_vehicle_classes_listing_idx ON charter_vehicle_classes (listing_id);

-- amount is VND when calc = 'fixed', basis points when calc = 'percent'.
-- Night and round_trip_waiting surcharges are fixed-only.
CREATE TABLE charter_surcharges (
    id               uuid PRIMARY KEY,
    vehicle_class_id uuid NOT NULL REFERENCES charter_vehicle_classes (id),
    kind             text NOT NULL CHECK (kind IN ('night','weekend','holiday','round_trip_waiting')),
    calc             text NOT NULL CHECK (calc IN ('fixed','percent')),
    amount           bigint NOT NULL CHECK (amount >= 0),
    active           boolean NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (vehicle_class_id, kind),
    CHECK (kind IN ('weekend','holiday') OR calc = 'fixed'),
    CHECK (calc <> 'percent' OR amount <= 10000)
);

-- Snapshot of a charter booking: names, seats, province and duration are
-- copied at booking time so later edits of the listing do not change history.
CREATE TABLE booking_charter_details (
    booking_id          uuid PRIMARY KEY,
    listing_id          uuid NOT NULL,
    vehicle_class_id    uuid NOT NULL,
    pickup_at           timestamptz NOT NULL,
    pickup_address      text NOT NULL CHECK (btrim(pickup_address) <> ''),
    pickup_lat          numeric(9,6) NOT NULL CHECK (pickup_lat BETWEEN -90 AND 90),
    pickup_lng          numeric(9,6) NOT NULL CHECK (pickup_lng BETWEEN -180 AND 180),
    dropoff_address     text NOT NULL CHECK (btrim(dropoff_address) <> ''),
    dropoff_lat         numeric(9,6) NOT NULL CHECK (dropoff_lat BETWEEN -90 AND 90),
    dropoff_lng         numeric(9,6) NOT NULL CHECK (dropoff_lng BETWEEN -180 AND 180),
    distance_km         numeric(7,1) NOT NULL CHECK (distance_km > 0),
    is_round_trip       boolean NOT NULL,
    return_at           timestamptz NULL,
    quote               jsonb NOT NULL,
    pickup_province_code text NOT NULL REFERENCES provinces (code),
    duration_minutes    int NOT NULL CHECK (duration_minutes > 0),
    vehicle_class_name  text NOT NULL,
    seats               int NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (is_round_trip = (return_at IS NOT NULL)),
    CHECK (return_at IS NULL OR return_at > pickup_at),
    FOREIGN KEY (booking_id, listing_id) REFERENCES bookings (id, listing_id),
    FOREIGN KEY (vehicle_class_id, listing_id) REFERENCES charter_vehicle_classes (id, listing_id)
);

CREATE INDEX booking_charter_details_class_idx ON booking_charter_details (vehicle_class_id);

-- +goose Down
DROP TABLE booking_charter_details;
DROP TABLE charter_surcharges;
DROP TABLE charter_vehicle_classes;
DROP TABLE charter_service_areas;
DROP TABLE charter_services;
