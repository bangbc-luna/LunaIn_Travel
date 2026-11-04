package files

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	// multipart overhead allowed on top of the file itself.
	bodyOverhead = 1 << 20
	maxFormField = 1 << 10

	uploadReadTimeout = 2 * time.Minute
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
	// The server-wide timeouts (15s read) are too short for a 10 MiB upload on
	// a slow link: extend them for this route only. Socket deadlines are not
	// business time, so time.Now() is used instead of the injected clock.
	rc := http.NewResponseController(w)
	now := time.Now()
	_ = rc.SetReadDeadline(now.Add(uploadReadTimeout))
	_ = rc.SetWriteDeadline(now.Add(uploadReadTimeout + 10*time.Second))

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
			httpx.WriteError(w, r, streamError(err))
			return
		}
		switch part.FormName() {
		case "purpose":
			b, err := io.ReadAll(io.LimitReader(part, maxFormField))
			if err != nil {
				httpx.WriteError(w, r, streamError(err))
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
				httpx.WriteError(w, r, uploadError(err))
				return
			}
			httpx.WriteJSON(w, http.StatusCreated, f)
			return
		}
	}
}

// streamError maps a failure of reading/parsing the multipart stream itself:
// a body cut off by MaxBytesReader is invalid_file (422); anything else
// (truncated body, bad part header, client disconnect) is a 400.
func streamError(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return invalidFile("Tệp vượt quá 10 MiB")
	}
	return httpx.ErrInvalidRequest
}

// uploadError maps an error from Upload: AppErrors pass through, a failure to
// read the file part is handled like a stream error, and everything else
// (storage, database) stays a 500.
func uploadError(err error) error {
	var ae *httpx.AppError
	if errors.As(err, &ae) {
		return err
	}
	if errors.Is(err, errReadBody) {
		return streamError(err)
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
