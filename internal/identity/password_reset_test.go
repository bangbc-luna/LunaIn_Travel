package identity

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const newPassword = "brandnew1234"

func TestRequestPasswordReset_UnknownEmailSilent(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "nobody@gmail.com"))
	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "not-an-email"))
	// Give River a moment: nothing must be sent.
	time.Sleep(300 * time.Millisecond)
	require.Empty(t, env.Mail.Messages())
}

func TestRequestPasswordReset_SendsCodeWithSubject(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@gmail.com")

	require.NoError(t, env.Svc.RequestPasswordReset(ctx, " An@gmail.com"))
	lastCode(t, env)
	msgs := env.Mail.Messages()
	require.Equal(t, "Đặt lại mật khẩu LunaIn Travel", msgs[len(msgs)-1].Subject)
	require.Equal(t, "an@gmail.com", msgs[len(msgs)-1].To)
}

func TestRequestPasswordReset_CooldownSilent(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@gmail.com")

	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "an@gmail.com"))
	lastCode(t, env)
	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "an@gmail.com"))
	time.Sleep(300 * time.Millisecond)
	require.Len(t, env.Mail.Messages(), 2) // signup + one reset

	env.Clock.Advance(61 * time.Second)
	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "an@gmail.com"))
	lastCode(t, env)
}

func TestRequestPasswordReset_LockedSilent(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@gmail.com")
	_, err := env.Pool.Exec(ctx, `UPDATE users SET status = 'locked'`)
	require.NoError(t, err)

	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "an@gmail.com"))
	time.Sleep(300 * time.Millisecond)
	require.Len(t, env.Mail.Messages(), 1) // signup only
}

func TestResetPassword_Success(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	tok := registerVerified(t, env, "an@gmail.com")

	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "an@gmail.com"))
	code := lastCode(t, env)
	require.NoError(t, env.Svc.ResetPassword(ctx, " An@gmail.com", code, newPassword))

	_, err := env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.ErrorIs(t, err, ErrInvalidCredentials)
	_, err = env.Svc.Login(ctx, "an@gmail.com", newPassword)
	require.NoError(t, err)
	_, err = env.Svc.Refresh(ctx, tok.RefreshToken)
	require.ErrorIs(t, err, ErrInvalidRefreshToken)

	// Single use.
	require.ErrorIs(t, env.Svc.ResetPassword(ctx, "an@gmail.com", code, "another1234"), ErrInvalidCode)
}

func TestResetPassword_WeakPassword(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@gmail.com")
	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "an@gmail.com"))
	code := lastCode(t, env)

	require.ErrorIs(t, env.Svc.ResetPassword(ctx, "an@gmail.com", code, "short"), ErrWeakPassword)
	require.NoError(t, env.Svc.ResetPassword(ctx, "an@gmail.com", code, newPassword))
}

func TestResetPassword_WrongCodeFiveTimes(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@gmail.com")
	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "an@gmail.com"))
	code := lastCode(t, env)
	wrong := wrongCode(code)

	for i := range 5 {
		require.ErrorIs(t, env.Svc.ResetPassword(ctx, "an@gmail.com", wrong, newPassword), ErrInvalidCode, "attempt %d", i+1)
	}
	var attempts int
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT attempts FROM email_verifications WHERE purpose = 'password_reset'`).Scan(&attempts))
	require.Equal(t, 5, attempts)

	require.ErrorIs(t, env.Svc.ResetPassword(ctx, "an@gmail.com", code, newPassword), ErrInvalidCode)
	_, err := env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.NoError(t, err)
}

func TestResetPassword_Expired(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@gmail.com")
	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "an@gmail.com"))
	code := lastCode(t, env)

	env.Clock.Advance(10*time.Minute + time.Second)
	require.ErrorIs(t, env.Svc.ResetPassword(ctx, "an@gmail.com", code, newPassword), ErrInvalidCode)
}

func TestResetPassword_UnknownEmail(t *testing.T) {
	env := newTestEnv(t)
	require.ErrorIs(t, env.Svc.ResetPassword(context.Background(), "nobody@gmail.com", "123456", newPassword), ErrInvalidCode)
}

func TestResetPassword_GoogleOnlyUserCanSetPassword(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	_, err := env.Pool.Exec(ctx, `
		INSERT INTO users (id, email, password_hash, email_verified_at, role)
		VALUES (gen_random_uuid(), 'gg@gmail.com', NULL, now(), 'customer')`)
	require.NoError(t, err)
	_, err = env.Svc.Login(ctx, "gg@gmail.com", newPassword)
	require.ErrorIs(t, err, ErrInvalidCredentials)

	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "gg@gmail.com"))
	code := lastCode(t, env)
	require.NoError(t, env.Svc.ResetPassword(ctx, "gg@gmail.com", code, newPassword))
	_, err = env.Svc.Login(ctx, "gg@gmail.com", newPassword)
	require.NoError(t, err)
}

func TestResetPassword_SignupCodeNotAccepted(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	signupCode := lastCode(t, env)

	require.ErrorIs(t, env.Svc.ResetPassword(ctx, "an@gmail.com", signupCode, newPassword), ErrInvalidCode)
	requireUserUnverified(t, env)
}

func TestResetPassword_UnverifiedUserBecomesVerified(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	signupCode := lastCode(t, env)

	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "an@gmail.com"))
	resetCode := lastCode(t, env)
	require.NoError(t, env.Svc.ResetPassword(ctx, "an@gmail.com", resetCode, newPassword))

	var verified bool
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT email_verified_at IS NOT NULL FROM users`).Scan(&verified))
	require.True(t, verified)
	// The pending signup code (and its old password) is dead.
	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", signupCode, testPassword)
	require.ErrorIs(t, err, ErrInvalidCode)
	_, err = env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.ErrorIs(t, err, ErrInvalidCredentials)
	_, err = env.Svc.Login(ctx, "an@gmail.com", newPassword)
	require.NoError(t, err)
}

func TestResetPassword_LockedUserRejected(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@gmail.com")
	require.NoError(t, env.Svc.RequestPasswordReset(ctx, "an@gmail.com"))
	code := lastCode(t, env)
	_, err := env.Pool.Exec(ctx, `UPDATE users SET status = 'locked'`)
	require.NoError(t, err)

	require.ErrorIs(t, env.Svc.ResetPassword(ctx, "an@gmail.com", code, newPassword), ErrInvalidCode)
}
