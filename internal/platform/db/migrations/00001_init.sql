-- +goose Up
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- +goose Down
-- Extension is intentionally kept.
