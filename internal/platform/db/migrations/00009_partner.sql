-- +goose Up
CREATE TABLE partners (
    id                       uuid PRIMARY KEY,
    legal_name               text NOT NULL CHECK (btrim(legal_name) <> ''),
    display_name             text NOT NULL CHECK (btrim(display_name) <> ''),
    tax_code                 text NULL UNIQUE,
    business_license_file_id uuid NULL REFERENCES files (id),
    address                  text NULL,
    province_code            text NULL REFERENCES provinces (code),
    ward_code                text NULL REFERENCES wards (code),
    contact_name             text NOT NULL DEFAULT '',
    contact_phone            text NOT NULL DEFAULT '',
    contact_email            text NOT NULL DEFAULT '',
    status                   text NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended')),
    suspended_at             timestamptz NULL,
    suspended_reason         text NULL,
    -- Average rating = rating_sum / rating_count; no float is stored.
    rating_sum               int  NOT NULL DEFAULT 0 CHECK (rating_sum >= 0),
    rating_count             int  NOT NULL DEFAULT 0 CHECK (rating_count >= 0),
    created_by               uuid NOT NULL REFERENCES users (id),
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    CHECK (status <> 'suspended' OR suspended_at IS NOT NULL)
);

-- A user belongs to at most one partner (UNIQUE user_id).
CREATE TABLE partner_members (
    partner_id uuid NOT NULL REFERENCES partners (id),
    user_id    uuid NOT NULL UNIQUE REFERENCES users (id),
    role       text NOT NULL CHECK (role IN ('owner')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (partner_id, user_id)
);

-- A row exists once a service type was ever enabled; disabled_at IS NULL
-- means it is currently enabled.
CREATE TABLE partner_service_types (
    partner_id   uuid NOT NULL REFERENCES partners (id),
    service_type text NOT NULL REFERENCES service_types (code),
    enabled_by   uuid NOT NULL REFERENCES users (id),
    enabled_at   timestamptz NOT NULL,
    disabled_at  timestamptz NULL,
    disabled_by  uuid NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (partner_id, service_type),
    CHECK ((disabled_at IS NULL) = (disabled_by IS NULL))
);

-- History of payout accounts: a change inserts a new row and clears
-- is_current on the old one.
CREATE TABLE partner_bank_accounts (
    id             uuid PRIMARY KEY,
    partner_id     uuid NOT NULL REFERENCES partners (id),
    bank_bin       text NOT NULL CHECK (bank_bin ~ '^[0-9]{6}$'),
    bank_name      text NOT NULL,
    account_number text NOT NULL CHECK (account_number ~ '^[0-9]{6,19}$'),
    account_holder text NOT NULL CHECK (btrim(account_holder) <> ''),
    is_current     bool NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX partner_bank_accounts_partner_idx ON partner_bank_accounts (partner_id, created_at DESC);
CREATE UNIQUE INDEX partner_bank_accounts_current_idx ON partner_bank_accounts (partner_id) WHERE is_current;

-- +goose Down
DROP TABLE partner_bank_accounts;
DROP TABLE partner_service_types;
DROP TABLE partner_members;
DROP TABLE partners;
