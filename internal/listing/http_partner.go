package listing

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// PartnerRoutes registers the partner listing API. Every route is
// Authenticate -> RequireRole(partner) -> requireMember (partner.Service's
// RequireMember); the partner_id always comes from partner.IDFrom, never from
// the client. A path id that is not a uuid, or names another partner's
// listing, is answered 404 not_found.
func (s *Service) PartnerRoutes(mux httpx.Router, iss *auth.Issuer, requireMember func(http.Handler) http.Handler) {
	member := func(h func(w http.ResponseWriter, r *http.Request, partnerID, userID uuid.UUID)) http.Handler {
		return httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			partnerID, ok := partner.IDFrom(r.Context())
			pr, okP := auth.PrincipalFrom(r.Context())
			if !ok || !okP {
				httpx.WriteError(w, r, httpx.ErrForbidden)
				return
			}
			h(w, r, partnerID, pr.UserID)
		}), auth.Authenticate(iss), auth.RequireRole(auth.RolePartner), requireMember)
	}
	withID := func(h func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID)) http.Handler {
		return member(func(w http.ResponseWriter, r *http.Request, partnerID, userID uuid.UUID) {
			id, err := uuid.Parse(r.PathValue("id"))
			if err != nil {
				httpx.WriteError(w, r, httpx.ErrNotFound)
				return
			}
			h(w, r, partnerID, userID, id)
		})
	}
	reply := func(w http.ResponseWriter, r *http.Request, status int, l Listing, err error) {
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, status, l)
	}

	mux.Handle("POST /v1/partner/listings", member(func(w http.ResponseWriter, r *http.Request, partnerID, userID uuid.UUID) {
		var in CreateInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		l, err := s.Create(r.Context(), partnerID, userID, in)
		reply(w, r, http.StatusCreated, l, err)
	}))

	mux.Handle("GET /v1/partner/listings", member(func(w http.ResponseWriter, r *http.Request, partnerID, _ uuid.UUID) {
		q := r.URL.Query()
		var f OwnFilter
		if v := q.Get("review_status"); v != "" {
			f.ReviewStatus = &v
		}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				httpx.WriteError(w, r, httpx.ErrInvalidRequest)
				return
			}
			f.Limit = n
		}
		if v := q.Get("cursor"); v != "" {
			c, err := uuid.Parse(v)
			if err != nil {
				httpx.WriteError(w, r, httpx.ErrInvalidRequest)
				return
			}
			f.Cursor = &c
		}
		items, next, err := s.ListOwn(r.Context(), partnerID, f)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, struct {
			Items      []Listing  `json:"items"`
			NextCursor *uuid.UUID `json:"next_cursor"`
		}{items, next})
	}))

	mux.Handle("GET /v1/partner/listings/{id}", withID(func(w http.ResponseWriter, r *http.Request, partnerID, _, id uuid.UUID) {
		l, err := s.Get(r.Context(), partnerID, id)
		reply(w, r, http.StatusOK, l, err)
	}))

	mux.Handle("PATCH /v1/partner/listings/{id}", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in UpdateInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		l, err := s.Update(r.Context(), partnerID, userID, id, in)
		reply(w, r, http.StatusOK, l, err)
	}))
}
