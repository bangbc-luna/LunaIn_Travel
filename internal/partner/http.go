package partner

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// Routes registers the partner self-service API. Every route resolves the
// partner from the session (RequireMember); none accepts a partner_id.
func (s *Service) Routes(mux httpx.Router, iss *auth.Issuer) {
	member := func(h func(w http.ResponseWriter, r *http.Request, partnerID, userID uuid.UUID)) http.Handler {
		return httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			partnerID, ok := IDFrom(r.Context())
			pr, okP := auth.PrincipalFrom(r.Context())
			if !ok || !okP {
				httpx.WriteError(w, r, httpx.ErrForbidden)
				return
			}
			h(w, r, partnerID, pr.UserID)
		}), auth.Authenticate(iss), auth.RequireRole(auth.RolePartner), s.RequireMember())
	}

	mux.Handle("GET /v1/partner/profile", member(func(w http.ResponseWriter, r *http.Request, partnerID, _ uuid.UUID) {
		p, err := s.Get(r.Context(), partnerID)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, p)
	}))

	mux.Handle("PATCH /v1/partner/profile", member(func(w http.ResponseWriter, r *http.Request, partnerID, userID uuid.UUID) {
		var u ProfileUpdate
		if err := httpx.DecodeJSON(r, &u); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, err := s.UpdateProfile(r.Context(), partnerID, userID, u)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, p)
	}))

	mux.Handle("GET /v1/partner/bank-accounts", member(func(w http.ResponseWriter, r *http.Request, partnerID, _ uuid.UUID) {
		list, err := s.BankAccounts(r.Context(), partnerID)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, list)
	}))

	mux.Handle("PUT /v1/partner/bank-account", member(func(w http.ResponseWriter, r *http.Request, partnerID, userID uuid.UUID) {
		var in BankAccountInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		ba, err := s.SetBankAccount(r.Context(), partnerID, userID, in)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, ba)
	}))
}

// pathCodeNotFound maps an unknown service-type code taken from the URL path
// to 404 not_found (Ruling 11): a path identifier that cannot name an
// existing resource is "not found", not a 422 field error.
func pathCodeNotFound(err error) error {
	if errors.Is(err, catalog.ErrInvalidServiceType) {
		return httpx.ErrNotFound
	}
	return err
}

// AdminRoutes registers the Super Admin partner API. Every route is
// Authenticate -> RequireRole(super_admin). A path id that is not a uuid cannot
// name a partner, so it is answered 404 not_found.
func (s *Service) AdminRoutes(mux httpx.Router, iss *auth.Issuer) {
	admin := func(h func(w http.ResponseWriter, r *http.Request, actor uuid.UUID)) http.Handler {
		return httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pr, _ := auth.PrincipalFrom(r.Context())
			h(w, r, pr.UserID)
		}), auth.Authenticate(iss), auth.RequireRole(auth.RoleSuperAdmin))
	}
	// withID parses {id} before calling h.
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
	noContent := func(w http.ResponseWriter, r *http.Request, err error) {
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}

	mux.Handle("POST /v1/admin/partners", admin(func(w http.ResponseWriter, r *http.Request, actor uuid.UUID) {
		var in CreateInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, err := s.Create(r.Context(), actor, in)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, p)
	}))

	mux.Handle("GET /v1/admin/partners", admin(func(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
		q := r.URL.Query()
		f := ListFilter{Query: q.Get("q")}
		if v := q.Get("status"); v != "" {
			f.Status = &v
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
		items, next, err := s.List(r.Context(), f)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if items == nil {
			items = []Partner{}
		}
		httpx.WriteJSON(w, http.StatusOK, struct {
			Items      []Partner  `json:"items"`
			NextCursor *uuid.UUID `json:"next_cursor"`
		}{items, next})
	}))

	mux.Handle("GET /v1/admin/partners/{id}", withID(func(w http.ResponseWriter, r *http.Request, _, id uuid.UUID) {
		p, err := s.Get(r.Context(), id)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, p)
	}))

	mux.Handle("PUT /v1/admin/partners/{id}/service-types/{code}", withID(func(w http.ResponseWriter, r *http.Request, actor, id uuid.UUID) {
		noContent(w, r, pathCodeNotFound(s.EnableServiceType(r.Context(), actor, id, r.PathValue("code"))))
	}))

	mux.Handle("DELETE /v1/admin/partners/{id}/service-types/{code}", withID(func(w http.ResponseWriter, r *http.Request, actor, id uuid.UUID) {
		noContent(w, r, pathCodeNotFound(s.DisableServiceType(r.Context(), actor, id, r.PathValue("code"))))
	}))

	mux.Handle("POST /v1/admin/partners/{id}/suspend", withID(func(w http.ResponseWriter, r *http.Request, actor, id uuid.UUID) {
		var req struct {
			Reason string `json:"reason"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		noContent(w, r, s.Suspend(r.Context(), actor, id, req.Reason))
	}))

	mux.Handle("POST /v1/admin/partners/{id}/unsuspend", withID(func(w http.ResponseWriter, r *http.Request, actor, id uuid.UUID) {
		noContent(w, r, s.Unsuspend(r.Context(), actor, id))
	}))
}
