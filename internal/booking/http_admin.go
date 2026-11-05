package booking

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// AdminRoutes registers the Super Admin booking API
// (Authenticate -> RequireRole(super_admin)).
func (s *Service) AdminRoutes(mux httpx.Router, iss *auth.Issuer) {
	admin := func(h http.HandlerFunc) http.Handler {
		return httpx.Chain(h, auth.Authenticate(iss), auth.RequireRole(auth.RoleSuperAdmin))
	}

	mux.Handle("GET /v1/admin/bookings", admin(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		lf, err := parseListFilter(q)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		f := AdminFilter{ListFilter: lf}
		for key, dst := range map[string]**uuid.UUID{"partner_id": &f.PartnerID, "customer_id": &f.CustomerID} {
			if v := q.Get(key); v != "" {
				id, err := uuid.Parse(v)
				if err != nil {
					httpx.WriteError(w, r, httpx.ErrInvalidRequest)
					return
				}
				*dst = &id
			}
		}
		if v := q.Get("code"); v != "" {
			f.Code = &v
		}
		items, next, err := s.AdminList(r.Context(), f)
		writeList(w, r, items, next, err)
	}))

	mux.Handle("GET /v1/admin/bookings/{id}", admin(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		v, err := s.AdminGet(r.Context(), id)
		writeView(w, r, http.StatusOK, v, err)
	}))
}
