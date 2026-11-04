package partner_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

func (e *env) issuer() *auth.Issuer {
	return auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, e.Clock)
}

func (e *env) token(t *testing.T, userID uuid.UUID, role auth.Role) string {
	t.Helper()
	s, _, err := e.issuer().Issue(auth.Principal{UserID: userID, Role: role})
	require.NoError(t, err)
	return s
}

// server serves the partner routes the way cmd/api wires them.
func (e *env) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	e.Svc.Routes(mux, e.issuer())
	e.Svc.AdminRoutes(mux, e.issuer())
	mux.HandleFunc("/", httpx.NotFound)
	srv := httptest.NewServer(httpx.Chain(mux, httpx.RequestID, httpx.Recover))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, tok, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, b
}

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var eb struct {
		Error struct{ Code string } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &eb), string(body))
	return eb.Error.Code
}

// memberChain runs h behind the same middleware stack as the partner routes.
func (e *env) memberChain(h http.HandlerFunc) http.Handler {
	return httpx.Chain(h, auth.Authenticate(e.issuer()), auth.RequireRole(auth.RolePartner), e.Svc.RequireMember())
}

func serve(t *testing.T, h http.Handler, tok string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRequireMember_SetsPartnerID(t *testing.T) {
	e := newEnv(t)
	a := e.create(t, input("alpha"))
	e.create(t, input("beta"))

	var got uuid.UUID
	var ok bool
	h := e.memberChain(func(w http.ResponseWriter, r *http.Request) {
		got, ok = partner.IDFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	w := serve(t, h, e.token(t, a.OwnerUserID, auth.RolePartner))
	require.Equal(t, http.StatusNoContent, w.Code)
	require.True(t, ok)
	require.Equal(t, a.ID, got)

	_, ok = partner.IDFrom(context.Background())
	require.False(t, ok)
}

func TestRequireMember_PartnerRoleWithoutMembership403(t *testing.T) {
	e := newEnv(t)
	var lone uuid.UUID
	require.NoError(t, db.InTx(context.Background(), e.Pool, func(tx pgx.Tx) error {
		var err error
		lone, err = e.Identity.CreateInternalUser(context.Background(), tx, "lone@partner.vn", ownerPassword, auth.RolePartner)
		return err
	}))
	called := false
	h := e.memberChain(func(w http.ResponseWriter, r *http.Request) { called = true })
	w := serve(t, h, e.token(t, lone, auth.RolePartner))
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, "forbidden", errCode(t, w.Body.Bytes()))
	require.False(t, called)
}
