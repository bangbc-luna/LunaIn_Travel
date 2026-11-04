-- +goose Up
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;

-- unaccent() is only STABLE (it depends on the search_path dictionary), so it
-- cannot back an index. This wrapper pins the dictionary explicitly and is
-- declared IMMUTABLE so f_unaccent(lower(title)) can be indexed (DB §5.3).
-- +goose StatementBegin
CREATE FUNCTION f_unaccent(text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
AS $$ SELECT public.unaccent('public.unaccent', $1) $$;
-- +goose StatementEnd

CREATE TABLE listings (
    id               uuid PRIMARY KEY,
    partner_id       uuid NOT NULL REFERENCES partners (id),
    service_type     text NOT NULL REFERENCES service_types (code),
    title            text NOT NULL CHECK (btrim(title) <> ''),
    -- Fixed at creation so public URLs stay stable when the title changes.
    slug             text NOT NULL UNIQUE,
    summary          text NULL,
    description      text NULL,
    cover_file_id    uuid NULL REFERENCES files (id),
    province_code    text NULL REFERENCES provinces (code),
    ward_code        text NULL,
    address          text NULL,
    latitude         numeric(9,6) NULL CHECK (latitude BETWEEN -90 AND 90),
    longitude        numeric(9,6) NULL CHECK (longitude BETWEEN -180 AND 180),
    review_status    text NOT NULL DEFAULT 'draft'
                     CHECK (review_status IN ('draft','pending_review','approved','rejected')),
    rejection_reason text NULL,
    submitted_at     timestamptz NULL,
    reviewed_at      timestamptz NULL,
    reviewed_by      uuid NULL REFERENCES users (id),
    visibility       text NOT NULL DEFAULT 'published'
                     CHECK (visibility IN ('published','unpublished_by_partner','hidden_by_admin')),
    hidden_reason    text NULL,
    hidden_at        timestamptz NULL,
    hidden_by        uuid NULL REFERENCES users (id),
    -- The visibility the listing had when an admin hid it; Unhide restores it.
    hidden_from_visibility text NULL
                     CHECK (hidden_from_visibility IN ('published','unpublished_by_partner')),
    -- Average rating = rating_sum / rating_count; no float is stored.
    rating_sum       int  NOT NULL DEFAULT 0 CHECK (rating_sum >= 0),
    rating_count     int  NOT NULL DEFAULT 0 CHECK (rating_count >= 0),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    -- Target of composite FKs (bookings.(listing_id, partner_id)), DB §1.2.
    UNIQUE (id, partner_id),
    -- The ward must belong to the province; province and ward are set or
    -- cleared together (so MATCH SIMPLE never skips a half-null pair).
    FOREIGN KEY (ward_code, province_code) REFERENCES wards (code, province_code),
    CHECK ((province_code IS NULL) = (ward_code IS NULL)),
    CHECK ((latitude IS NULL) = (longitude IS NULL)),
    CHECK (review_status <> 'rejected' OR rejection_reason IS NOT NULL),
    CHECK (visibility <> 'hidden_by_admin' OR hidden_reason IS NOT NULL),
    CHECK (visibility <> 'hidden_by_admin' OR hidden_from_visibility IS NOT NULL)
);

CREATE INDEX listings_partner_idx ON listings (partner_id, id DESC);
CREATE INDEX listings_public_idx ON listings (service_type, province_code)
    WHERE review_status = 'approved' AND visibility = 'published';
CREATE INDEX listings_title_trgm_idx ON listings USING gin (f_unaccent(lower(title)) gin_trgm_ops);

-- Every partner-side edit of a listing, with changes = {field: {old, new}}.
CREATE TABLE listing_audit_log (
    id            uuid PRIMARY KEY,
    listing_id    uuid NOT NULL REFERENCES listings (id),
    actor_user_id uuid NOT NULL REFERENCES users (id),
    action        text NOT NULL,
    changes       jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX listing_audit_log_listing_idx ON listing_audit_log (listing_id, created_at DESC);

-- Gallery of a listing; sort_order is the position in the partner's list.
CREATE TABLE listing_images (
    id         uuid PRIMARY KEY,
    listing_id uuid NOT NULL REFERENCES listings (id),
    file_id    uuid NOT NULL REFERENCES files (id),
    sort_order int  NOT NULL CHECK (sort_order >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (listing_id, sort_order),
    UNIQUE (listing_id, file_id)
);

-- Refund tiers: cancelling at least hours_before hours ahead refunds refund_bps/10000.
CREATE TABLE listing_cancellation_tiers (
    id           uuid PRIMARY KEY,
    listing_id   uuid NOT NULL REFERENCES listings (id),
    hours_before int  NOT NULL CHECK (hours_before BETWEEN 0 AND 8760),
    refund_bps   int  NOT NULL CHECK (refund_bps BETWEEN 0 AND 10000),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (listing_id, hours_before)
);

-- +goose Down
DROP TABLE listing_cancellation_tiers;
DROP TABLE listing_images;
DROP TABLE listing_audit_log;
DROP TABLE listings;
DROP FUNCTION f_unaccent(text);
