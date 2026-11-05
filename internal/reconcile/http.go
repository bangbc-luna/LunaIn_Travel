package reconcile

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// manualRunTimeout bounds a manual run, which goes on if the client leaves
// (a half-done run would only be redone).
const manualRunTimeout = 10 * time.Minute

// AdminRoutes registers the Super Admin reconciliation API
// (Authenticate -> RequireRole(super_admin)):
//   - GET /v1/admin/reconciliations?limit=&before=YYYY-MM-DD, newest first;
//   - GET /v1/admin/reconciliations/{run_date}: a malformed or unknown date
//     is 404;
//   - POST /v1/admin/reconciliations/run (optional empty body): runs now,
//     synchronously, and answers the result.
func (s *Service) AdminRoutes(mux httpx.Router, iss *auth.Issuer) {
	admin := func(h http.HandlerFunc) http.Handler {
		return httpx.Chain(h, auth.Authenticate(iss), auth.RequireRole(auth.RoleSuperAdmin))
	}

	mux.Handle("GET /v1/admin/reconciliations", admin(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := 0
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				httpx.WriteError(w, r, httpx.ErrInvalidRequest)
				return
			}
			limit = n
		}
		var before *time.Time
		if v := q.Get("before"); v != "" {
			d, err := time.Parse(time.DateOnly, v)
			if err != nil {
				httpx.WriteError(w, r, httpx.ErrInvalidRequest)
				return
			}
			before = &d
		}
		items, err := s.d.Payments.ListReconciliations(r.Context(), limit, before)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, struct {
			Items []payment.Reconciliation `json:"items"`
		}{items})
	}))

	mux.Handle("GET /v1/admin/reconciliations/{run_date}", admin(func(w http.ResponseWriter, r *http.Request) {
		d, err := time.Parse(time.DateOnly, r.PathValue("run_date"))
		if err != nil {
			httpx.WriteError(w, r, payment.ErrReconciliationNotFound)
			return
		}
		rec, err := s.d.Payments.GetReconciliation(r.Context(), d)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, rec)
	}))

	mux.Handle("POST /v1/admin/reconciliations/run", admin(func(w http.ResponseWriter, r *http.Request) {
		var in struct{}
		if _, err := httpx.DecodeOptionalJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), manualRunTimeout)
		defer cancel()
		rec, err := s.Run(ctx, &p.UserID)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, rec)
	}))
}
