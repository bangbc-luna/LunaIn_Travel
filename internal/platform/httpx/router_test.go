package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecordingRouter(t *testing.T) {
	mux := http.NewServeMux()
	var r Router = NewRecordingRouter(mux)
	r.Handle("POST /b", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) }))
	r.HandleFunc("GET /a/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/b", nil))
	require.Equal(t, http.StatusCreated, rec.Code)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/a/1", nil))
	require.Equal(t, http.StatusAccepted, rec.Code)

	got := r.(*RecordingRouter).Patterns()
	require.Equal(t, []string{"GET /a/{id}", "POST /b"}, got)
	got[0] = "mutated"
	require.Equal(t, "GET /a/{id}", r.(*RecordingRouter).Patterns()[0])
}
