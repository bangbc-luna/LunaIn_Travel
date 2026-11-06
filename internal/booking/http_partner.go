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
		// ?trip_id: the passengers of one ride trip.
		if v := r.URL.Query().Get("trip_id"); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				httpx.WriteError(w, r, httpx.ErrInvalidRequest)
				return
			}
			f.TripID = &id
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

	// Request Booking answers (no body for accept).
	mux.Handle("POST /v1/partner/bookings/{id}/accept", member(func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		v, err := s.AcceptRequest(r.Context(), partnerID, p.UserID, id)
		writeView(w, r, http.StatusOK, v, err)
	}))

	mux.Handle("POST /v1/partner/bookings/{id}/reject", member(func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			Reason string `json:"reason"`
		}
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		v, err := s.RejectRequest(r.Context(), partnerID, p.UserID, id, in.Reason)
		writeView(w, r, http.StatusOK, v, err)
	}))

	// Self-drive rental or private charter: the customer never came (no
	// body). Rental: the vehicle came back; charter: the trip is over (no
	// body). The handover or pickup is POST /v1/partner/check-ins.
	mux.Handle("POST /v1/partner/bookings/{id}/no-show", member(func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		v, err := s.MarkNoShow(r.Context(), partnerID, p.UserID, id)
		writeView(w, r, http.StatusOK, v, err)
	}))

	mux.Handle("POST /v1/partner/bookings/{id}/return", member(func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		v, err := s.MarkRentalReturned(r.Context(), partnerID, p.UserID, id)
		writeView(w, r, http.StatusOK, v, err)
	}))

	mux.Handle("POST /v1/partner/bookings/{id}/complete", member(func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		v, err := s.CompleteCharter(r.Context(), partnerID, p.UserID, id)
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
