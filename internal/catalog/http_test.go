package catalog_test

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
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

func newServer(t *testing.T, e *env) (srv *httptest.Server, adminTok, partnerTok string) {
	t.Helper()
	clk := &clock.Fixed{T: now}
	iss := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk)
	tok := func(id uuid.UUID, role auth.Role) string {
		s, _, err := iss.Issue(auth.Principal{UserID: id, Role: role})
		require.NoError(t, err)
		return s
	}
	mux := http.NewServeMux()
	e.Svc.Routes(mux, iss)
	mux.HandleFunc("/", httpx.NotFound)
	srv = httptest.NewServer(httpx.Chain(mux, httpx.RequestID, httpx.Recover))
	t.Cleanup(srv.Close)
	return srv, tok(e.Admin, auth.RoleSuperAdmin), tok(uuid.Must(uuid.NewV7()), auth.RolePartner)
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

func TestHTTP_ServiceTypesPublic(t *testing.T) {
	srv, _, _ := newServer(t, newEnv(t))
	status, body := do(t, srv, "GET", "/v1/service-types", "", "")
	require.Equal(t, http.StatusOK, status)
	var list []catalog.ServiceType
	require.NoError(t, json.Unmarshal(body, &list))
	codes := make([]string, len(list))
	for i, st := range list {
		codes[i] = st.Code
	}
	require.Equal(t, []string{"attraction_ticket", "scheduled_ride", "private_charter", "self_drive_rental"}, codes)
	require.Equal(t, "Vé tham quan", list[0].Name)
	require.Equal(t, "instant", list[0].BookingMode)
	require.Equal(t, "request", list[3].BookingMode)
}

func TestHTTP_CreateRateRequiresFields(t *testing.T) {
	e := newEnv(t)
	srv, adminTok, _ := newServer(t, e)
	for _, c := range []struct{ body, code string }{
		{`{"service_type":"attraction_ticket"}`, "invalid_commission_rate"},
		{`{"service_type":"attraction_ticket","rate_bps":null}`, "invalid_commission_rate"},
		{`{"rate_bps":1000}`, "invalid_service_type"},
		// An unknown code in the body is a field error: stays 422 (Ruling 11).
		{`{"service_type":"nope","rate_bps":1000}`, "invalid_service_type"},
	} {
		s, body := do(t, srv, "POST", "/v1/admin/commission-rates", adminTok, c.body)
		require.Equal(t, http.StatusUnprocessableEntity, s, c.body)
		require.Contains(t, string(body), c.code, c.body)
	}
	var rates, audits int
	require.NoError(t, e.Pool.QueryRow(context.Background(), `SELECT count(*) FROM commission_rates`).Scan(&rates))
	require.NoError(t, e.Pool.QueryRow(context.Background(), `SELECT count(*) FROM admin_audit_log WHERE action = 'commission_rate.create'`).Scan(&audits))
	require.Zero(t, rates)
	require.Zero(t, audits)

	// An explicit 0 is still accepted.
	s, _ := do(t, srv, "POST", "/v1/admin/commission-rates", adminTok, `{"service_type":"attraction_ticket","rate_bps":0}`)
	require.Equal(t, http.StatusCreated, s)
}

func TestHTTP_CommissionAdminOnly(t *testing.T) {
	srv, adminTok, partnerTok := newServer(t, newEnv(t))
	routes := []struct{ method, path, body string }{
		{"GET", "/v1/admin/commission-rates", ""},
		{"GET", "/v1/admin/commission-rates/attraction_ticket/history", ""},
		{"POST", "/v1/admin/commission-rates", `{"service_type":"attraction_ticket","rate_bps":1000}`},
	}
	for _, r := range routes {
		s, _ := do(t, srv, r.method, r.path, "", r.body)
		require.Equal(t, http.StatusUnauthorized, s, r.path)
		s, _ = do(t, srv, r.method, r.path, partnerTok, r.body)
		require.Equal(t, http.StatusForbidden, s, r.path)
	}

	s, _ := do(t, srv, "POST", "/v1/admin/commission-rates", adminTok, `{"service_type":"attraction_ticket","rate_bps":1000}`)
	require.Equal(t, http.StatusCreated, s)
	s, _ = do(t, srv, "POST", "/v1/admin/commission-rates", adminTok, `{"service_type":"attraction_ticket","rate_bps":1000}`)
	require.Equal(t, http.StatusConflict, s)
	s, _ = do(t, srv, "POST", "/v1/admin/commission-rates", adminTok, `{"service_type":"attraction_ticket","rate_bps":1,"bogus":1}`)
	require.Equal(t, http.StatusBadRequest, s)

	later := now.Add(48 * time.Hour).Format(time.RFC3339)
	s, _ = do(t, srv, "POST", "/v1/admin/commission-rates", adminTok,
		`{"service_type":"attraction_ticket","rate_bps":1500,"effective_from":"`+later+`"}`)
	require.Equal(t, http.StatusCreated, s)

	s, body := do(t, srv, "GET", "/v1/admin/commission-rates", adminTok, "")
	require.Equal(t, http.StatusOK, s)
	var overview []struct {
		ServiceType string `json:"service_type"`
		Current     *int   `json:"current_rate_bps"`
		Scheduled   []struct {
			RateBps int `json:"rate_bps"`
		} `json:"scheduled"`
	}
	require.NoError(t, json.Unmarshal(body, &overview))
	require.Len(t, overview, 4)
	require.Equal(t, "attraction_ticket", overview[0].ServiceType)
	require.NotNil(t, overview[0].Current)
	require.Equal(t, 1000, *overview[0].Current)
	require.Len(t, overview[0].Scheduled, 1)
	require.Equal(t, 1500, overview[0].Scheduled[0].RateBps)
	require.Nil(t, overview[1].Current)
	require.Contains(t, string(body), `"current_rate_bps":null`)
	require.Contains(t, string(body), `"scheduled":[]`)

	s, body = do(t, srv, "GET", "/v1/admin/commission-rates/attraction_ticket/history", adminTok, "")
	require.Equal(t, http.StatusOK, s)
	var hist []catalog.CommissionRate
	require.NoError(t, json.Unmarshal(body, &hist))
	require.Len(t, hist, 2)
	require.Equal(t, 1500, hist[0].RateBps)

	// Unknown code in the path → 404 not_found (Ruling 11).
	s, body = do(t, srv, "GET", "/v1/admin/commission-rates/nope/history", adminTok, "")
	require.Equal(t, http.StatusNotFound, s)
	require.Contains(t, string(body), `"code":"not_found"`)
}
