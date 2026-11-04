package identity

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

const testPassword = "abc12345"

func TestRegister_SendsCodeAndCreatesUnverifiedCustomer(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))

	var (
		role       string
		verifiedAt *time.Time
		profiles   int
	)
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT role, email_verified_at FROM users WHERE email = 'an@gmail.com'`).Scan(&role, &verifiedAt))
	require.Equal(t, "customer", role)
	require.Nil(t, verifiedAt)
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(*) FROM customer_profiles p JOIN users u ON u.id = p.user_id WHERE u.email = 'an@gmail.com'`).Scan(&profiles))
	require.Equal(t, 1, profiles)

	code := lastCode(t, env)
	require.Len(t, code, 6)
	time.Sleep(200 * time.Millisecond) // give any stray extra email a chance to arrive
	msgs := env.Mail.Messages()
	require.Len(t, msgs, 1)
	require.Equal(t, "an@gmail.com", msgs[0].To)
	require.Equal(t, "Mã xác minh LunaIn Travel", msgs[0].Subject)
	require.Contains(t, msgs[0].Text, "Mã có hiệu lực trong 10 phút.")

	// Only the HMAC of the code is stored.
	var codeHash []byte
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT code_hash FROM email_verifications`).Scan(&codeHash))
	require.NotContains(t, string(codeHash), code)
}

func TestRegister_NormalizesEmail(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "  An@Gmail.com ", testPassword))

	var email string
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT email FROM users`).Scan(&email))
	require.Equal(t, "an@gmail.com", email)
	lastCode(t, env)
	require.Equal(t, "an@gmail.com", env.Mail.Messages()[0].To)
}

func TestRegister_RejectsInvalidInput(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.ErrorIs(t, env.Svc.Register(ctx, "not-an-email", testPassword), ErrInvalidEmail)
	require.ErrorIs(t, env.Svc.Register(ctx, "an@gmail.com", "abcdefgh"), ErrWeakPassword)
}

func TestRegister_VerifiedEmailTaken(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", lastCode(t, env))
	require.NoError(t, err)

	env.Clock.Advance(time.Hour)
	require.ErrorIs(t, env.Svc.Register(ctx, "AN@gmail.com", testPassword), ErrEmailTaken)
}

func TestRegister_UnverifiedReRegisterRespectsCooldown(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	oldCode := lastCode(t, env)

	require.ErrorIs(t, env.Svc.Register(ctx, "an@gmail.com", "newpass123"), ErrResendTooSoon)

	env.Clock.Advance(61 * time.Second)
	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", "newpass123"))
	newCode := lastCode(t, env)

	if oldCode != newCode {
		_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", oldCode)
		require.ErrorIs(t, err, ErrInvalidCode)
	}
	// The old code row is invalidated regardless of the random values.
	var active int
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(*) FROM email_verifications WHERE consumed_at IS NULL`).Scan(&active))
	require.Equal(t, 1, active)

	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", newCode)
	require.NoError(t, err)

	// The newest password is the one stored.
	var hash string
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT password_hash FROM users`).Scan(&hash))
	ok, err := VerifyPassword(hash, "newpass123")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestResendVerification_UnknownEmailSilent(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.ResendVerification(ctx, "nobody@gmail.com"))
	require.NoError(t, env.Svc.ResendVerification(ctx, "not-an-email"))

	// Already verified: silent as well.
	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", lastCode(t, env))
	require.NoError(t, err)
	env.Clock.Advance(time.Hour)
	require.NoError(t, env.Svc.ResendVerification(ctx, "an@gmail.com"))

	time.Sleep(300 * time.Millisecond)
	require.Len(t, env.Mail.Messages(), 1)
	var jobs int
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'send_email'`).Scan(&jobs))
	require.Equal(t, 1, jobs)
}

func TestResendVerification_Cooldown(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	oldCode := lastCode(t, env)

	env.Clock.Advance(59 * time.Second)
	require.ErrorIs(t, env.Svc.ResendVerification(ctx, " AN@gmail.com"), ErrResendTooSoon)

	env.Clock.Advance(2 * time.Second)
	require.NoError(t, env.Svc.ResendVerification(ctx, " AN@gmail.com"))
	newCode := lastCode(t, env)
	require.Equal(t, "an@gmail.com", env.Mail.Messages()[1].To)

	// Cooldown restarts from the new code.
	require.ErrorIs(t, env.Svc.ResendVerification(ctx, "an@gmail.com"), ErrResendTooSoon)

	if oldCode != newCode {
		_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", oldCode)
		require.ErrorIs(t, err, ErrInvalidCode)
	}
	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", newCode)
	require.NoError(t, err)
}

func TestVerifyEmail_Success(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	code := lastCode(t, env)
	env.Clock.Advance(3 * time.Minute)

	tok, err := env.Svc.VerifyEmail(ctx, " An@gmail.com", code)
	require.NoError(t, err)
	require.NotEmpty(t, tok.AccessToken)
	require.NotEmpty(t, tok.RefreshToken)
	require.Equal(t, auth.RoleCustomer, tok.Role)
	require.True(t, tok.AccessExpiresAt.Equal(env.Clock.Now().Add(15*time.Minute)))
	require.True(t, tok.RefreshExpiresAt.Equal(env.Clock.Now().Add(30*24*time.Hour)))

	var verifiedAt time.Time
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT email_verified_at FROM users`).Scan(&verifiedAt))
	require.True(t, verifiedAt.Equal(env.Clock.Now()), "got %v want %v", verifiedAt, env.Clock.Now())

	// Refresh token is stored only as a hash.
	var n int
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_tokens WHERE token_hash = sha256(convert_to($1, 'UTF8'))`, tok.RefreshToken).Scan(&n))
	require.Equal(t, 1, n)
}

func TestVerifyEmail_Expired(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	code := lastCode(t, env)

	env.Clock.Advance(10*time.Minute + time.Second)
	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", code)
	require.ErrorIs(t, err, ErrInvalidCode)
}

func TestVerifyEmail_LockedAfterFiveWrong(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	code := lastCode(t, env)
	wrong := wrongCode(code)

	for i := range 5 {
		_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", wrong)
		require.ErrorIs(t, err, ErrInvalidCode, "attempt %d", i+1)
	}
	var attempts int
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT attempts FROM email_verifications`).Scan(&attempts))
	require.Equal(t, 5, attempts)

	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", code)
	require.ErrorIs(t, err, ErrInvalidCode)
}

func TestVerifyEmail_CodeSingleUse(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	code := lastCode(t, env)

	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", code)
	require.NoError(t, err)
	_, err = env.Svc.VerifyEmail(ctx, "an@gmail.com", code)
	require.ErrorIs(t, err, ErrInvalidCode)
}

func TestVerifyEmail_UnknownEmail(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.Svc.VerifyEmail(context.Background(), "nobody@gmail.com", "123456")
	require.ErrorIs(t, err, ErrInvalidCode)
}

func wrongCode(code string) string {
	var n int
	fmt.Sscanf(code, "%d", &n)
	return fmt.Sprintf("%06d", (n+1)%1000000)
}
