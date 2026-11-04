-- +goose Up
CREATE INDEX user_identities_user_idx ON user_identities (user_id);

-- +goose Down
DROP INDEX user_identities_user_idx;
