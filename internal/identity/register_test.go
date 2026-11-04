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
	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", lastCode(t, env), testPassword)
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
		_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", oldCode, "newpass123")
		require.ErrorIs(t, err, ErrInvalidCode)
	}
	// The old code row is invalidated regardless of the random values.
	var active int
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(*) FROM email_verifications WHERE consumed_at IS NULL`).Scan(&active))
	require.Equal(t, 1, active)

	// The new code row carries the new password; the user row has none yet.
	var pending string
	var userHash *string
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT password_hash FROM email_verifications WHERE consumed_at IS NULL`).Scan(&pending))
	ok, err := VerifyPassword(pending, "newpass123")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT password_hash FROM users`).Scan(&userHash))
	require.Nil(t, userHash)

	_, err = env.Svc.VerifyEmail(ctx, "an@gmail.com", newCode, "newpass123")
	require.NoError(t, err)

	// After verification the newest password is the account password.
	var hash string
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT password_hash FROM users`).Scan(&hash))
	ok, err = VerifyPassword(hash, "newpass123")
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
	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", lastCode(t, env), testPassword)
	require.NoError(t, err)
	env.Clock.Advance(time.Hour)
	require.NoError(t, env.Svc.ResendVerification(ctx, "an@gmail.com"))

	time.Sleep(300 * time.Millisecond)
	require.Len(t, env.Mail.Messages(), 1)
	var jobs int
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'send_email'`).Scan(&jobs))
	require.Equal(t, 1, jobs)
}

func TestResendVerification_DailyCap(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Register sends code 1; four resends send codes 2-5.
	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	for range 4 {
		env.Clock.Advance(61 * time.Second)
		require.NoError(t, env.Svc.ResendVerification(ctx, "an@gmail.com"))
	}
	// The 6th code request in 24h is refused, by resend and re-register alike.
	env.Clock.Advance(61 * time.Second)
	require.ErrorIs(t, env.Svc.ResendVerification(ctx, "an@gmail.com"), ErrResendTooSoon)
	require.ErrorIs(t, env.Svc.Register(ctx, "an@gmail.com", testPassword), ErrResendTooSoon)
	require.Equal(t, 5, mailsWithSubject(t, env, verificationSubject, 5))
	require.Equal(t, 5, count(t, env, `SELECT count(*) FROM email_verifications WHERE purpose = 'signup'`))
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
		_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", oldCode, testPassword)
		require.ErrorIs(t, err, ErrInvalidCode)
	}
	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", newCode, testPassword)
	require.NoError(t, err)
}

func TestVerifyEmail_Success(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	code := lastCode(t, env)
	env.Clock.Advance(3 * time.Minute)

	tok, err := env.Svc.VerifyEmail(ctx, " An@gmail.com", code, testPassword)
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
	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", code, testPassword)
	require.ErrorIs(t, err, ErrInvalidCode)
}

func TestVerifyEmail_LockedAfterFiveWrong(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	code := lastCode(t, env)
	wrong := wrongCode(code)

	for i := range 5 {
		_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", wrong, testPassword)
		require.ErrorIs(t, err, ErrInvalidCode, "attempt %d", i+1)
	}
	var attempts int
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT attempts FROM email_verifications`).Scan(&attempts))
	require.Equal(t, 5, attempts)

	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", code, testPassword)
	require.ErrorIs(t, err, ErrInvalidCode)
}

func TestVerifyEmail_CodeSingleUse(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	code := lastCode(t, env)

	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", code, testPassword)
	require.NoError(t, err)
	_, err = env.Svc.VerifyEmail(ctx, "an@gmail.com", code, testPassword)
	require.ErrorIs(t, err, ErrInvalidCode)
}

func TestVerifyEmail_UnknownEmail(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.Svc.VerifyEmail(context.Background(), "nobody@gmail.com", "123456", testPassword)
	require.ErrorIs(t, err, ErrInvalidCode)
}

func TestVerifyEmail_RequiresSignupPassword(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	code := lastCode(t, env)

	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", code, "other1234")
	require.ErrorIs(t, err, ErrInvalidCode)
	require.Equal(t, 1, codeAttempts(t, env))
	requireUserUnverified(t, env)

	_, err = env.Svc.VerifyEmail(ctx, "an@gmail.com", code, testPassword)
	require.NoError(t, err)
}

func TestRegister_ReRegisterCannotHijack(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	const victimPw, attackerPw = "victim123", "attacker123"

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", victimPw))
	lastCode(t, env)

	env.Clock.Advance(61 * time.Second)
	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", attackerPw))
	attackerCode := lastCode(t, env) // mailed to the victim, not the attacker

	// The re-registration does not touch the account password.
	var userHash *string
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT password_hash FROM users`).Scan(&userHash))
	require.Nil(t, userHash)

	// Victim uses the newest code with their own password: rejected.
	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", attackerCode, victimPw)
	require.ErrorIs(t, err, ErrInvalidCode)
	// Attacker without the code cannot verify.
	_, err = env.Svc.VerifyEmail(ctx, "an@gmail.com", wrongCode(attackerCode), attackerPw)
	require.ErrorIs(t, err, ErrInvalidCode)
	requireUserUnverified(t, env)

	// Victim re-registers after the cooldown and verifies with the newest code.
	env.Clock.Advance(61 * time.Second)
	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", victimPw))
	victimCode := lastCode(t, env)
	_, err = env.Svc.VerifyEmail(ctx, "an@gmail.com", victimCode, victimPw)
	require.NoError(t, err)

	var hash string
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT password_hash FROM users`).Scan(&hash))
	ok, err := VerifyPassword(hash, victimPw)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = VerifyPassword(hash, attackerPw)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestResendVerification_KeepsPendingPassword(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	lastCode(t, env)

	env.Clock.Advance(61 * time.Second)
	require.NoError(t, env.Svc.ResendVerification(ctx, "an@gmail.com"))
	code := lastCode(t, env)

	_, err := env.Svc.VerifyEmail(ctx, "an@gmail.com", code, testPassword)
	require.NoError(t, err)

	var hash string
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT password_hash FROM users`).Scan(&hash))
	ok, err := VerifyPassword(hash, testPassword)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestResendVerification_IgnoresNonCustomerAndLocked(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	lastCode(t, env)
	env.Clock.Advance(61 * time.Second)

	_, err := env.Pool.Exec(ctx, `UPDATE users SET status = 'locked'`)
	require.NoError(t, err)
	require.NoError(t, env.Svc.ResendVerification(ctx, "an@gmail.com"))

	_, err = env.Pool.Exec(ctx, `UPDATE users SET status = 'active', role = 'partner'`)
	require.NoError(t, err)
	require.NoError(t, env.Svc.ResendVerification(ctx, "an@gmail.com"))

	var codes int
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT count(*) FROM email_verifications`).Scan(&codes))
	require.Equal(t, 1, codes)
}

func codeAttempts(t *testing.T, env *testEnv) int {
	t.Helper()
	var n int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT attempts FROM email_verifications WHERE consumed_at IS NULL`).Scan(&n))
	return n
}

func requireUserUnverified(t *testing.T, env *testEnv) {
	t.Helper()
	var verifiedAt *time.Time
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT email_verified_at FROM users`).Scan(&verifiedAt))
	require.Nil(t, verifiedAt)
}

func wrongCode(code string) string {
	var n int
	fmt.Sscanf(code, "%d", &n)
	return fmt.Sprintf("%06d", (n+1)%1000000)
}
