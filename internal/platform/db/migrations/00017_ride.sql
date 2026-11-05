-- +goose Up
-- Scheduled ride schema (DB §5.5, §6). Cross-partner integrity uses composite FKs (DB §1.2).

-- Minutes before departure at which a trip stops being bookable; the effective
-- cutoff is max(this, hold_minutes + hold_grace_minutes).
ALTER TABLE platform_settings
    ADD COLUMN ride_booking_cutoff_minutes int NOT NULL DEFAULT 60 CHECK (ride_booking_cutoff_minutes >= 0);

-- One-way route of a scheduled_ride listing.
CREATE TABLE ride_routes (
    listing_id           uuid PRIMARY KEY REFERENCES listings (id),
    origin_province_code text NOT NULL,
    origin_ward_code     text NOT NULL,
    origin_name          text NOT NULL CHECK (btrim(origin_name) <> ''),
    origin_address       text NOT NULL CHECK (btrim(origin_address) <> ''),
    dest_province_code   text NOT NULL,
    dest_ward_code       text NOT NULL,
    dest_name            text NOT NULL CHECK (btrim(dest_name) <> ''),
    dest_address         text NOT NULL CHECK (btrim(dest_address) <> ''),
    duration_minutes     int  NOT NULL CHECK (duration_minutes BETWEEN 1 AND 4320),
    vehicle_description  text NOT NULL CHECK (btrim(vehicle_description) <> ''),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (origin_ward_code, origin_province_code) REFERENCES wards (code, province_code),
    FOREIGN KEY (dest_ward_code, dest_province_code) REFERENCES wards (code, province_code)
);

CREATE INDEX ride_routes_origin_dest_idx ON ride_routes (origin_province_code, dest_province_code);

-- Recurring departure. weekdays is a bitmask: bit 0 = Monday ... bit 6 = Sunday.
-- generated_until is the last VN date the trip-generation job covered.
CREATE TABLE ride_schedules (
    id              uuid PRIMARY KEY,
    listing_id      uuid NOT NULL REFERENCES ride_routes (listing_id),
    departure_time  time NOT NULL,
    weekdays        smallint NOT NULL CHECK (weekdays BETWEEN 1 AND 127),
    seat_capacity   int NOT NULL CHECK (seat_capacity > 0),
    price_per_seat  bigint NOT NULL CHECK (price_per_seat > 0),
    valid_from      date NOT NULL,
    valid_to        date NULL,
    active          boolean NOT NULL DEFAULT true,
    generated_until date NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (valid_to IS NULL OR valid_to >= valid_from),
    -- Target of the composite FK from ride_trips.
    UNIQUE (id, listing_id)
);

CREATE INDEX ride_schedules_listing_idx ON ride_schedules (listing_id);

-- Concrete departure. schedule_id is NULL for a one-off trip. A trip of one
-- listing can never point at a schedule of another (composite FK).
CREATE TABLE ride_trips (
    id               uuid PRIMARY KEY,
    listing_id       uuid NOT NULL REFERENCES ride_routes (listing_id),
    schedule_id      uuid NULL,
    departure_at     timestamptz NOT NULL,
    seat_capacity    int NOT NULL CHECK (seat_capacity > 0),
    seats_reserved   int NOT NULL DEFAULT 0,
    price_per_seat   bigint NOT NULL CHECK (price_per_seat > 0),
    status           text NOT NULL DEFAULT 'scheduled' CHECK (status IN ('scheduled','cancelled','departed')),
    cancelled_reason text NULL,
    cancelled_at     timestamptz NULL,
    cancelled_by     uuid NULL REFERENCES users (id),
    cancel_source    text NULL CHECK (cancel_source IN ('partner','admin','schedule')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (schedule_id, listing_id) REFERENCES ride_schedules (id, listing_id),
    -- Last-line guard against overselling; the app also checks in its UPDATE.
    CONSTRAINT ride_trips_seats_check CHECK (seats_reserved BETWEEN 0 AND seat_capacity),
    CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL)),
    CHECK ((cancelled_at IS NULL) = (cancel_source IS NULL)),
    CHECK (status <> 'cancelled' OR cancelled_reason IS NOT NULL),
    UNIQUE (listing_id, departure_at),
    -- Target of the composite FK from booking_ride_details.
    UNIQUE (id, listing_id)
);

-- Trip search.
CREATE INDEX ride_trips_search_idx ON ride_trips (departure_at) WHERE status = 'scheduled';
CREATE INDEX ride_trips_schedule_idx ON ride_trips (schedule_id, departure_at);

-- Safety net: a trip with reserved seats keeps its time and price. The app
-- checks first and returns a proper error.
-- +goose StatementBegin
CREATE FUNCTION ride_trips_booked_immutable() RETURNS trigger AS $$
BEGIN
    IF OLD.seats_reserved > 0
       AND (NEW.departure_at IS DISTINCT FROM OLD.departure_at
            OR NEW.price_per_seat IS DISTINCT FROM OLD.price_per_seat) THEN
        RAISE EXCEPTION 'ride_trip_booked' USING ERRCODE = 'P0001';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER ride_trips_booked_immutable
    BEFORE UPDATE ON ride_trips
    FOR EACH ROW EXECUTE FUNCTION ride_trips_booked_immutable();

-- Snapshot of the trip a ride booking is for. origin_name / dest_name are
-- copied at booking time.
CREATE TABLE booking_ride_details (
    booking_id  uuid PRIMARY KEY,
    listing_id  uuid NOT NULL,
    trip_id     uuid NOT NULL,
    seats       int  NOT NULL CHECK (seats > 0),
    direction   text NOT NULL CHECK (direction IN ('outbound','return')),
    origin_name text NOT NULL,
    dest_name   text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (booking_id, listing_id) REFERENCES bookings (id, listing_id),
    FOREIGN KEY (trip_id, listing_id) REFERENCES ride_trips (id, listing_id)
);

CREATE INDEX booking_ride_details_trip_idx ON booking_ride_details (trip_id);

ALTER TABLE bookings
    ADD CONSTRAINT bookings_checked_in_at_check CHECK (status <> 'checked_in' OR checked_in_at IS NOT NULL);

-- +goose Down
ALTER TABLE bookings DROP CONSTRAINT bookings_checked_in_at_check;
DROP TABLE booking_ride_details;
DROP TRIGGER ride_trips_booked_immutable ON ride_trips;
DROP FUNCTION ride_trips_booked_immutable();
DROP TABLE ride_trips;
DROP TABLE ride_schedules;
DROP TABLE ride_routes;
ALTER TABLE platform_settings DROP COLUMN ride_booking_cutoff_minutes;
