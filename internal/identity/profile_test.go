package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

func str(s string) *string { return &s }

func newCustomer(t *testing.T, env *testEnv, email string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, env.Svc.Register(ctx, email, testPassword))
	var id uuid.UUID
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&id))
	return id
}

func TestUpdateProfile_PartialUpdate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	id := newCustomer(t, env, "an@gmail.com")

	_, err := env.Svc.UpdateProfile(ctx, id, ProfileUpdate{Phone: str("0912345678"), DateOfBirth: str("1990-05-20")})
	require.NoError(t, err)
	p, err := env.Svc.UpdateProfile(ctx, id, ProfileUpdate{FullName: str("  Nguyễn Văn An ")})
	require.NoError(t, err)

	require.Equal(t, "an@gmail.com", p.Email)
	require.Equal(t, "Nguyễn Văn An", *p.FullName)
	require.Equal(t, "0912345678", *p.Phone)
	require.Equal(t, "1990-05-20", p.DateOfBirth.Format("2006-01-02"))
	got, err := env.Svc.GetProfile(ctx, id)
	require.NoError(t, err)
	require.Equal(t, p, got)
}

func TestUpdateProfile_PhoneNormalization(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	id := newCustomer(t, env, "an@gmail.com")

	p, err := env.Svc.UpdateProfile(ctx, id, ProfileUpdate{Phone: str("+84 912.345.678")})
	require.NoError(t, err)
	require.Equal(t, "0912345678", *p.Phone)

	_, err = env.Svc.UpdateProfile(ctx, id, ProfileUpdate{Phone: str("12345")})
	require.ErrorIs(t, err, ErrInvalidProfile)
	w := httptest.NewRecorder()
	httpx.WriteError(w, httptest.NewRequest(http.MethodPut, "/", nil), err)
	require.Equal(t, 422, w.Code)
	require.Contains(t, w.Body.String(), `"invalid_profile"`)
	require.Contains(t, w.Body.String(), "phone")
}

func TestUpdateProfile_ClearField(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	id := newCustomer(t, env, "an@gmail.com")
	_, err := env.Svc.UpdateProfile(ctx, id, ProfileUpdate{FullName: str("An"), Phone: str("0912345678")})
	require.NoError(t, err)

	p, err := env.Svc.UpdateProfile(ctx, id, ProfileUpdate{FullName: str("")})
	require.NoError(t, err)
	require.Nil(t, p.FullName)
	require.Equal(t, "0912345678", *p.Phone)
}

func TestUpdateProfile_Validation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	id := newCustomer(t, env, "an@gmail.com")

	for name, u := range map[string]ProfileUpdate{
		"future dob":  {DateOfBirth: str("2026-11-05")},
		"today dob":   {DateOfBirth: str("2026-11-04")},
		"garbage dob": {DateOfBirth: str("20/05/1990")},
		"long name":   {FullName: str(strings.Repeat("a", 101))},
	} {
		_, err := env.Svc.UpdateProfile(ctx, id, u)
		require.ErrorIs(t, err, ErrInvalidProfile, name)
	}
	p, err := env.Svc.UpdateProfile(ctx, id, ProfileUpdate{DateOfBirth: str("2026-11-03"), FullName: str(strings.Repeat("ạ", 100))})
	require.NoError(t, err)
	require.NotNil(t, p.DateOfBirth)
	p, err = env.Svc.UpdateProfile(ctx, id, ProfileUpdate{DateOfBirth: str("")})
	require.NoError(t, err)
	require.Nil(t, p.DateOfBirth)
}

func TestUpdateProfile_FutureDOB(t *testing.T) {
	env := newTestEnv(t)
	id := newCustomer(t, env, "an@gmail.com")
	_, err := env.Svc.UpdateProfile(context.Background(), id, ProfileUpdate{DateOfBirth: str("2999-01-01")})
	require.ErrorIs(t, err, ErrInvalidProfile)
	w := httptest.NewRecorder()
	httpx.WriteError(w, httptest.NewRequest(http.MethodPut, "/", nil), err)
	require.Contains(t, w.Body.String(), "date_of_birth")
}

func TestProfile_NotFoundForInternalUser(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	id := uuid.New()
	_, err := env.Pool.Exec(ctx,
		`INSERT INTO users (id, email, role) VALUES ($1, 'ops@x.vn', 'partner')`, id)
	require.NoError(t, err)
	_, err = env.Svc.GetProfile(ctx, id)
	require.ErrorIs(t, err, httpx.ErrNotFound)
	_, err = env.Svc.UpdateProfile(ctx, id, ProfileUpdate{FullName: str("X")})
	require.ErrorIs(t, err, httpx.ErrNotFound)
}

func TestFillProfileIfEmpty_DoesNotOverwrite(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	id := newCustomer(t, env, "an@gmail.com")
	_, err := env.Svc.UpdateProfile(ctx, id, ProfileUpdate{FullName: str("A")})
	require.NoError(t, err)

	require.NoError(t, db.InTx(ctx, env.Pool, func(tx pgx.Tx) error {
		return env.Svc.FillProfileIfEmpty(ctx, tx, id, "B", "+84 987 654 321")
	}))
	p, err := env.Svc.GetProfile(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "A", *p.FullName)
	require.Equal(t, "0987654321", *p.Phone)

	// Invalid values are skipped; a missing row is a no-op.
	id2 := newCustomer(t, env, "bo@gmail.com")
	require.NoError(t, db.InTx(ctx, env.Pool, func(tx pgx.Tx) error {
		return env.Svc.FillProfileIfEmpty(ctx, tx, id2, "", "123")
	}))
	p, err = env.Svc.GetProfile(ctx, id2)
	require.NoError(t, err)
	require.Nil(t, p.FullName)
	require.Nil(t, p.Phone)
	require.NoError(t, db.InTx(ctx, env.Pool, func(tx pgx.Tx) error {
		return env.Svc.FillProfileIfEmpty(ctx, tx, uuid.New(), "B", "0912345678")
	}))
}
