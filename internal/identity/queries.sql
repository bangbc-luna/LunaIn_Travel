-- name: InsertUser :exec
INSERT INTO users (id, email, password_hash, role, created_at, updated_at)
VALUES (@id, @email, @password_hash, @role, @now, @now);

-- name: InsertCustomerProfile :exec
INSERT INTO customer_profiles (user_id, created_at, updated_at)
VALUES (@user_id, @now, @now);

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = @email;

-- name: GetUserByEmailForUpdate :one
SELECT * FROM users WHERE email = @email FOR UPDATE;

-- name: UpdateUserPassword :exec
UPDATE users SET password_hash = @password_hash, updated_at = @now WHERE id = @id;

-- name: MarkEmailVerified :exec
UPDATE users SET email_verified_at = @now::timestamptz, updated_at = @now::timestamptz
WHERE id = @id AND email_verified_at IS NULL;

-- name: InsertVerification :exec
INSERT INTO email_verifications (id, user_id, purpose, code_hash, expires_at, created_at, updated_at)
VALUES (@id, @user_id, @purpose, @code_hash, @expires_at, @now, @now);

-- name: GetLatestCodeCreatedAt :one
SELECT created_at FROM email_verifications
WHERE user_id = @user_id AND purpose = @purpose
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetLatestActiveCodeForUpdate :one
SELECT * FROM email_verifications
WHERE user_id = @user_id AND purpose = @purpose AND consumed_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT 1
FOR UPDATE;

-- name: IncrementCodeAttempts :exec
UPDATE email_verifications SET attempts = attempts + 1, updated_at = @now WHERE id = @id;

-- name: ConsumeCode :exec
UPDATE email_verifications SET consumed_at = @now::timestamptz, updated_at = @now::timestamptz
WHERE id = @id AND consumed_at IS NULL;

-- name: InvalidateCodes :exec
UPDATE email_verifications SET consumed_at = @now::timestamptz, updated_at = @now::timestamptz
WHERE user_id = @user_id AND purpose = @purpose AND consumed_at IS NULL;

-- name: InsertRefreshToken :exec
INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at, created_at, updated_at)
VALUES (@id, @user_id, @family_id, @token_hash, @expires_at, @now, @now);
