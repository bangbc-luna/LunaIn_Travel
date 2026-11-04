-- +goose Up
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);

-- +goose Down
DROP INDEX refresh_tokens_user_idx;
