package identity

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

// registerVerified signs up email with testPassword and verifies it.
func registerVerified(t *testing.T, env *testEnv, email string) Tokens {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, env.Svc.Register(ctx, email, testPassword))
	tok, err := env.Svc.VerifyEmail(ctx, email, lastCode(t, env), testPassword)
	require.NoError(t, err)
	return tok
}

func TestLogin_Success(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	first := registerVerified(t, env, "an@gmail.com")

	tok, err := env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.NoError(t, err)
	require.NotEmpty(t, tok.AccessToken)
	require.NotEmpty(t, tok.RefreshToken)
	require.Equal(t, auth.RoleCustomer, tok.Role)
	require.Equal(t, env.Clock.Now().Add(30*24*time.Hour), tok.RefreshExpiresAt)

	// Each login starts a new family.
	var families int
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(DISTINCT family_id) FROM refresh_tokens WHERE token_hash = ANY($1)`,
		[][]byte{hashRefreshToken(first.RefreshToken), hashRefreshToken(tok.RefreshToken)}).Scan(&families))
	require.Equal(t, 2, families)
}

func TestLogin_EmailCaseInsensitive(t *testing.T) {
	env := newTestEnv(t)
	registerVerified(t, env, "An@Gmail.com")

	_, err := env.Svc.Login(context.Background(), "an@gmail.com", testPassword)
	require.NoError(t, err)
	_, err = env.Svc.Login(context.Background(), "  AN@gmail.com ", testPassword)
	require.NoError(t, err)
}

func TestLogin_WrongPassword(t *testing.T) {
	env := newTestEnv(t)
	registerVerified(t, env, "an@gmail.com")

	_, err := env.Svc.Login(context.Background(), "an@gmail.com", "wrong1234")
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestLogin_UnknownEmail(t *testing.T) {
	env := newTestEnv(t)

	_, err := env.Svc.Login(context.Background(), "nobody@gmail.com", testPassword)
	require.ErrorIs(t, err, ErrInvalidCredentials)
	_, err = env.Svc.Login(context.Background(), "not-an-email", testPassword)
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestLogin_PasswordlessAccount(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@gmail.com")
	// A Google-only account has no password.
	_, err := env.Pool.Exec(ctx, `UPDATE users SET password_hash = NULL`)
	require.NoError(t, err)

	_, err = env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestLogin_Unverified(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	lastCode(t, env)

	_, err := env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.ErrorIs(t, err, ErrEmailNotVerified)
	// A wrong password does not reveal that the account is pending.
	_, err = env.Svc.Login(ctx, "an@gmail.com", "wrong1234")
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestLogin_Locked(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@gmail.com")
	_, err := env.Pool.Exec(ctx, `UPDATE users SET status = 'locked'`)
	require.NoError(t, err)

	_, err = env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.ErrorIs(t, err, ErrAccountLocked)
	_, err = env.Svc.Login(ctx, "an@gmail.com", "wrong1234")
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestRefresh_Rotates(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	r1 := registerVerified(t, env, "an@gmail.com")

	env.Clock.Advance(time.Minute)
	r2, err := env.Svc.Refresh(ctx, r1.RefreshToken)
	require.NoError(t, err)
	require.NotEqual(t, r1.RefreshToken, r2.RefreshToken)
	require.NotEmpty(t, r2.AccessToken)
	require.Equal(t, auth.RoleCustomer, r2.Role)

	r3, err := env.Svc.Refresh(ctx, r2.RefreshToken)
	require.NoError(t, err)
	require.NotEqual(t, r2.RefreshToken, r3.RefreshToken)

	// All rotations stay in the same family.
	var families, rows int
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(DISTINCT family_id), count(*) FROM refresh_tokens`).Scan(&families, &rows))
	require.Equal(t, 1, families)
	require.Equal(t, 3, rows)
}

func TestRefresh_ReuseRevokesFamily(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	r1 := registerVerified(t, env, "an@gmail.com")

	r2, err := env.Svc.Refresh(ctx, r1.RefreshToken)
	require.NoError(t, err)

	_, err = env.Svc.Refresh(ctx, r1.RefreshToken)
	require.ErrorIs(t, err, ErrInvalidRefreshToken)
	_, err = env.Svc.Refresh(ctx, r2.RefreshToken)
	require.ErrorIs(t, err, ErrInvalidRefreshToken)

	var active int
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NULL`).Scan(&active))
	require.Equal(t, 0, active)
}

func TestRefresh_ReuseKeepsOtherFamilies(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	r1 := registerVerified(t, env, "an@gmail.com")
	other, err := env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.NoError(t, err)

	_, err = env.Svc.Refresh(ctx, r1.RefreshToken)
	require.NoError(t, err)
	_, err = env.Svc.Refresh(ctx, r1.RefreshToken)
	require.ErrorIs(t, err, ErrInvalidRefreshToken)

	_, err = env.Svc.Refresh(ctx, other.RefreshToken)
	require.NoError(t, err)
}

func TestRefresh_ConcurrentReuse(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	r1 := registerVerified(t, env, "an@gmail.com")

	var (
		wg      sync.WaitGroup
		results [2]Tokens
		errs    [2]error
	)
	start := make(chan struct{})
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = env.Svc.Refresh(ctx, r1.RefreshToken)
		}()
	}
	close(start)
	wg.Wait()

	var issued string
	successes := 0
	for i := range 2 {
		if errs[i] == nil {
			successes++
			issued = results[i].RefreshToken
		} else {
			require.ErrorIs(t, errs[i], ErrInvalidRefreshToken)
		}
	}
	require.Equal(t, 1, successes)

	_, err := env.Svc.Refresh(ctx, issued)
	require.ErrorIs(t, err, ErrInvalidRefreshToken)
}

func TestRefresh_Expired(t *testing.T) {
	env := newTestEnv(t)
	r1 := registerVerified(t, env, "an@gmail.com")

	env.Clock.Advance(30*24*time.Hour + time.Second)
	_, err := env.Svc.Refresh(context.Background(), r1.RefreshToken)
	require.ErrorIs(t, err, ErrInvalidRefreshToken)
}

func TestRefresh_Garbage(t *testing.T) {
	env := newTestEnv(t)

	_, err := env.Svc.Refresh(context.Background(), "garbage")
	require.ErrorIs(t, err, ErrInvalidRefreshToken)
	_, err = env.Svc.Refresh(context.Background(), "")
	require.ErrorIs(t, err, ErrInvalidRefreshToken)
}

func TestRefresh_LockedUserRevokesFamily(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	r1 := registerVerified(t, env, "an@gmail.com")
	_, err := env.Pool.Exec(ctx, `UPDATE users SET status = 'locked'`)
	require.NoError(t, err)

	_, err = env.Svc.Refresh(ctx, r1.RefreshToken)
	require.ErrorIs(t, err, ErrAccountLocked)

	// The revocation was committed: unlocking does not revive the session.
	_, err = env.Pool.Exec(ctx, `UPDATE users SET status = 'active'`)
	require.NoError(t, err)
	_, err = env.Svc.Refresh(ctx, r1.RefreshToken)
	require.ErrorIs(t, err, ErrInvalidRefreshToken)
}

func TestRefresh_UsesCurrentRole(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	r1 := registerVerified(t, env, "an@gmail.com")
	_, err := env.Pool.Exec(ctx, `UPDATE users SET role = 'partner'`)
	require.NoError(t, err)

	r2, err := env.Svc.Refresh(ctx, r1.RefreshToken)
	require.NoError(t, err)
	require.Equal(t, auth.RolePartner, r2.Role)
}

func TestLogout_RevokesFamily(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	r1 := registerVerified(t, env, "an@gmail.com")
	r2, err := env.Svc.Refresh(ctx, r1.RefreshToken)
	require.NoError(t, err)
	other, err := env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.NoError(t, err)

	require.NoError(t, env.Svc.Logout(ctx, r2.RefreshToken))
	_, err = env.Svc.Refresh(ctx, r2.RefreshToken)
	require.ErrorIs(t, err, ErrInvalidRefreshToken)

	// Idempotent, and garbage tokens are accepted silently.
	require.NoError(t, env.Svc.Logout(ctx, r2.RefreshToken))
	require.NoError(t, env.Svc.Logout(ctx, "garbage"))

	// Other sessions are untouched.
	_, err = env.Svc.Refresh(ctx, other.RefreshToken)
	require.NoError(t, err)
}

func TestRevokeAllSessions(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	r1 := registerVerified(t, env, "an@gmail.com")
	r2, err := env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.NoError(t, err)

	var userID uuid.UUID
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT id FROM users`).Scan(&userID))
	require.NoError(t, db.InTx(ctx, env.Pool, func(tx pgx.Tx) error {
		return env.Svc.RevokeAllSessions(ctx, tx, userID)
	}))

	_, err = env.Svc.Refresh(ctx, r1.RefreshToken)
	require.ErrorIs(t, err, ErrInvalidRefreshToken)
	_, err = env.Svc.Refresh(ctx, r2.RefreshToken)
	require.ErrorIs(t, err, ErrInvalidRefreshToken)
}

func TestRevokeAllSessions_LocksUserRow(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@gmail.com")
	_, err := env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.NoError(t, err)
	var userID uuid.UUID
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT id FROM users`).Scan(&userID))

	txA, err := env.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = txA.Rollback(ctx) }()
	require.NoError(t, env.Svc.RevokeAllSessions(ctx, txA, userID))

	// While A is open, nobody else can lock the user row.
	txB, err := env.Pool.Begin(ctx)
	require.NoError(t, err)
	_, err = txB.Exec(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE NOWAIT`, userID)
	require.NoError(t, txB.Rollback(ctx)) // before asserting, so a failure cannot leak B's lock
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "user row was not locked by RevokeAllSessions")
	require.Equal(t, "55P03", pgErr.Code) // lock_not_available

	require.NoError(t, txA.Commit(ctx))
	var active int
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&active))
	require.Equal(t, 0, active)
}

func TestCreateInternalUser(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	var id uuid.UUID
	require.NoError(t, db.InTx(ctx, env.Pool, func(tx pgx.Tx) error {
		var err error
		id, err = env.Svc.CreateInternalUser(ctx, tx, " Partner@LunaIn.vn ", testPassword, auth.RolePartner)
		return err
	}))

	var (
		email      string
		verifiedAt *time.Time
		profiles   int
	)
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT email, email_verified_at FROM users WHERE id = $1`, id).Scan(&email, &verifiedAt))
	require.Equal(t, "partner@lunain.vn", email)
	require.NotNil(t, verifiedAt)
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT count(*) FROM customer_profiles`).Scan(&profiles))
	require.Equal(t, 0, profiles)

	tok, err := env.Svc.Login(ctx, "partner@lunain.vn", testPassword)
	require.NoError(t, err)
	require.Equal(t, auth.RolePartner, tok.Role)

	// Duplicate email.
	err = db.InTx(ctx, env.Pool, func(tx pgx.Tx) error {
		_, err := env.Svc.CreateInternalUser(ctx, tx, "PARTNER@lunain.vn", testPassword, auth.RoleSuperAdmin)
		return err
	})
	require.ErrorIs(t, err, ErrEmailTaken)

	// Customers are never created this way.
	err = db.InTx(ctx, env.Pool, func(tx pgx.Tx) error {
		_, err := env.Svc.CreateInternalUser(ctx, tx, "c@lunain.vn", testPassword, auth.RoleCustomer)
		return err
	})
	require.ErrorIs(t, err, errNotInternalRole)

	// Input is validated.
	err = db.InTx(ctx, env.Pool, func(tx pgx.Tx) error {
		_, err := env.Svc.CreateInternalUser(ctx, tx, "bad", testPassword, auth.RoleSuperAdmin)
		return err
	})
	require.ErrorIs(t, err, ErrInvalidEmail)
	err = db.InTx(ctx, env.Pool, func(tx pgx.Tx) error {
		_, err := env.Svc.CreateInternalUser(ctx, tx, "admin@lunain.vn", "short", auth.RoleSuperAdmin)
		return err
	})
	require.ErrorIs(t, err, ErrWeakPassword)
}
