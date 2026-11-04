package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

var secret = []byte("test-secret-test-secret-test-secret")

func newClock() *clock.Fixed {
	return &clock.Fixed{T: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)}
}

func TestIssuer_RoundTrip(t *testing.T) {
	iss := auth.NewIssuer(secret, 15*time.Minute, newClock())
	p := auth.Principal{UserID: uuid.New(), Role: auth.RoleCustomer}
	tok, exp, err := iss.Issue(p)
	require.NoError(t, err)
	require.Equal(t, newClock().T.Add(15*time.Minute), exp)
	got, err := iss.Verify(tok)
	require.NoError(t, err)
	require.Equal(t, p, got)
}

func TestIssuer_Expired(t *testing.T) {
	c := newClock()
	iss := auth.NewIssuer(secret, 15*time.Minute, c)
	tok, _, err := iss.Issue(auth.Principal{UserID: uuid.New(), Role: auth.RoleCustomer})
	require.NoError(t, err)
	c.Advance(15*time.Minute + time.Second)
	_, err = iss.Verify(tok)
	require.ErrorIs(t, err, auth.ErrInvalidToken)
}

func TestIssuer_WrongSecret(t *testing.T) {
	c := newClock()
	tok, _, err := auth.NewIssuer(secret, 15*time.Minute, c).Issue(auth.Principal{UserID: uuid.New(), Role: auth.RoleCustomer})
	require.NoError(t, err)
	_, err = auth.NewIssuer([]byte("another-secret-another-secret-00"), 15*time.Minute, c).Verify(tok)
	require.ErrorIs(t, err, auth.ErrInvalidToken)
}

func TestIssuer_RejectsNoneAlg(t *testing.T) {
	c := newClock()
	claims := jwt.MapClaims{
		"sub": uuid.NewString(), "role": "super_admin", "iss": "lunain",
		"iat": c.T.Unix(), "exp": c.T.Add(time.Minute).Unix(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	_, err = auth.NewIssuer(secret, 15*time.Minute, c).Verify(tok)
	require.ErrorIs(t, err, auth.ErrInvalidToken)
}

func TestIssuer_RejectsBadIssuerAndRole(t *testing.T) {
	c := newClock()
	sign := func(iss, role string) string {
		s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": uuid.NewString(), "role": role, "iss": iss,
			"iat": c.T.Unix(), "exp": c.T.Add(time.Minute).Unix(),
		}).SignedString(secret)
		require.NoError(t, err)
		return s
	}
	v := auth.NewIssuer(secret, 15*time.Minute, c)
	_, err := v.Verify(sign("evil", "customer"))
	require.ErrorIs(t, err, auth.ErrInvalidToken)
	_, err = v.Verify(sign("lunain", "root"))
	require.ErrorIs(t, err, auth.ErrInvalidToken)
}

func TestRequireRole(t *testing.T) {
	c := newClock()
	iss := auth.NewIssuer(secret, 15*time.Minute, c)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := httpx.Chain(ok, auth.Authenticate(iss), auth.RequireRole(auth.RoleSuperAdmin))

	do := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	cust, _, _ := iss.Issue(auth.Principal{UserID: uuid.New(), Role: auth.RoleCustomer})
	admin, _, _ := iss.Issue(auth.Principal{UserID: uuid.New(), Role: auth.RoleSuperAdmin})
	require.Equal(t, http.StatusUnauthorized, do(""))
	require.Equal(t, http.StatusForbidden, do(cust))
	require.Equal(t, http.StatusOK, do(admin))
}

func TestAuthenticate_MalformedHeader(t *testing.T) {
	iss := auth.NewIssuer(secret, 15*time.Minute, newClock())
	h := auth.Authenticate(iss)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, hdr := range []string{"Token abc", "Bearer", "Bearer garbage"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", hdr)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code, hdr)
	}
}
