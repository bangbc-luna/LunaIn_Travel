package partner

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// Routes registers the partner self-service API. Every route resolves the
// partner from the session (RequireMember); none accepts a partner_id.
func (s *Service) Routes(mux *http.ServeMux, iss *auth.Issuer) {
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
