package listing

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// AdminRoutes registers the Super Admin listing moderation API. Every route is
// Authenticate -> RequireRole(super_admin). A path id that is not a uuid, or
// names no listing, is answered 404 not_found.
func (s *Service) AdminRoutes(mux httpx.Router, iss *auth.Issuer) {
	admin := func(h func(w http.ResponseWriter, r *http.Request, actor uuid.UUID)) http.Handler {
		return httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pr, _ := auth.PrincipalFrom(r.Context())
			h(w, r, pr.UserID)
		}), auth.Authenticate(iss), auth.RequireRole(auth.RoleSuperAdmin))
	}
	withID := func(h func(w http.ResponseWriter, r *http.Request, actor, id uuid.UUID)) http.Handler {
		return admin(func(w http.ResponseWriter, r *http.Request, actor uuid.UUID) {
			id, err := uuid.Parse(r.PathValue("id"))
			if err != nil {
				httpx.WriteError(w, r, httpx.ErrNotFound)
				return
			}
			h(w, r, actor, id)
		})
	}
	reply := func(w http.ResponseWriter, r *http.Request, l Listing, err error) {
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, l)
	}
	withReason := func(f func(r *http.Request, actor, id uuid.UUID, reason string) (Listing, error)) http.Handler {
		return withID(func(w http.ResponseWriter, r *http.Request, actor, id uuid.UUID) {
			var in struct {
				Reason string `json:"reason"`
			}
			if err := httpx.DecodeJSON(r, &in); err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			l, err := f(r, actor, id, in.Reason)
			reply(w, r, l, err)
		})
	}

	mux.Handle("GET /v1/admin/listings", admin(func(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
		q := r.URL.Query()
		var f AdminFilter
		for key, dst := range map[string]**string{
			"review_status": &f.ReviewStatus, "visibility": &f.Visibility, "service_type": &f.ServiceType,
		} {
			if v := q.Get(key); v != "" {
				*dst = &v
			}
		}
		if v := q.Get("partner_id"); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				httpx.WriteError(w, r, httpx.ErrInvalidRequest)
				return
			}
			f.PartnerID = &id
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
		items, next, err := s.AdminList(r.Context(), f)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, struct {
			Items      []Listing  `json:"items"`
			NextCursor *uuid.UUID `json:"next_cursor"`
		}{items, next})
	}))

	mux.Handle("GET /v1/admin/listings/{id}", withID(func(w http.ResponseWriter, r *http.Request, _, id uuid.UUID) {
		d, err := s.AdminGet(r.Context(), id)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, d)
	}))

	mux.Handle("GET /v1/admin/listings/{id}/ride-trips", withID(func(w http.ResponseWriter, r *http.Request, _, id uuid.UUID) {
		from, to, err := tripRange(r, s.clock.Now())
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		trips, err := s.AdminListTrips(r.Context(), id, from, to)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, trips)
	}))

	mux.Handle("POST /v1/admin/listings/{id}/approve", withID(func(w http.ResponseWriter, r *http.Request, actor, id uuid.UUID) {
		l, err := s.Approve(r.Context(), actor, id)
		reply(w, r, l, err)
	}))
	mux.Handle("POST /v1/admin/listings/{id}/unhide", withID(func(w http.ResponseWriter, r *http.Request, actor, id uuid.UUID) {
		l, err := s.Unhide(r.Context(), actor, id)
		reply(w, r, l, err)
	}))
	mux.Handle("POST /v1/admin/listings/{id}/reject", withReason(func(r *http.Request, actor, id uuid.UUID, reason string) (Listing, error) {
		return s.Reject(r.Context(), actor, id, reason)
	}))
	mux.Handle("POST /v1/admin/listings/{id}/hide", withReason(func(r *http.Request, actor, id uuid.UUID, reason string) (Listing, error) {
		return s.Hide(r.Context(), actor, id, reason)
	}))
}
