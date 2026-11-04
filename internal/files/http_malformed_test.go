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

	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

func TestHTTP_UploadMalformedAndOversize(t *testing.T) {
	e := newEnv(t)
	clk := &clock.Fixed{T: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	iss := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk)
	partner := e.user(t, auth.RolePartner)
	tok, _, err := iss.Issue(partner)
	require.NoError(t, err)

	mux := http.NewServeMux()
	e.Svc.Routes(mux, iss, httpx.ClientIPFunc(0))
	mux.HandleFunc("/", httpx.NotFound)
	srv := httptest.NewServer(httpx.Chain(mux, httpx.RequestID, httpx.Recover))
	t.Cleanup(srv.Close)

	send := func(body []byte, contentType string) (int, string) {
		req, err := http.NewRequest("POST", srv.URL+"/v1/files", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		var out struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out.Error.Code
	}
	multipartBody := func(fileSize int) ([]byte, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		require.NoError(t, mw.WriteField("purpose", "listing_image"))
		fw, err := mw.CreateFormFile("file", "x.png")
		require.NoError(t, err)
		_, err = fw.Write(pngBytes(t))
		require.NoError(t, err)
		_, err = fw.Write(make([]byte, fileSize))
		require.NoError(t, err)
		require.NoError(t, mw.Close())
		return buf.Bytes(), mw.FormDataContentType()
	}

	// truncated: closing boundary missing and the file cut short
	full, ct := multipartBody(2000)
	status, code := send(full[:len(full)-1000], ct)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "invalid_request", code)

	// garbage that is not valid multipart
	status, code = send([]byte("--nope\r\nbroken header\r\n"), "multipart/form-data; boundary=nope")
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "invalid_request", code)

	// file just over 10 MiB (inside the body cap): rejected by Upload
	over, ct := multipartBody(files.MaxSize + 1)
	status, code = send(over, ct)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_file", code)

	// body beyond 10 MiB + overhead: cut off by MaxBytesReader, same answer
	huge, ct := multipartBody(files.MaxSize + 1<<20 + 100<<10)
	status, code = send(huge, ct)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_file", code)
	require.Zero(t, e.Mem.Len())
}
