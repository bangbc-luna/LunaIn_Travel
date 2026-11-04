package identity

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

func googleTok(env *testEnv, token, sub, email string) {
	env.Google.Tokens[token] = GoogleIdentity{Subject: sub, Email: email, EmailVerified: true}
}

func userIDByEmail(t *testing.T, env *testEnv, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, env.Pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email = $1`, email).Scan(&id))
	return id
}

func count(t *testing.T, env *testEnv, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, env.Pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func TestGoogle_NewUser(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	googleTok(env, "t1", "sub-1", "An@Gmail.com")

	tok, err := env.Svc.LoginWithGoogle(ctx, "t1")
	require.NoError(t, err)
	require.NotEmpty(t, tok.AccessToken)
	require.Equal(t, auth.RoleCustomer, tok.Role)

	id := userIDByEmail(t, env, "an@gmail.com")
	var role string
	var verified bool
	var hashNull bool
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT role, email_verified_at IS NOT NULL, password_hash IS NULL FROM users WHERE id = $1`, id).Scan(&role, &verified, &hashNull))
	require.Equal(t, "customer", role)
	require.True(t, verified)
	require.True(t, hashNull)
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM user_identities WHERE user_id = $1 AND provider = 'google' AND provider_subject = 'sub-1'`, id))
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM customer_profiles WHERE user_id = $1`, id))
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM refresh_tokens WHERE user_id = $1`, id))
}

func TestGoogle_ReturningUser(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	googleTok(env, "t1", "sub-1", "an@gmail.com")

	_, err := env.Svc.LoginWithGoogle(ctx, "t1")
	require.NoError(t, err)
	id := userIDByEmail(t, env, "an@gmail.com")
	_, err = env.Svc.LoginWithGoogle(ctx, "t1")
	require.NoError(t, err)

	require.Equal(t, id, userIDByEmail(t, env, "an@gmail.com"))
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM users`))
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM user_identities`))
	require.Equal(t, 2, count(t, env, `SELECT count(*) FROM refresh_tokens WHERE user_id = $1`, id))
}

func TestGoogle_IdentityWinsWhenEmailChanged(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	googleTok(env, "t1", "sub-1", "an@gmail.com")
	_, err := env.Svc.LoginWithGoogle(ctx, "t1")
	require.NoError(t, err)
	id := userIDByEmail(t, env, "an@gmail.com")

	googleTok(env, "t2", "sub-1", "new@gmail.com")
	_, err = env.Svc.LoginWithGoogle(ctx, "t2")
	require.NoError(t, err)
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM users`))
	require.Equal(t, 2, count(t, env, `SELECT count(*) FROM refresh_tokens WHERE user_id = $1`, id))
}

func TestGoogle_LinksExistingUnverifiedUser(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	require.NoError(t, env.Svc.Register(ctx, "an@gmail.com", testPassword))
	code := lastCode(t, env)
	id := userIDByEmail(t, env, "an@gmail.com")
	googleTok(env, "t1", "sub-1", "An@gmail.com")

	_, err := env.Svc.LoginWithGoogle(ctx, "t1")
	require.NoError(t, err)

	require.Equal(t, id, userIDByEmail(t, env, "an@gmail.com"))
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM users`))
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM users WHERE id = $1 AND email_verified_at IS NOT NULL AND password_hash IS NULL`, id))
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM user_identities WHERE user_id = $1`, id))

	// The pending signup code (and its password) is dead.
	_, err = env.Svc.VerifyEmail(ctx, "an@gmail.com", code, testPassword)
	require.ErrorIs(t, err, ErrInvalidCode)
	_, err = env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestGoogle_LinksExistingVerifiedPasswordUser(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@gmail.com")
	id := userIDByEmail(t, env, "an@gmail.com")
	googleTok(env, "t1", "sub-1", "an@gmail.com")

	_, err := env.Svc.LoginWithGoogle(ctx, "t1")
	require.NoError(t, err)

	require.Equal(t, id, userIDByEmail(t, env, "an@gmail.com"))
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM user_identities WHERE user_id = $1`, id))
	_, err = env.Svc.Login(ctx, "an@gmail.com", testPassword)
	require.NoError(t, err)
}

func TestGoogle_UnverifiedGoogleEmailRejected(t *testing.T) {
	env := newTestEnv(t)
	env.Google.Tokens["t1"] = GoogleIdentity{Subject: "sub-1", Email: "an@gmail.com", EmailVerified: false}

	_, err := env.Svc.LoginWithGoogle(context.Background(), "t1")
	require.ErrorIs(t, err, ErrGoogleTokenInvalid)
	require.Equal(t, 0, count(t, env, `SELECT count(*) FROM users`))
}

func TestGoogle_InvalidTokenRejected(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.Svc.LoginWithGoogle(context.Background(), "unknown")
	require.ErrorIs(t, err, ErrGoogleTokenInvalid)
}

func TestGoogle_DoesNotLinkPartner(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	for _, role := range []auth.Role{auth.RolePartner, auth.RoleSuperAdmin} {
		email := string(role) + "@lunain.vn"
		require.NoError(t, db.InTx(ctx, env.Pool, func(tx pgx.Tx) error {
			_, err := env.Svc.CreateInternalUser(ctx, tx, email, testPassword, role)
			return err
		}))
		googleTok(env, string(role), "sub-"+string(role), email)

		_, err := env.Svc.LoginWithGoogle(ctx, string(role))
		require.ErrorIs(t, err, ErrGoogleTokenInvalid)
		require.Equal(t, 0, count(t, env, `SELECT count(*) FROM user_identities`))
	}
}

func TestGoogle_LockedUser(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	googleTok(env, "t1", "sub-1", "an@gmail.com")
	_, err := env.Svc.LoginWithGoogle(ctx, "t1")
	require.NoError(t, err)
	_, err = env.Pool.Exec(ctx, `UPDATE users SET status = 'locked' WHERE email = 'an@gmail.com'`)
	require.NoError(t, err)

	_, err = env.Svc.LoginWithGoogle(ctx, "t1")
	require.ErrorIs(t, err, ErrAccountLocked)

	// A locked user reached by email linking is refused and not linked.
	registerVerified(t, env, "bo@gmail.com")
	_, err = env.Pool.Exec(ctx, `UPDATE users SET status = 'locked' WHERE email = 'bo@gmail.com'`)
	require.NoError(t, err)
	googleTok(env, "t2", "sub-2", "bo@gmail.com")
	_, err = env.Svc.LoginWithGoogle(ctx, "t2")
	require.ErrorIs(t, err, ErrAccountLocked)
	require.Equal(t, 0, count(t, env, `SELECT count(*) FROM user_identities WHERE provider_subject = 'sub-2'`))
}

func TestGoogle_ConcurrentFirstLogin(t *testing.T) {
	env := newTestEnv(t)
	googleTok(env, "t1", "sub-1", "an@gmail.com")

	const n = 2
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = env.Svc.LoginWithGoogle(context.Background(), "t1")
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM users`))
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM user_identities`))
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM customer_profiles`))
}

func TestGoogle_DoesNotLinkNonGmailWithoutHD(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@company.vn")
	id := userIDByEmail(t, env, "an@company.vn")
	googleTok(env, "t1", "sub-1", "an@company.vn")

	_, err := env.Svc.LoginWithGoogle(ctx, "t1")
	require.ErrorIs(t, err, ErrGoogleLinkRequiresPassword)
	require.Equal(t, 0, count(t, env, `SELECT count(*) FROM user_identities`))
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM users WHERE id = $1 AND password_hash IS NOT NULL`, id))
	_, err = env.Svc.Login(ctx, "an@company.vn", testPassword)
	require.NoError(t, err)
}

func TestGoogle_DoesNotLinkUnverifiedNonGmailAndKeepsCodes(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	require.NoError(t, env.Svc.Register(ctx, "an@company.vn", testPassword))
	code := lastCode(t, env)
	googleTok(env, "t1", "sub-1", "an@company.vn")

	_, err := env.Svc.LoginWithGoogle(ctx, "t1")
	require.ErrorIs(t, err, ErrGoogleLinkRequiresPassword)
	require.Equal(t, 0, count(t, env, `SELECT count(*) FROM user_identities`))
	_, err = env.Svc.VerifyEmail(ctx, "an@company.vn", code, testPassword)
	require.NoError(t, err)
}

func TestGoogle_LinksWorkspaceWithHD(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	registerVerified(t, env, "an@company.vn")
	id := userIDByEmail(t, env, "an@company.vn")
	env.Google.Tokens["t1"] = GoogleIdentity{Subject: "sub-1", Email: "an@company.vn", EmailVerified: true, HostedDomain: "company.vn"}

	_, err := env.Svc.LoginWithGoogle(ctx, "t1")
	require.NoError(t, err)
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM user_identities WHERE user_id = $1`, id))
}

func TestGoogle_NewNonGmailUserStillCreated(t *testing.T) {
	env := newTestEnv(t)
	googleTok(env, "t1", "sub-1", "bo@company.vn")

	_, err := env.Svc.LoginWithGoogle(context.Background(), "t1")
	require.NoError(t, err)
	id := userIDByEmail(t, env, "bo@company.vn")
	require.Equal(t, 1, count(t, env, `SELECT count(*) FROM user_identities WHERE user_id = $1`, id))
}

// A Google login that read the identity before a password reset detached it
// must not issue a session once it gets the user row lock.
func TestGoogle_LoginRacingResetDoesNotIssueSession(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	googleTok(env, "t1", "sub-1", "bo@company.vn")
	_, err := env.Svc.LoginWithGoogle(ctx, "t1")
	require.NoError(t, err)
	id := userIDByEmail(t, env, "bo@company.vn")
	before := count(t, env, `SELECT count(*) FROM refresh_tokens WHERE user_id = $1`, id)

	// R stands in for ResetPassword: it holds the user row lock.
	r, err := env.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = r.Rollback(context.Background()) }()
	_, err = r.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, id)
	require.NoError(t, err)
	var rPID int
	require.NoError(t, r.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&rPID))

	done := make(chan error, 1)
	go func() {
		_, err := env.Svc.LoginWithGoogle(ctx, "t1")
		done <- err
	}()

	// Wait (bounded) until the login is blocked behind R's lock, i.e. it has
	// already read the identity.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		require.NoError(t, env.Pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, rPID).Scan(&waiting))
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("login finished before blocking on the user lock: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for login to block on the user row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err = r.Exec(ctx, `DELETE FROM user_identities WHERE user_id = $1`, id)
	require.NoError(t, err)
	require.NoError(t, r.Commit(ctx))

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrGoogleLinkRequiresPassword)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for login to return")
	}
	require.Equal(t, before, count(t, env, `SELECT count(*) FROM refresh_tokens WHERE user_id = $1`, id))
	require.Equal(t, 0, count(t, env, `SELECT count(*) FROM user_identities WHERE user_id = $1`, id))
}

func TestIdentityFromClaims(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want GoogleIdentity
		ok   bool
	}{
		{"bool", `{"email":"a@b.com","email_verified":true}`, GoogleIdentity{Subject: "s", Email: "a@b.com", EmailVerified: true}, true},
		{"string", `{"email":"a@b.com","email_verified":"true"}`, GoogleIdentity{Subject: "s", Email: "a@b.com", EmailVerified: true}, true},
		{"false", `{"email":"a@b.com","email_verified":false}`, GoogleIdentity{Subject: "s", Email: "a@b.com"}, true},
		{"hd", `{"email":"a@b.com","email_verified":true,"hd":"b.com"}`, GoogleIdentity{Subject: "s", Email: "a@b.com", EmailVerified: true, HostedDomain: "b.com"}, true},
		{"missing flag", `{"email":"a@b.com"}`, GoogleIdentity{Subject: "s", Email: "a@b.com"}, true},
		{"garbage flag", `{"email":"a@b.com","email_verified":1}`, GoogleIdentity{}, false},
		{"bad json", `{`, GoogleIdentity{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := identityFromClaims("s", []byte(c.raw))
			if !c.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
	_, err := identityFromClaims("", []byte(`{"email":"a@b.com","email_verified":true}`))
	require.Error(t, err)
}
