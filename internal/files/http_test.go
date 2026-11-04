package files_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

func TestHTTP_UploadMultipart(t *testing.T) {
	e := newEnv(t)
	clk := &clock.Fixed{T: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	iss := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk)
	partner := e.user(t, auth.RolePartner)
	other := e.user(t, auth.RolePartner)
	tok := func(p auth.Principal) string {
		s, _, err := iss.Issue(p)
		require.NoError(t, err)
		return s
	}

	mux := http.NewServeMux()
	e.Svc.Routes(mux, iss, httpx.ClientIPFunc(0))
	mux.HandleFunc("/", httpx.NotFound)
	srv := httptest.NewServer(httpx.Chain(mux, httpx.RequestID, httpx.Recover))
	t.Cleanup(srv.Close)

	post := func(token, purpose string, content []byte) *http.Response {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		require.NoError(t, mw.WriteField("purpose", purpose))
		fw, err := mw.CreateFormFile("file", "x.jpg")
		require.NoError(t, err)
		_, err = fw.Write(content)
		require.NoError(t, err)
		require.NoError(t, mw.Close())
		req, err := http.NewRequest("POST", srv.URL+"/v1/files", &buf)
		require.NoError(t, err)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}

	require.Equal(t, http.StatusUnauthorized, post("", "listing_image", pngBytes(t)).StatusCode)

	res := post(tok(partner), "listing_image", pngBytes(t))
	require.Equal(t, http.StatusCreated, res.StatusCode)
	var f struct {
		ID         string `json:"id"`
		Visibility string `json:"visibility"`
		URL        string `json:"url"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&f))
	require.NotEmpty(t, f.ID)
	require.Equal(t, "public", f.Visibility)
	require.NotEmpty(t, f.URL)

	// disguised executable named .jpg: rejected by content
	res = post(tok(partner), "listing_image", []byte("MZ\x90\x00\x03\x00\x00\x00"))
	require.Equal(t, http.StatusUnprocessableEntity, res.StatusCode)

	// private file: owner gets a URL, other partner gets 404
	res = post(tok(partner), "business_license", pngBytes(t))
	require.Equal(t, http.StatusCreated, res.StatusCode)
	var pf struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&pf))
	require.Empty(t, pf.URL)

	get := func(token, id string) *http.Response {
		req, err := http.NewRequest("GET", srv.URL+"/v1/files/"+id+"/url", nil)
		require.NoError(t, err)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		r, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Body.Close() })
		return r
	}
	require.Equal(t, http.StatusUnauthorized, get("", pf.ID).StatusCode)
	require.Equal(t, http.StatusNotFound, get(tok(other), pf.ID).StatusCode)
	require.Equal(t, http.StatusBadRequest, get(tok(partner), "not-a-uuid").StatusCode)
	r := get(tok(partner), pf.ID)
	require.Equal(t, http.StatusOK, r.StatusCode)
	var u struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.NewDecoder(r.Body).Decode(&u))
	require.Contains(t, u.URL, "memory://private/business_license/")
}
