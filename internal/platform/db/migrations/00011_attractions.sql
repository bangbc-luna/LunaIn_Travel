-- +goose Up
-- Details of attraction_ticket listings (DB §5.4). That a child row belongs to
-- an attraction_ticket listing is enforced by the app (listing.Service, 422
-- wrong_service_type) under the listings row lock, not by a CHECK that reads
-- another table.
CREATE TABLE attractions (
    listing_id          uuid PRIMARY KEY REFERENCES listings (id),
    opening_note        text NULL,
    checkin_instructions text NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Never deleted (bookings reference them later); deactivate with active = false.
CREATE TABLE attraction_ticket_types (
    id          uuid PRIMARY KEY,
    listing_id  uuid NOT NULL REFERENCES attractions (listing_id),
    name        text NOT NULL CHECK (btrim(name) <> ''),
    description text NULL,
    audience    text NOT NULL CHECK (audience IN ('child','adult')),
    price       bigint NOT NULL CHECK (price > 0),
    active      boolean NOT NULL DEFAULT true,
    sort_order  int NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, listing_id)
);

CREATE INDEX attraction_ticket_types_listing_idx ON attraction_ticket_types (listing_id, sort_order, name);

-- weekdays is a bitmask: bit 0 = Monday ... bit 6 = Sunday.
CREATE TABLE attraction_time_slots (
    id         uuid PRIMARY KEY,
    listing_id uuid NOT NULL REFERENCES attractions (listing_id),
    start_time time NOT NULL,
    end_time   time NOT NULL,
    weekdays   smallint NOT NULL CHECK (weekdays BETWEEN 1 AND 127),
    active     boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (end_time > start_time),
    UNIQUE (id, listing_id)
);

CREATE INDEX attraction_time_slots_listing_idx ON attraction_time_slots (listing_id, start_time);

CREATE TABLE attraction_closed_dates (
    listing_id uuid NOT NULL REFERENCES attractions (listing_id),
    date       date NOT NULL,
    reason     text NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (listing_id, date)
);

-- +goose Down
DROP TABLE attraction_closed_dates;
DROP TABLE attraction_time_slots;
DROP TABLE attraction_ticket_types;
DROP TABLE attractions;
