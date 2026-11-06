package listing

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// charterRoutes registers the private charter routes: service, vehicle classes
// and surcharges. Each one answers ErrNotCharterListing for a listing of
// another service type.
func (s *Service) charterRoutes(mux httpx.Router, withID func(h func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID)) http.Handler) {
	reply := func(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, status, v)
	}

	mux.Handle("GET /v1/partner/listings/{id}/charter", withID(func(w http.ResponseWriter, r *http.Request, partnerID, _, id uuid.UUID) {
		d, err := s.GetCharter(r.Context(), partnerID, id)
		reply(w, r, http.StatusOK, d, err)
	}))

	mux.Handle("PUT /v1/partner/listings/{id}/charter-service", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in CharterServiceInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		st, err := s.SetCharterService(r.Context(), partnerID, userID, id, in)
		reply(w, r, http.StatusOK, st, err)
	}))

	mux.Handle("POST /v1/partner/listings/{id}/charter-classes", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in CharterClassInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		c, err := s.CreateCharterClass(r.Context(), partnerID, userID, id, in)
		reply(w, r, http.StatusCreated, c, err)
	}))

	mux.Handle("PATCH /v1/partner/listings/{id}/charter-classes/{classID}", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		classID, err := uuid.Parse(r.PathValue("classID"))
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrNotFound)
			return
		}
		var in CharterClassPatch
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		c, err := s.UpdateCharterClass(r.Context(), partnerID, userID, id, classID, in)
		reply(w, r, http.StatusOK, c, err)
	}))

	mux.Handle("PUT /v1/partner/listings/{id}/charter-classes/{classID}/surcharges", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		classID, err := uuid.Parse(r.PathValue("classID"))
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrNotFound)
			return
		}
		var in struct {
			Surcharges []SurchargeInput `json:"surcharges"`
		}
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if in.Surcharges == nil {
			httpx.WriteError(w, r, invalidCharter("Cần gửi danh sách phụ phí (surcharges)"))
			return
		}
		c, err := s.SetCharterSurcharges(r.Context(), partnerID, userID, id, classID, in.Surcharges)
		reply(w, r, http.StatusOK, c, err)
	}))
}

// Public quotes call the paid map API, so they are rate limited per client IP.
const charterQuotesPerMinute = 20

// CharterQuoteRoutes registers the public (unauthenticated) charter quote:
// POST /v1/charter-quotes, limited to 20 requests a minute per client IP
// (clientIP, see httpx.ClientIPFunc). A quote depends on the time and the
// route, so it is never cached.
func (s *Service) CharterQuoteRoutes(mux httpx.Router, clientIP func(*http.Request) string) {
	quote := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var in CharterQuoteInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		res, err := s.QuoteCharter(r.Context(), in)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, res)
	})
	mux.Handle("POST /v1/charter-quotes", httpx.RateLimit(charterQuotesPerMinute, charterQuotesPerMinute, clientIP)(quote))
}
