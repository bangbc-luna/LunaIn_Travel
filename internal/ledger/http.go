package ledger

import (
	"net/http"

	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// PartnerRoutes registers the partner balance API. The chain is
// Authenticate -> RequireRole(partner) -> requireMember (partner.Service's
// RequireMember); the partner_id comes from partner.IDFrom, never the client.
func (s *Service) PartnerRoutes(mux httpx.Router, iss *auth.Issuer, requireMember func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/partner/balance", httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		partnerID, ok := partner.IDFrom(r.Context())
		if !ok {
			httpx.WriteError(w, r, httpx.ErrForbidden)
			return
		}
		b, err := s.Balance(r.Context(), partnerID)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, b)
	}), auth.Authenticate(iss), auth.RequireRole(auth.RolePartner), requireMember))
}
