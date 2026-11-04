-- +goose Up
CREATE TABLE users (
    id                uuid PRIMARY KEY,
    email             text NOT NULL UNIQUE CHECK (email = lower(btrim(email))),
    password_hash     text NULL,
    email_verified_at timestamptz NULL,
    role              text NOT NULL CHECK (role IN ('customer', 'partner', 'super_admin')),
    status            text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'locked')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_identities (
    id               uuid PRIMARY KEY,
    user_id          uuid NOT NULL REFERENCES users (id),
    provider         text NOT NULL CHECK (provider IN ('google')),
    provider_subject text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_subject)
);

CREATE TABLE email_verifications (
    id          uuid PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users (id),
    purpose     text NOT NULL CHECK (purpose IN ('signup', 'password_reset')),
    code_hash     bytea NOT NULL,
    -- For signup: argon2id hash of the password submitted with the signup.
    -- It becomes users.password_hash only when the code is verified.
    password_hash text NULL,
    expires_at    timestamptz NOT NULL,
    attempts      int NOT NULL DEFAULT 0,
    consumed_at   timestamptz NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (purpose <> 'signup' OR password_hash IS NOT NULL)
);

CREATE INDEX email_verifications_user_purpose_created_idx
    ON email_verifications (user_id, purpose, created_at DESC);

CREATE TABLE refresh_tokens (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id),
    family_id  uuid NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz NULL,
    revoked_at timestamptz NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);

CREATE TABLE customer_profiles (
    user_id       uuid PRIMARY KEY REFERENCES users (id),
    full_name     text NULL,
    phone         text NULL,
    date_of_birth date NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE customer_profiles;
DROP TABLE refresh_tokens;
DROP TABLE email_verifications;
DROP TABLE user_identities;
DROP TABLE users;
