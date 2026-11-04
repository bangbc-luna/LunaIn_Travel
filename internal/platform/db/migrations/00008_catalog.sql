-- +goose Up
CREATE TABLE service_types (
    code         text PRIMARY KEY,
    name         text NOT NULL,
    booking_mode text NOT NULL CHECK (booking_mode IN ('instant','request')),
    sort_order   int  NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

INSERT INTO service_types (code, name, booking_mode, sort_order) VALUES
    ('attraction_ticket', 'Vé tham quan',     'instant', 1),
    ('scheduled_ride',    'Xe tuyến cố định', 'instant', 2),
    ('private_charter',   'Xe bao chuyến',    'request', 3),
    ('self_drive_rental', 'Thuê xe tự lái',   'request', 4);

-- Append-only history: a change is a new row. No rows are seeded on purpose, so
-- a service type without a configured rate cannot be sold at an accidental 0%.
CREATE TABLE commission_rates (
    id             uuid PRIMARY KEY,
    service_type   text NOT NULL REFERENCES service_types (code),
    rate_bps       int  NOT NULL CHECK (rate_bps BETWEEN 0 AND 10000),
    effective_from timestamptz NOT NULL,
    created_by     uuid NOT NULL REFERENCES users (id),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (service_type, effective_from)
);

-- +goose Down
DROP TABLE commission_rates;
DROP TABLE service_types;
