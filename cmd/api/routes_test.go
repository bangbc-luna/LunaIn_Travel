package main

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/storage"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

var testNow = time.Date(2026, 11, 4, 9, 0, 0, 0, time.FixedZone("ICT", 7*3600))

type testApp struct {
	handler http.Handler
	issuer  *auth.Issuer
	// patterns are all patterns recorded at registration time.
	patterns []string
}

// newTestApp builds the real mux over a fresh Postgres, in-memory storage and
// a fixed clock.
func newTestApp(t *testing.T) *testApp {
	t.Helper()
	pool := testutil.Postgres(t)
	clk := &clock.Fixed{T: testNow}
	issuer := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk)
	idSvc := identity.NewService(identity.Deps{
		Pool: pool, Issuer: issuer, Clock: clk,
		CodePepper: []byte("test-pepper"), RefreshTTL: 30 * 24 * time.Hour,
	})
	fileSvc := files.NewService(pool, storage.NewMemory(), clk, "http://files.test")
	geoSvc := geo.NewService(pool)
	catSvc := catalog.NewService(pool, clk)
	partnerSvc := partner.NewService(partner.Deps{
		Pool: pool, Clock: clk, Users: idSvc, Catalog: catSvc, Geo: geoSvc, Files: fileSvc,
	})
	mux, patterns := buildMux(muxDeps{
		Pool: pool, Issuer: issuer, ClientIP: httpx.ClientIPFunc(0),
		Identity: idSvc, Settings: settings.NewService(pool, clk), Files: fileSvc,
		Geo: geoSvc, Catalog: catSvc, Partner: partnerSvc,
	})
	return &testApp{handler: httpx.Chain(mux, httpx.RequestID, httpx.Recover), issuer: issuer, patterns: patterns}
}

func (a *testApp) token(t *testing.T, role auth.Role) string {
	t.Helper()
	s, _, err := a.issuer.Issue(auth.Principal{UserID: uuid.New(), Role: role})
	require.NoError(t, err)
	return s
}

func (a *testApp) do(method, path, tok string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

// adminRoutes lists every /v1/admin/* route: the registered pattern and a
// sample request path. TestAdminRouteListIsComplete fails when a route is
// added or removed in a module without updating this list.
var adminRoutes = []struct{ pattern, sample string }{
	{"GET /v1/admin/settings", "/v1/admin/settings"},
	{"PATCH /v1/admin/settings", "/v1/admin/settings"},
	{"GET /v1/admin/holidays", "/v1/admin/holidays?year=2026"},
	{"POST /v1/admin/holidays", "/v1/admin/holidays"},
	{"DELETE /v1/admin/holidays/{date}", "/v1/admin/holidays/2026-09-02"},
	{"GET /v1/admin/commission-rates", "/v1/admin/commission-rates"},
	{"GET /v1/admin/commission-rates/{service_type}/history", "/v1/admin/commission-rates/attraction_ticket/history"},
	{"POST /v1/admin/commission-rates", "/v1/admin/commission-rates"},
	{"POST /v1/admin/partners", "/v1/admin/partners"},
	{"GET /v1/admin/partners", "/v1/admin/partners"},
	{"GET /v1/admin/partners/{id}", "/v1/admin/partners/" + uuid.Nil.String()},
	{"PUT /v1/admin/partners/{id}/service-types/{code}", "/v1/admin/partners/" + uuid.Nil.String() + "/service-types/attraction_ticket"},
	{"DELETE /v1/admin/partners/{id}/service-types/{code}", "/v1/admin/partners/" + uuid.Nil.String() + "/service-types/attraction_ticket"},
	{"POST /v1/admin/partners/{id}/suspend", "/v1/admin/partners/" + uuid.Nil.String() + "/suspend"},
	{"POST /v1/admin/partners/{id}/unsuspend", "/v1/admin/partners/" + uuid.Nil.String() + "/unsuspend"},
}

func TestAdminRouteListIsComplete(t *testing.T) {
	declared := make([]string, 0, len(adminRoutes))
	for _, r := range adminRoutes {
		declared = append(declared, r.pattern)
	}
	sort.Strings(declared)
	var registered []string
	for _, p := range (newTestApp(t)).patterns {
		if _, path, _ := strings.Cut(p, " "); strings.HasPrefix(path, "/v1/admin/") {
			registered = append(registered, p)
		}
	}
	sort.Strings(registered)
	require.Equal(t, registered, declared,
		"adminRoutes in routes_test.go must list every /v1/admin route registered by the modules")
}

func TestAllAdminRoutesRejectNonAdmins(t *testing.T) {
	app := newTestApp(t)
	customer := app.token(t, auth.RoleCustomer)
	partnerTok := app.token(t, auth.RolePartner)
	for _, r := range adminRoutes {
		method := strings.SplitN(r.pattern, " ", 2)[0]
		t.Run(r.pattern, func(t *testing.T) {
			require.Equal(t, http.StatusUnauthorized, app.do(method, r.sample, "").Code, "no token")
			require.Equal(t, http.StatusUnauthorized, app.do(method, r.sample, "garbage").Code, "bad token")
			require.Equal(t, http.StatusForbidden, app.do(method, r.sample, customer).Code, "customer")
			require.Equal(t, http.StatusForbidden, app.do(method, r.sample, partnerTok).Code, "partner")
		})
	}
}

func TestPublicRoutes(t *testing.T) {
	app := newTestApp(t)
	for _, p := range []string{"/v1/geo/provinces", "/v1/service-types", "/healthz", "/readyz"} {
		require.Equal(t, http.StatusOK, app.do("GET", p, "").Code, p)
	}
	rec := app.do("GET", "/nope", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), `"error"`)
}
