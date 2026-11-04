-- name: InsertUser :exec
INSERT INTO users (id, email, password_hash, role, created_at, updated_at)
VALUES (@id, @email, @password_hash, @role, @now, @now);

-- name: InsertCustomerProfile :exec
INSERT INTO customer_profiles (user_id, created_at, updated_at)
VALUES (@user_id, @now, @now);

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = @email;

-- name: GetUserByEmailForUpdate :one
SELECT * FROM users WHERE email = @email FOR NO KEY UPDATE;

-- name: MarkEmailVerified :exec
UPDATE users
SET email_verified_at = @now::timestamptz, password_hash = @password_hash, updated_at = @now::timestamptz
WHERE id = @id AND email_verified_at IS NULL;

-- name: InsertVerification :exec
INSERT INTO email_verifications (id, user_id, purpose, code_hash, password_hash, expires_at, created_at, updated_at)
VALUES (@id, @user_id, @purpose, @code_hash, @password_hash, @expires_at, @now, @now);

-- name: GetLatestCode :one
SELECT * FROM email_verifications
WHERE user_id = @user_id AND purpose = @purpose
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetLatestActiveCodeForUpdate :one
SELECT * FROM email_verifications
WHERE user_id = @user_id AND purpose = @purpose AND consumed_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT 1
FOR UPDATE;

-- name: CountCodesSince :one
SELECT count(*) FROM email_verifications
WHERE user_id = @user_id AND purpose = @purpose AND created_at > @since;

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

-- name: GetUserByIDForUpdate :one
SELECT * FROM users WHERE id = @id FOR NO KEY UPDATE;

-- name: InsertVerifiedUser :execrows
INSERT INTO users (id, email, password_hash, email_verified_at, role, created_at, updated_at)
VALUES (@id, @email, @password_hash, @now::timestamptz, @role, @now::timestamptz, @now::timestamptz)
ON CONFLICT (email) DO NOTHING;

-- name: GetRefreshTokenByHash :one
SELECT * FROM refresh_tokens WHERE token_hash = @token_hash;

-- name: GetRefreshTokenForUpdate :one
SELECT * FROM refresh_tokens WHERE id = @id FOR UPDATE;

-- name: MarkRefreshTokenUsed :exec
UPDATE refresh_tokens SET used_at = @now::timestamptz, updated_at = @now::timestamptz
WHERE id = @id AND used_at IS NULL;

-- name: RevokeRefreshTokenFamily :exec
UPDATE refresh_tokens SET revoked_at = @now::timestamptz, updated_at = @now::timestamptz
WHERE family_id = @family_id AND revoked_at IS NULL;

-- name: RevokeUserRefreshTokens :exec
UPDATE refresh_tokens SET revoked_at = @now::timestamptz, updated_at = @now::timestamptz
WHERE user_id = @user_id AND revoked_at IS NULL;

-- name: SetUserPassword :exec
UPDATE users
SET password_hash = @password_hash,
    email_verified_at = COALESCE(email_verified_at, @now::timestamptz),
    updated_at = @now::timestamptz
WHERE id = @id;

-- name: GetIdentity :one
SELECT * FROM user_identities WHERE provider = @provider AND provider_subject = @provider_subject;

-- name: InsertIdentity :execrows
INSERT INTO user_identities (id, user_id, provider, provider_subject, created_at, updated_at)
VALUES (@id, @user_id, @provider, @provider_subject, @now, @now)
ON CONFLICT (provider, provider_subject) DO NOTHING;

-- name: SetEmailVerifiedKeepPassword :exec
UPDATE users
SET email_verified_at = COALESCE(email_verified_at, @now::timestamptz), updated_at = @now::timestamptz
WHERE id = @id;

-- name: GetCustomerProfile :one
SELECT p.user_id, u.email, p.full_name, p.phone, p.date_of_birth
FROM customer_profiles p JOIN users u ON u.id = p.user_id
WHERE p.user_id = @user_id;

-- name: GetCustomerProfileForUpdate :one
SELECT * FROM customer_profiles WHERE user_id = @user_id FOR UPDATE;

-- name: UpdateCustomerProfile :exec
UPDATE customer_profiles
SET full_name = @full_name, phone = @phone, date_of_birth = @date_of_birth, updated_at = @now
WHERE user_id = @user_id;

-- name: FillCustomerProfileIfEmpty :exec
UPDATE customer_profiles
SET full_name = COALESCE(full_name, sqlc.narg('full_name')),
    phone = COALESCE(phone, sqlc.narg('phone')),
    updated_at = @now
WHERE user_id = @user_id;

-- name: DeleteUserIdentities :exec
DELETE FROM user_identities WHERE user_id = @user_id;
