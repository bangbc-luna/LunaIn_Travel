package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/payout"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/maps"
	"github.com/bangbc-luna/lunain-travel/internal/platform/storage"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
	"github.com/bangbc-luna/lunain-travel/internal/reconcile"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

var testNow = time.Date(2026, 11, 4, 9, 0, 0, 0, time.FixedZone("ICT", 7*3600))

type testApp struct {
	handler  http.Handler
	issuer   *auth.Issuer
	pool     *pgxpool.Pool
	identity *identity.Service
	partner  *partner.Service
	// patterns are all patterns recorded at registration time.
	patterns []string
}

// newTestApp builds the real mux over a fresh Postgres, in-memory storage and
// a fixed clock.
func newTestApp(t *testing.T) *testApp {
	t.Helper()
	return newTestAppEnv(t, "test")
}

// newTestAppEnv is newTestApp with APP_ENV = appEnv (payment.Fake provider).
func newTestAppEnv(t *testing.T, appEnv string) *testApp {
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
	settingsSvc := settings.NewService(pool, clk)
	listingSvc := listing.NewService(listing.Deps{
		Pool: pool, Clock: clk, Partners: partnerSvc, Catalog: catSvc, Geo: geoSvc, Files: fileSvc,
		BookingGuard: booking.Guard{}, Maps: maps.NewFake(), Holidays: settingsSvc,
	})
	paymentSvc := payment.NewService(pool, clk, payment.NewFake(), nil)
	ledgerSvc := ledger.NewService(pool, clk)
	bookingSvc := booking.NewService(booking.Deps{
		Pool: pool, Clock: clk, Listings: listingSvc, Catalog: catSvc, Settings: settingsSvc,
		Identity: idSvc, Payments: paymentSvc, Ledger: ledgerSvc,
	})
	payoutSvc := payout.NewService(payout.Deps{
		Pool: pool, Clock: clk, Ledger: ledgerSvc, Partners: partnerSvc, Settings: settingsSvc, Files: fileSvc,
	})
	reconcileSvc := reconcile.NewService(reconcile.Deps{
		Pool: pool, Clock: clk, Payments: paymentSvc, Ledger: ledgerSvc, Handler: bookingSvc, Identity: idSvc,
	})
	mux, patterns := buildMux(muxDeps{
		Pool: pool, Issuer: issuer, ClientIP: httpx.ClientIPFunc(0),
		Identity: idSvc, Settings: settingsSvc, Files: fileSvc,
		Geo: geoSvc, Catalog: catSvc, Partner: partnerSvc, Listing: listingSvc,
		Ledger: ledgerSvc, Payment: paymentSvc, Booking: bookingSvc, Payout: payoutSvc, Reconcile: reconcileSvc,
		AppEnv: appEnv,
	})
	return &testApp{
		handler: httpx.Chain(mux, httpx.RequestID, httpx.Recover), issuer: issuer,
		pool: pool, identity: idSvc, partner: partnerSvc, patterns: patterns,
	}
}

// token issues an access token for role with a random user id (no DB row).
func (a *testApp) token(t *testing.T, role auth.Role) string {
	t.Helper()
	return a.tokenFor(t, uuid.New(), role)
}

func (a *testApp) tokenFor(t *testing.T, userID uuid.UUID, role auth.Role) string {
	t.Helper()
	s, _, err := a.issuer.Issue(auth.Principal{UserID: userID, Role: role})
	require.NoError(t, err)
	return s
}

// superAdmin creates a real super_admin user and returns its id.
func (a *testApp) superAdmin(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	require.NoError(t, db.InTx(ctx, a.pool, func(tx pgx.Tx) error {
		var err error
		id, err = a.identity.CreateInternalUser(ctx, tx, "admin@lunain.vn", "adminpass123", auth.RoleSuperAdmin)
		return err
	}))
	return id
}

// partnerMember creates a real partner through the services and returns the
// owner's user id (role partner, member of the partner).
func (a *testApp) partnerMember(t *testing.T, adminID uuid.UUID) uuid.UUID {
	t.Helper()
	p, err := a.partner.Create(context.Background(), adminID, partner.CreateInput{
		LegalName: "Công ty Alpha", DisplayName: "Alpha",
		OwnerEmail: "owner@alpha.vn", OwnerPassword: "matkhau123",
		ServiceTypes: []string{"attraction_ticket"},
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, p.OwnerUserID)
	return p.OwnerUserID
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

// requireError asserts the response status and JSON error code.
func requireError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, msg string) {
	t.Helper()
	require.Equal(t, status, rec.Code, "%s: %s", msg, rec.Body.String())
	var eb struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &eb), "%s: %s", msg, rec.Body.String())
	require.Equal(t, code, eb.Error.Code, msg)
}

// requireAuthorized asserts the request got past authentication and
// authorization (any status but 401/403).
func requireAuthorized(t *testing.T, rec *httptest.ResponseRecorder, msg string) {
	t.Helper()
	require.NotEqual(t, http.StatusUnauthorized, rec.Code, "%s: %s", msg, rec.Body.String())
	require.NotEqual(t, http.StatusForbidden, rec.Code, "%s: %s", msg, rec.Body.String())
}

type route struct{ pattern, sample string }

func (r route) method() string { return strings.SplitN(r.pattern, " ", 2)[0] }

// adminRoutes lists every /v1/admin/* route: the registered pattern and a
// sample request path. TestRouteListsAreComplete fails when a route is added
// or removed in a module without updating this list.
var adminRoutes = []route{
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
	{"GET /v1/admin/partners/{id}/ledger", "/v1/admin/partners/" + uuid.Nil.String() + "/ledger"},
	{"POST /v1/admin/partners/{id}/adjustments", "/v1/admin/partners/" + uuid.Nil.String() + "/adjustments"},
	{"GET /v1/admin/listings", "/v1/admin/listings"},
	{"GET /v1/admin/listings/{id}", "/v1/admin/listings/" + uuid.Nil.String()},
	{"POST /v1/admin/listings/{id}/approve", "/v1/admin/listings/" + uuid.Nil.String() + "/approve"},
	{"POST /v1/admin/listings/{id}/reject", "/v1/admin/listings/" + uuid.Nil.String() + "/reject"},
	{"POST /v1/admin/listings/{id}/hide", "/v1/admin/listings/" + uuid.Nil.String() + "/hide"},
	{"POST /v1/admin/listings/{id}/unhide", "/v1/admin/listings/" + uuid.Nil.String() + "/unhide"},
	{"GET /v1/admin/listings/{id}/ride-trips", "/v1/admin/listings/" + uuid.Nil.String() + "/ride-trips"},
	{"GET /v1/admin/bookings", "/v1/admin/bookings"},
	{"GET /v1/admin/bookings/{id}", "/v1/admin/bookings/" + uuid.Nil.String()},
	{"POST /v1/admin/bookings/{id}/cancel", "/v1/admin/bookings/" + uuid.Nil.String() + "/cancel"},
	{"POST /v1/admin/ride-trips/{id}/cancel", "/v1/admin/ride-trips/" + uuid.Nil.String() + "/cancel"},
	{"GET /v1/admin/payouts", "/v1/admin/payouts"},
	{"GET /v1/admin/payouts/{id}", "/v1/admin/payouts/" + uuid.Nil.String()},
	{"POST /v1/admin/payouts/{id}/approve", "/v1/admin/payouts/" + uuid.Nil.String() + "/approve"},
	{"POST /v1/admin/payouts/{id}/mark-paid", "/v1/admin/payouts/" + uuid.Nil.String() + "/mark-paid"},
	{"POST /v1/admin/payouts/{id}/reject", "/v1/admin/payouts/" + uuid.Nil.String() + "/reject"},
	{"GET /v1/admin/refunds", "/v1/admin/refunds"},
	{"GET /v1/admin/refunds/{id}", "/v1/admin/refunds/" + uuid.Nil.String()},
	{"POST /v1/admin/refunds/{id}/complete", "/v1/admin/refunds/" + uuid.Nil.String() + "/complete"},
	{"GET /v1/admin/payments", "/v1/admin/payments"},
	{"GET /v1/admin/payments/{id}", "/v1/admin/payments/" + uuid.Nil.String()},
	{"POST /v1/admin/payments/{id}/seen", "/v1/admin/payments/" + uuid.Nil.String() + "/seen"},
	{"POST /v1/admin/payments/{id}/resolve", "/v1/admin/payments/" + uuid.Nil.String() + "/resolve"},
	{"GET /v1/admin/reconciliations", "/v1/admin/reconciliations"},
	{"GET /v1/admin/reconciliations/{run_date}", "/v1/admin/reconciliations/2026-11-04"},
	{"POST /v1/admin/reconciliations/run", "/v1/admin/reconciliations/run"},
}

// partnerRoutes lists every /v1/partner/* route, like adminRoutes.
var partnerRoutes = []route{
	{"GET /v1/partner/profile", "/v1/partner/profile"},
	{"PATCH /v1/partner/profile", "/v1/partner/profile"},
	{"GET /v1/partner/bank-accounts", "/v1/partner/bank-accounts"},
	{"PUT /v1/partner/bank-account", "/v1/partner/bank-account"},
	{"POST /v1/partner/listings", "/v1/partner/listings"},
	{"GET /v1/partner/listings", "/v1/partner/listings"},
	{"GET /v1/partner/listings/{id}", "/v1/partner/listings/" + uuid.Nil.String()},
	{"PATCH /v1/partner/listings/{id}", "/v1/partner/listings/" + uuid.Nil.String()},
	{"PUT /v1/partner/listings/{id}/images", "/v1/partner/listings/" + uuid.Nil.String() + "/images"},
	{"GET /v1/partner/listings/{id}/cancellation-policy", "/v1/partner/listings/" + uuid.Nil.String() + "/cancellation-policy"},
	{"PUT /v1/partner/listings/{id}/cancellation-policy", "/v1/partner/listings/" + uuid.Nil.String() + "/cancellation-policy"},
	{"PUT /v1/partner/listings/{id}/attraction", "/v1/partner/listings/" + uuid.Nil.String() + "/attraction"},
	{"GET /v1/partner/listings/{id}/attraction", "/v1/partner/listings/" + uuid.Nil.String() + "/attraction"},
	{"POST /v1/partner/listings/{id}/ticket-types", "/v1/partner/listings/" + uuid.Nil.String() + "/ticket-types"},
	{"PATCH /v1/partner/listings/{id}/ticket-types/{ticketTypeID}", "/v1/partner/listings/" + uuid.Nil.String() + "/ticket-types/" + uuid.Nil.String()},
	{"POST /v1/partner/listings/{id}/time-slots", "/v1/partner/listings/" + uuid.Nil.String() + "/time-slots"},
	{"PATCH /v1/partner/listings/{id}/time-slots/{slotID}", "/v1/partner/listings/" + uuid.Nil.String() + "/time-slots/" + uuid.Nil.String()},
	{"PUT /v1/partner/listings/{id}/closed-dates/{date}", "/v1/partner/listings/" + uuid.Nil.String() + "/closed-dates/2099-01-01"},
	{"DELETE /v1/partner/listings/{id}/closed-dates/{date}", "/v1/partner/listings/" + uuid.Nil.String() + "/closed-dates/2099-01-01"},
	{"GET /v1/partner/listings/{id}/ride", "/v1/partner/listings/" + uuid.Nil.String() + "/ride"},
	{"PUT /v1/partner/listings/{id}/ride-route", "/v1/partner/listings/" + uuid.Nil.String() + "/ride-route"},
	{"POST /v1/partner/listings/{id}/ride-schedules", "/v1/partner/listings/" + uuid.Nil.String() + "/ride-schedules"},
	{"PATCH /v1/partner/listings/{id}/ride-schedules/{scheduleID}", "/v1/partner/listings/" + uuid.Nil.String() + "/ride-schedules/" + uuid.Nil.String()},
	{"GET /v1/partner/listings/{id}/rental", "/v1/partner/listings/" + uuid.Nil.String() + "/rental"},
	{"PUT /v1/partner/listings/{id}/rental-store", "/v1/partner/listings/" + uuid.Nil.String() + "/rental-store"},
	{"POST /v1/partner/listings/{id}/rental-pickup-points", "/v1/partner/listings/" + uuid.Nil.String() + "/rental-pickup-points"},
	{"PATCH /v1/partner/listings/{id}/rental-pickup-points/{pointID}", "/v1/partner/listings/" + uuid.Nil.String() + "/rental-pickup-points/" + uuid.Nil.String()},
	{"POST /v1/partner/listings/{id}/rental-models", "/v1/partner/listings/" + uuid.Nil.String() + "/rental-models"},
	{"PATCH /v1/partner/listings/{id}/rental-models/{modelID}", "/v1/partner/listings/" + uuid.Nil.String() + "/rental-models/" + uuid.Nil.String()},
	{"GET /v1/partner/listings/{id}/charter", "/v1/partner/listings/" + uuid.Nil.String() + "/charter"},
	{"PUT /v1/partner/listings/{id}/charter-service", "/v1/partner/listings/" + uuid.Nil.String() + "/charter-service"},
	{"POST /v1/partner/listings/{id}/charter-classes", "/v1/partner/listings/" + uuid.Nil.String() + "/charter-classes"},
	{"PATCH /v1/partner/listings/{id}/charter-classes/{classID}", "/v1/partner/listings/" + uuid.Nil.String() + "/charter-classes/" + uuid.Nil.String()},
	{"PUT /v1/partner/listings/{id}/charter-classes/{classID}/surcharges", "/v1/partner/listings/" + uuid.Nil.String() + "/charter-classes/" + uuid.Nil.String() + "/surcharges"},
	{"GET /v1/partner/listings/{id}/ride-trips", "/v1/partner/listings/" + uuid.Nil.String() + "/ride-trips"},
	{"POST /v1/partner/listings/{id}/ride-trips", "/v1/partner/listings/" + uuid.Nil.String() + "/ride-trips"},
	{"PATCH /v1/partner/listings/{id}/ride-trips/{tripID}", "/v1/partner/listings/" + uuid.Nil.String() + "/ride-trips/" + uuid.Nil.String()},
	{"POST /v1/partner/listings/{id}/ride-trips/{tripID}/cancel", "/v1/partner/listings/" + uuid.Nil.String() + "/ride-trips/" + uuid.Nil.String() + "/cancel"},
	{"POST /v1/partner/listings/{id}/submit", "/v1/partner/listings/" + uuid.Nil.String() + "/submit"},
	{"POST /v1/partner/listings/{id}/publish", "/v1/partner/listings/" + uuid.Nil.String() + "/publish"},
	{"POST /v1/partner/listings/{id}/unpublish", "/v1/partner/listings/" + uuid.Nil.String() + "/unpublish"},
	{"GET /v1/partner/balance", "/v1/partner/balance"},
	{"GET /v1/partner/bookings", "/v1/partner/bookings"},
	{"GET /v1/partner/bookings/{id}", "/v1/partner/bookings/" + uuid.Nil.String()},
	{"POST /v1/partner/bookings/{id}/accept", "/v1/partner/bookings/" + uuid.Nil.String() + "/accept"},
	{"POST /v1/partner/bookings/{id}/reject", "/v1/partner/bookings/" + uuid.Nil.String() + "/reject"},
	{"POST /v1/partner/bookings/{id}/no-show", "/v1/partner/bookings/" + uuid.Nil.String() + "/no-show"},
	{"POST /v1/partner/bookings/{id}/return", "/v1/partner/bookings/" + uuid.Nil.String() + "/return"},
	{"POST /v1/partner/bookings/{id}/complete", "/v1/partner/bookings/" + uuid.Nil.String() + "/complete"},
	{"POST /v1/partner/check-ins", "/v1/partner/check-ins"},
	{"POST /v1/partner/payouts", "/v1/partner/payouts"},
	{"GET /v1/partner/payouts", "/v1/partner/payouts"},
	{"GET /v1/partner/payouts/preview", "/v1/partner/payouts/preview"},
	{"GET /v1/partner/payouts/{id}", "/v1/partner/payouts/" + uuid.Nil.String()},
}

// recordedUnder returns the recorded patterns whose path starts with prefix.
// A pattern without a method (no space) is a path on its own.
func recordedUnder(patterns []string, prefix string) []string {
	var out []string
	for _, p := range patterns {
		path := p
		if _, after, ok := strings.Cut(p, " "); ok {
			path = after
		}
		if strings.HasPrefix(path, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func declaredPatterns(rs []route) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.pattern)
	}
	sort.Strings(out)
	return out
}

func TestRouteListsAreComplete(t *testing.T) {
	patterns := newTestApp(t).patterns
	require.Equal(t, recordedUnder(patterns, "/v1/admin/"), declaredPatterns(adminRoutes),
		"adminRoutes in routes_test.go must list every /v1/admin route registered by the modules")
	require.Equal(t, recordedUnder(patterns, "/v1/partner/"), declaredPatterns(partnerRoutes),
		"partnerRoutes in routes_test.go must list every /v1/partner route registered by the modules")
}

func TestRecordedUnder_MethodlessPattern(t *testing.T) {
	got := recordedUnder([]string{"/v1/admin/x", "GET /v1/admin/y", "/v1/public", "POST /v1/partner/z"}, "/v1/admin/")
	require.Equal(t, []string{"/v1/admin/x", "GET /v1/admin/y"}, got)
}

func TestAllAdminRoutesRequireSuperAdmin(t *testing.T) {
	app := newTestApp(t)
	adminID := app.superAdmin(t)
	adminTok := app.tokenFor(t, adminID, auth.RoleSuperAdmin)
	customer := app.token(t, auth.RoleCustomer)
	member := app.tokenFor(t, app.partnerMember(t, adminID), auth.RolePartner)
	for _, r := range adminRoutes {
		t.Run(r.pattern, func(t *testing.T) {
			m := r.method()
			requireError(t, app.do(m, r.sample, ""), http.StatusUnauthorized, "unauthorized", "no token")
			requireError(t, app.do(m, r.sample, "garbage"), http.StatusUnauthorized, "unauthorized", "bad token")
			requireError(t, app.do(m, r.sample, customer), http.StatusForbidden, "forbidden", "customer")
			requireError(t, app.do(m, r.sample, member), http.StatusForbidden, "forbidden", "partner member")
			requireAuthorized(t, app.do(m, r.sample, adminTok), "super_admin")
		})
	}
}

func TestAllPartnerRoutesRequireMembership(t *testing.T) {
	app := newTestApp(t)
	adminID := app.superAdmin(t)
	adminTok := app.tokenFor(t, adminID, auth.RoleSuperAdmin)
	customer := app.token(t, auth.RoleCustomer)
	nonMember := app.token(t, auth.RolePartner)
	member := app.tokenFor(t, app.partnerMember(t, adminID), auth.RolePartner)
	for _, r := range partnerRoutes {
		t.Run(r.pattern, func(t *testing.T) {
			m := r.method()
			requireError(t, app.do(m, r.sample, ""), http.StatusUnauthorized, "unauthorized", "no token")
			requireError(t, app.do(m, r.sample, "garbage"), http.StatusUnauthorized, "unauthorized", "bad token")
			requireError(t, app.do(m, r.sample, customer), http.StatusForbidden, "forbidden", "customer")
			requireError(t, app.do(m, r.sample, adminTok), http.StatusForbidden, "forbidden", "super_admin")
			requireError(t, app.do(m, r.sample, nonMember), http.StatusForbidden, "forbidden", "partner without membership")
			requireAuthorized(t, app.do(m, r.sample, member), "partner member")
		})
	}
}

func TestPublicRoutes(t *testing.T) {
	app := newTestApp(t)
	for _, p := range []string{"/v1/geo/provinces", "/v1/service-types", "/v1/listings", "/healthz", "/readyz"} {
		require.Equal(t, http.StatusOK, app.do("GET", p, "").Code, p)
	}
	// A public listing detail needs no token: an unknown slug is a JSON 404.
	rec := app.do("GET", "/v1/listings/khong-ton-tai", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), `"not_found"`)
	require.Equal(t, []string{"GET /v1/listings", "GET /v1/listings/{slug}"}, recordedUnder(app.patterns, "/v1/listings"))
	// The rental vehicle search needs no token: missing parameters are a 400,
	// not a 401.
	require.Equal(t, []string{"GET /v1/rental-vehicles"}, recordedUnder(app.patterns, "/v1/rental-vehicles"))
	requireError(t, app.do("GET", "/v1/rental-vehicles", ""), http.StatusBadRequest, "invalid_request", "rental search without parameters")
	// The charter quote needs no token: an empty quote is a 422, not a 401.
	require.Equal(t, []string{"POST /v1/charter-quotes"}, recordedUnder(app.patterns, "/v1/charter-quotes"))
	requireError(t, app.do("POST", "/v1/charter-quotes", ""), http.StatusUnprocessableEntity, "invalid_location", "empty charter quote")
	rec = app.do("GET", "/nope", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), `"error"`)
}

// customerBookingRoutes are the customer-only booking routes.
var customerBookingRoutes = []route{
	{"POST /v1/bookings/attraction", "/v1/bookings/attraction"},
	{"POST /v1/bookings/ride", "/v1/bookings/ride"},
	{"POST /v1/bookings/rental", "/v1/bookings/rental"},
	{"POST /v1/bookings/charter", "/v1/bookings/charter"},
	{"GET /v1/bookings", "/v1/bookings"},
	{"GET /v1/bookings/{id}", "/v1/bookings/" + uuid.Nil.String()},
	{"POST /v1/bookings/{id}/pay", "/v1/bookings/" + uuid.Nil.String() + "/pay"},
	{"POST /v1/bookings/{id}/cancel", "/v1/bookings/" + uuid.Nil.String() + "/cancel"},
	{"POST /v1/bookings/{id}/reschedule", "/v1/bookings/" + uuid.Nil.String() + "/reschedule"},
	{"GET /v1/refunds", "/v1/refunds"},
	{"PUT /v1/refunds/{id}/bank-account", "/v1/refunds/" + uuid.Nil.String() + "/bank-account"},
	{"GET /v1/customer/refund-account", "/v1/customer/refund-account"},
	{"PUT /v1/customer/refund-account", "/v1/customer/refund-account"},
}

func TestCustomerBookingRoutesRequireCustomer(t *testing.T) {
	app := newTestApp(t)
	var recorded []string
	for _, prefix := range []string{"/v1/bookings", "/v1/refunds", "/v1/customer/refund-account"} {
		recorded = append(recorded, recordedUnder(app.patterns, prefix)...)
	}
	sort.Strings(recorded)
	require.Equal(t, declaredPatterns(customerBookingRoutes), recorded)
	adminID := app.superAdmin(t)
	adminTok := app.tokenFor(t, adminID, auth.RoleSuperAdmin)
	member := app.tokenFor(t, app.partnerMember(t, adminID), auth.RolePartner)
	customer := app.token(t, auth.RoleCustomer)
	for _, r := range customerBookingRoutes {
		t.Run(r.pattern, func(t *testing.T) {
			m := r.method()
			requireError(t, app.do(m, r.sample, ""), http.StatusUnauthorized, "unauthorized", "no token")
			requireError(t, app.do(m, r.sample, member), http.StatusForbidden, "forbidden", "partner member")
			requireError(t, app.do(m, r.sample, adminTok), http.StatusForbidden, "forbidden", "super_admin")
			requireAuthorized(t, app.do(m, r.sample, customer), "customer")
		})
	}
}

const simulatePaidRoute = "POST /v1/dev/payments/{order_code}/simulate-paid"

func TestPaymentWebhookIsPublic(t *testing.T) {
	app := newTestApp(t)
	require.Contains(t, app.patterns, "POST /v1/payments/payos/webhook")
	// No token needed: a bad body is rejected by the signature check, not auth.
	requireError(t, app.do("POST", "/v1/payments/payos/webhook", ""), http.StatusBadRequest, "invalid_signature",
		"webhook without token")
}

func TestDevSimulateRoute_FakeOutsideProdOnly(t *testing.T) {
	require.Contains(t, newTestAppEnv(t, "dev").patterns, simulatePaidRoute)
	require.Contains(t, newTestAppEnv(t, "test").patterns, simulatePaidRoute)
	prod := newTestAppEnv(t, "prod")
	require.NotContains(t, prod.patterns, simulatePaidRoute)
	require.Equal(t, http.StatusNotFound, prod.do("POST", "/v1/dev/payments/100000/simulate-paid", "").Code)
}
