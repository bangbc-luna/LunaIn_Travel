package files

import (
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	// multipart overhead allowed on top of the file itself.
	bodyOverhead = 1 << 20
	maxFormField = 1 << 10
)

// Routes registers the file API on mux.
func (s *Service) Routes(mux *http.ServeMux, iss *auth.Issuer, clientIP func(*http.Request) string) {
	authed := func(h http.HandlerFunc, mws ...func(http.Handler) http.Handler) http.Handler {
		return httpx.Chain(h, append([]func(http.Handler) http.Handler{
			auth.Authenticate(iss),
			auth.RequireRole(auth.RoleCustomer, auth.RolePartner, auth.RoleSuperAdmin),
		}, mws...)...)
	}

	mux.Handle("POST /v1/files", authed(s.handleUpload, httpx.RateLimit(30, 30, clientIP)))
	mux.Handle("GET /v1/files/{id}/url", authed(s.handleURL))
}

func (s *Service) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxSize+bodyOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrInvalidRequest)
		return
	}
	pr, _ := auth.PrincipalFrom(r.Context())

	// The purpose field must precede the file part so the file can be
	// streamed straight into Upload without buffering twice.
	var purpose string
	for {
		part, err := mr.NextPart()
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrInvalidRequest)
			return
		}
		switch part.FormName() {
		case "purpose":
			b, err := io.ReadAll(io.LimitReader(part, maxFormField))
			if err != nil {
				httpx.WriteError(w, r, uploadReadError(err))
				return
			}
			purpose = string(b)
		case "file":
			if purpose == "" {
				httpx.WriteError(w, r, httpx.ErrInvalidRequest)
				return
			}
			f, err := s.Upload(r.Context(), pr, purpose, part)
			if err != nil {
				httpx.WriteError(w, r, uploadReadError(err))
				return
			}
			httpx.WriteJSON(w, http.StatusCreated, f)
			return
		}
	}
}

// uploadReadError maps a body cut off by MaxBytesReader to invalid_file.
func uploadReadError(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return invalidFile("Tệp vượt quá 10 MiB")
	}
	return err
}

func (s *Service) handleURL(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrInvalidRequest)
		return
	}
	pr, _ := auth.PrincipalFrom(r.Context())
	u, err := s.URL(r.Context(), pr, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"url": u})
}
