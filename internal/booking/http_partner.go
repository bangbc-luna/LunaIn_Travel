package booking

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// PartnerRoutes registers the partner booking API. The chain is
// Authenticate -> RequireRole(partner) -> requireMember; the partner_id comes
// from partner.IDFrom, never the client. Another partner's booking is 404.
func (s *Service) PartnerRoutes(mux httpx.Router, iss *auth.Issuer, requireMember func(http.Handler) http.Handler) {
	member := func(h func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID)) http.Handler {
		return httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			partnerID, ok := partner.IDFrom(r.Context())
			if !ok {
				httpx.WriteError(w, r, httpx.ErrForbidden)
				return
			}
			h(w, r, partnerID)
		}), auth.Authenticate(iss), auth.RequireRole(auth.RolePartner), requireMember)
	}

	mux.Handle("GET /v1/partner/bookings", member(func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID) {
		f, err := parseListFilter(r.URL.Query())
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		items, next, err := s.ListForPartner(r.Context(), partnerID, f)
		writeList(w, r, items, next, err)
	}))

	mux.Handle("GET /v1/partner/bookings/{id}", member(func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		v, err := s.GetForPartner(r.Context(), partnerID, id)
		writeView(w, r, http.StatusOK, v, err)
	}))

	mux.Handle("POST /v1/partner/check-ins", member(func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID) {
		var in checkInInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		var res CheckInResult
		var err error
		switch {
		case in.QRToken != nil && in.Code == nil && *in.QRToken != "":
			res, err = s.CheckIn(r.Context(), partnerID, p.UserID, *in.QRToken)
		case in.Code != nil && in.QRToken == nil && *in.Code != "":
			res, err = s.CheckInByCode(r.Context(), partnerID, p.UserID, *in.Code)
		default:
			err = ErrInvalidCheckIn
		}
		writeView(w, r, http.StatusOK, res, err)
	}))
}
