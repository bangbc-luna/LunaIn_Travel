package identity

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

func newHTTPServer(t *testing.T, env *testEnv) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	env.Svc.Routes(mux, env.Issuer, httpx.ClientIPFunc(0))
	mux.HandleFunc("/", httpx.NotFound) // as in cmd/api
	srv := httptest.NewServer(httpx.Chain(mux, httpx.RequestID, httpx.Recover))
	t.Cleanup(srv.Close)
	return srv
}

func doJSON(t *testing.T, srv *httptest.Server, method, path, bearer string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		require.NoError(t, err)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestHTTP_SignupFlow(t *testing.T) {
	env := newTestEnv(t)
	srv := newHTTPServer(t, env)

	st, _ := doJSON(t, srv, "POST", "/v1/auth/register", "", map[string]string{"email": "u@example.com", "password": "Passw0rd123"})
	require.Equal(t, http.StatusAccepted, st)

	code := lastCode(t, env)
	st, body := doJSON(t, srv, "POST", "/v1/auth/verify-email", "", map[string]string{"email": "u@example.com", "code": code, "password": "Passw0rd123"})
	require.Equal(t, http.StatusOK, st)
	access, _ := body["access_token"].(string)
	require.NotEmpty(t, access)

	st, me := doJSON(t, srv, "GET", "/v1/me", access, nil)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "customer", me["role"])
	require.Equal(t, "u@example.com", me["email"])
	require.NotEmpty(t, me["id"])

	st, prof := doJSON(t, srv, "PATCH", "/v1/customer/profile", access, map[string]string{"full_name": "Nguyen A"})
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "Nguyen A", prof["full_name"])
}

func TestHTTP_ProfileRequiresCustomer(t *testing.T) {
	env := newTestEnv(t)
	srv := newHTTPServer(t, env)

	tok, _, err := env.Issuer.Issue(auth.Principal{UserID: uuid.New(), Role: auth.RolePartner})
	require.NoError(t, err)
	st, body := doJSON(t, srv, "GET", "/v1/customer/profile", tok, nil)
	require.Equal(t, http.StatusForbidden, st)
	require.Equal(t, "forbidden", errCode(body))

	st, _ = doJSON(t, srv, "GET", "/v1/customer/profile", "", nil)
	require.Equal(t, http.StatusUnauthorized, st)
}

func TestHTTP_UnknownFieldRejected(t *testing.T) {
	env := newTestEnv(t)
	srv := newHTTPServer(t, env)
	st, body := doJSON(t, srv, "POST", "/v1/auth/login", "", `{"email":"a@b.com","password":"x","admin":true}`)
	require.Equal(t, http.StatusBadRequest, st)
	require.Equal(t, "invalid_request", errCode(body))
}

func TestHTTP_ForgotUnknownEmail202(t *testing.T) {
	env := newTestEnv(t)
	srv := newHTTPServer(t, env)
	st, _ := doJSON(t, srv, "POST", "/v1/auth/password/forgot", "", map[string]string{"email": "nobody@example.com"})
	require.Equal(t, http.StatusAccepted, st)
}

func TestHTTP_LoginRateLimited(t *testing.T) {
	env := newTestEnv(t)
	srv := newHTTPServer(t, env)
	// Malformed bodies still pass through the limiter (outermost) but never
	// reach argon2, so the burst finishes well inside one refill interval.
	post := func() *http.Response {
		resp, err := http.Post(srv.URL+"/v1/auth/login", "application/json", strings.NewReader("{"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	for i := 1; i <= 20; i++ {
		require.Equal(t, http.StatusBadRequest, post().StatusCode, "request %d", i)
	}
	resp := post()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.NotEmpty(t, resp.Header.Get("Retry-After"))
}

func TestHTTP_UnknownRouteJSON404(t *testing.T) {
	env := newTestEnv(t)
	srv := newHTTPServer(t, env)

	for _, path := range []string{
		"/v1/nope",
		"/v1/auth/login", // known path, wrong method (GET)
	} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		_ = resp.Body.Close()
		require.Equal(t, http.StatusNotFound, resp.StatusCode, path)
		require.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json"), path)
		require.Equal(t, "not_found", errCode(out), path)
	}
}
