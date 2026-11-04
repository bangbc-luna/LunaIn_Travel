package httpx_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

type body struct {
	A int `json:"a"`
}

func decode(s string) error {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(s))
	var b body
	return httpx.DecodeJSON(r, &b)
}

func TestDecodeJSON_UnknownField(t *testing.T) {
	require.True(t, errors.Is(decode(`{"a":1,"zzz":2}`), httpx.ErrInvalidRequest))
}

func TestDecodeJSON_TooLarge(t *testing.T) {
	big := `{"a":1,"pad":"` + strings.Repeat("x", 2<<20) + `"}`
	require.True(t, errors.Is(decode(big), httpx.ErrInvalidRequest))
}

func TestDecodeJSON_Malformed(t *testing.T) {
	require.True(t, errors.Is(decode(`{"a":`), httpx.ErrInvalidRequest))
}

func TestDecodeJSON_TrailingValueAndOK(t *testing.T) {
	require.True(t, errors.Is(decode(`{"a":1}{"a":2}`), httpx.ErrInvalidRequest))
	require.NoError(t, decode(`{"a":1}`))
}

func TestWriteError_AppError(t *testing.T) {
	w := httptest.NewRecorder()
	httpx.WriteError(w, httptest.NewRequest(http.MethodGet, "/", nil), httpx.ErrForbidden)
	require.Equal(t, 403, w.Code)
	require.JSONEq(t, `{"error":{"code":"forbidden","message":"Bạn không có quyền thực hiện thao tác này"}}`, w.Body.String())
}

func TestWriteError_UnknownErrorIs500(t *testing.T) {
	w := httptest.NewRecorder()
	httpx.WriteError(w, httptest.NewRequest(http.MethodGet, "/", nil), errors.New("secret db detail"))
	require.Equal(t, 500, w.Code)
	require.Contains(t, w.Body.String(), `"internal_error"`)
	require.NotContains(t, w.Body.String(), "secret")
}

func TestRecover(t *testing.T) {
	h := httpx.Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, 500, w.Code)
	require.Contains(t, w.Body.String(), `"internal_error"`)
}

func TestRequestID(t *testing.T) {
	var got string
	h := httpx.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = httpx.RequestIDFrom(r.Context()) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	require.NotEmpty(t, got)
	require.Equal(t, got, w.Header().Get("X-Request-ID"))
}

func TestRateLimit(t *testing.T) {
	h := httpx.RateLimit(2, 2, func(*http.Request) string { return "k" })(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	var codes []int
	var last *httptest.ResponseRecorder
	for range 3 {
		last = httptest.NewRecorder()
		h.ServeHTTP(last, httptest.NewRequest(http.MethodGet, "/", nil))
		codes = append(codes, last.Code)
	}
	require.Equal(t, []int{200, 200, 429}, codes)
	require.NotEmpty(t, last.Header().Get("Retry-After"))
}

func xffReq(lines ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "1.2.3.4:5555"
	for _, l := range lines {
		r.Header.Add("X-Forwarded-For", l)
	}
	return r
}

func TestClientIPFunc(t *testing.T) {
	t.Run("hops 0 ignores spoofed XFF", func(t *testing.T) {
		require.Equal(t, "1.2.3.4", httpx.ClientIPFunc(0)(xffReq("9.9.9.9")))
	})
	t.Run("hops 1 takes last", func(t *testing.T) {
		require.Equal(t, "2.2.2.2", httpx.ClientIPFunc(1)(xffReq("1.1.1.1, 2.2.2.2")))
	})
	t.Run("hops 2 takes second from right", func(t *testing.T) {
		require.Equal(t, "1.1.1.1", httpx.ClientIPFunc(2)(xffReq("1.1.1.1, 2.2.2.2")))
	})
	t.Run("too few entries falls back to RemoteAddr", func(t *testing.T) {
		require.Equal(t, "1.2.3.4", httpx.ClientIPFunc(3)(xffReq("1.1.1.1, 2.2.2.2")))
	})
	t.Run("multiple header lines combined in order", func(t *testing.T) {
		require.Equal(t, "1.1.1.1", httpx.ClientIPFunc(2)(xffReq("1.1.1.1", "2.2.2.2")))
		require.Equal(t, "2.2.2.2", httpx.ClientIPFunc(1)(xffReq("1.1.1.1", "2.2.2.2")))
	})
	t.Run("RemoteAddr without port", func(t *testing.T) {
		r := xffReq()
		r.RemoteAddr = "5.6.7.8"
		require.Equal(t, "5.6.7.8", httpx.ClientIPFunc(0)(r))
	})
}
