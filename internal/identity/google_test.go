package identity

import (
	"context"
	"sync"
	"testing"

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

func TestIdentityFromClaims(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want GoogleIdentity
		ok   bool
	}{
		{"bool", `{"email":"a@b.com","email_verified":true}`, GoogleIdentity{"s", "a@b.com", true}, true},
		{"string", `{"email":"a@b.com","email_verified":"true"}`, GoogleIdentity{"s", "a@b.com", true}, true},
		{"false", `{"email":"a@b.com","email_verified":false}`, GoogleIdentity{"s", "a@b.com", false}, true},
		{"missing flag", `{"email":"a@b.com"}`, GoogleIdentity{"s", "a@b.com", false}, true},
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
