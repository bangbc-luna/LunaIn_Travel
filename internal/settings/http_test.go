package settings_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

func TestHTTP_SettingsRequireSuperAdmin(t *testing.T) {
	pool := testutil.Postgres(t)
	ctx := context.Background()
	clk := &clock.Fixed{T: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	iss := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk)

	idSvc := identity.NewService(identity.Deps{Pool: pool, Clock: clk, CodePepper: []byte("p")})
	var adminID uuid.UUID
	require.NoError(t, db.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		adminID, err = idSvc.CreateInternalUser(ctx, tx, "admin@lunain.vn", "Passw0rd123", auth.RoleSuperAdmin)
		return err
	}))
	adminTok := issue(t, iss, adminID, auth.RoleSuperAdmin)
	custTok := issue(t, iss, uuid.Must(uuid.NewV7()), auth.RoleCustomer)

	mux := http.NewServeMux()
	settings.NewService(pool, clk).Routes(mux, iss)
	mux.HandleFunc("/", httpx.NotFound)
	srv := httptest.NewServer(httpx.Chain(mux, httpx.RequestID, httpx.Recover))
	t.Cleanup(srv.Close)

	do := func(method, path, tok, body string) int {
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		return res.StatusCode
	}

	require.Equal(t, http.StatusUnauthorized, do("GET", "/v1/admin/settings", "", ""))
	require.Equal(t, http.StatusForbidden, do("GET", "/v1/admin/settings", custTok, ""))
	require.Equal(t, http.StatusOK, do("GET", "/v1/admin/settings", adminTok, ""))
	require.Equal(t, http.StatusOK, do("PATCH", "/v1/admin/settings", adminTok, `{"hold_minutes":30}`))
	require.Equal(t, http.StatusUnprocessableEntity, do("PATCH", "/v1/admin/settings", adminTok, `{"hold_minutes":0}`))
	require.Equal(t, http.StatusBadRequest, do("PATCH", "/v1/admin/settings", adminTok, `{"bogus":1}`))
	require.Equal(t, http.StatusForbidden, do("POST", "/v1/admin/holidays", custTok, `{"date":"2026-09-02","name":"x"}`))
	require.Equal(t, http.StatusCreated, do("POST", "/v1/admin/holidays", adminTok, `{"date":"2026-09-02","name":"Quốc khánh"}`))
	require.Equal(t, http.StatusConflict, do("POST", "/v1/admin/holidays", adminTok, `{"date":"2026-09-02","name":"Quốc khánh"}`))
	require.Equal(t, http.StatusBadRequest, do("POST", "/v1/admin/holidays", adminTok, `{"date":"nope","name":"x"}`))
	require.Equal(t, http.StatusOK, do("GET", "/v1/admin/holidays?year=2026", adminTok, ""))
	require.Equal(t, http.StatusBadRequest, do("GET", "/v1/admin/holidays?year=abc", adminTok, ""))
	require.Equal(t, http.StatusNoContent, do("DELETE", "/v1/admin/holidays/2026-09-02", adminTok, ""))
	require.Equal(t, http.StatusNotFound, do("DELETE", "/v1/admin/holidays/2026-09-02", adminTok, ""))
	// A malformed {date} cannot name a holiday → 404 (Ruling 11).
	require.Equal(t, http.StatusNotFound, do("DELETE", "/v1/admin/holidays/2026-13-45", adminTok, ""))
	require.Equal(t, http.StatusNotFound, do("DELETE", "/v1/admin/holidays/nope", adminTok, ""))
	// Holiday name over 100 characters → 422.
	require.Equal(t, http.StatusUnprocessableEntity,
		do("POST", "/v1/admin/holidays", adminTok, `{"date":"2026-09-03","name":"`+strings.Repeat("a", 101)+`"}`))
}

func issue(t *testing.T, iss *auth.Issuer, id uuid.UUID, role auth.Role) string {
	t.Helper()
	tok, _, err := iss.Issue(auth.Principal{UserID: id, Role: role})
	require.NoError(t, err)
	return tok
}
