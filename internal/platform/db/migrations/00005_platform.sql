-- +goose Up
CREATE TABLE admin_audit_log (
    id            uuid PRIMARY KEY,
    actor_user_id uuid NOT NULL REFERENCES users (id),
    action        text NOT NULL,
    target_kind   text NOT NULL,
    target_id     uuid NULL,
    before        jsonb NULL,
    after         jsonb NULL,
    reason        text NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX admin_audit_log_target_idx
    ON admin_audit_log (target_kind, target_id, created_at DESC);

-- Single-row table: one typed column per setting so the DB rejects bad values.
CREATE TABLE platform_settings (
    id                             smallint PRIMARY KEY CHECK (id = 1),
    hold_minutes                   int    NOT NULL DEFAULT 15     CHECK (hold_minutes > 0),
    hold_grace_minutes             int    NOT NULL DEFAULT 5      CHECK (hold_grace_minutes >= 0),
    request_response_hours         int    NOT NULL DEFAULT 12     CHECK (request_response_hours > 0),
    request_payment_hours          int    NOT NULL DEFAULT 2      CHECK (request_payment_hours > 0),
    charter_late_confirm_min_hours int    NOT NULL DEFAULT 3      CHECK (charter_late_confirm_min_hours >= 0),
    ride_checkin_window_minutes    int    NOT NULL DEFAULT 60     CHECK (ride_checkin_window_minutes > 0),
    ride_no_show_after_minutes     int    NOT NULL DEFAULT 60     CHECK (ride_no_show_after_minutes >= 0),
    charter_auto_complete_hours    int    NOT NULL DEFAULT 24     CHECK (charter_auto_complete_hours > 0),
    release_delay_days             int    NOT NULL DEFAULT 1      CHECK (release_delay_days >= 0),
    min_payout_amount              bigint NOT NULL DEFAULT 100000 CHECK (min_payout_amount >= 0),
    reschedule_window_days         int    NOT NULL DEFAULT 14     CHECK (reschedule_window_days > 0),
    updated_by                     uuid   NULL REFERENCES users (id),
    created_at                     timestamptz NOT NULL DEFAULT now(),
    updated_at                     timestamptz NOT NULL DEFAULT now()
);

INSERT INTO platform_settings (id) VALUES (1);

CREATE TABLE holidays (
    date       date PRIMARY KEY,
    name       text NOT NULL CHECK (btrim(name) <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE files (
    id           uuid PRIMARY KEY,
    storage_key  text NOT NULL UNIQUE,
    purpose      text NOT NULL CHECK (purpose IN ('listing_image','review_image','business_license','payout_proof','refund_proof')),
    visibility   text NOT NULL CHECK (visibility IN ('public','private')),
    content_type text NOT NULL,
    size_bytes   bigint NOT NULL CHECK (size_bytes > 0),
    uploaded_by  uuid NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE files;
DROP TABLE holidays;
DROP TABLE platform_settings;
DROP TABLE admin_audit_log;
