-- +goose Up
-- Administrative units after the 2025 merger (Quyết định 19/2025/QĐ-TTg).
-- Rows are seeded by 00007_geo_seed.sql, generated from internal/geo/data.
CREATE TABLE provinces (
    code       text PRIMARY KEY,
    name       text NOT NULL,
    slug       text NOT NULL UNIQUE,
    sort_order int  NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE wards (
    code          text PRIMARY KEY,
    province_code text NOT NULL REFERENCES provinces (code),
    name          text NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('xa', 'phuong', 'dac_khu')),
    slug          text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (province_code, slug)
);

CREATE INDEX wards_province_code_idx ON wards (province_code);

-- +goose Down
DROP TABLE wards;
DROP TABLE provinces;
