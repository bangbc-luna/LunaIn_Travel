package listing

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
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

	mux.Handle("PUT /v1/partner/listings/{id}/images", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in struct {
			FileIDs     []uuid.UUID `json:"file_ids"`
			CoverFileID *uuid.UUID  `json:"cover_file_id"`
		}
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		l, err := s.SetImages(r.Context(), partnerID, userID, id, in.FileIDs, in.CoverFileID)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		images, err := listImages(r.Context(), s.pool, id)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, struct {
			Images      []Image    `json:"images"`
			CoverFileID *uuid.UUID `json:"cover_file_id"`
		}{images, l.CoverFileID})
	}))

	type policyBody struct {
		Tiers []CancellationTier `json:"tiers"`
	}
	mux.Handle("GET /v1/partner/listings/{id}/cancellation-policy", withID(func(w http.ResponseWriter, r *http.Request, partnerID, _, id uuid.UUID) {
		if _, err := s.Get(r.Context(), partnerID, id); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		tiers, err := listTiers(r.Context(), s.pool, id)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, policyBody{tiers})
	}))

	mux.Handle("PUT /v1/partner/listings/{id}/cancellation-policy", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in policyBody
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		tiers, err := s.SetCancellationPolicy(r.Context(), partnerID, userID, id, in.Tiers)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, policyBody{tiers})
	}))

	s.attractionRoutes(mux, withID)
}

// attractionRoutes registers the attraction detail routes: info, ticket
// types, time slots and closed dates.
func (s *Service) attractionRoutes(mux httpx.Router, withID func(h func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID)) http.Handler) {
	mux.Handle("GET /v1/partner/listings/{id}/attraction", withID(func(w http.ResponseWriter, r *http.Request, partnerID, _, id uuid.UUID) {
		l, err := s.Get(r.Context(), partnerID, id)
		if err == nil && l.ServiceType != attractionServiceType {
			err = ErrWrongServiceType
		}
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		d, err := s.attractionDetail(r.Context(), s.pool, id, true)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, d)
	}))

	mux.Handle("PUT /v1/partner/listings/{id}/attraction", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in AttractionInfo
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		info, err := s.SetAttractionInfo(r.Context(), partnerID, userID, id, in.OpeningNote, in.CheckinInstructions)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, info)
	}))

	mux.Handle("POST /v1/partner/listings/{id}/ticket-types", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in TicketTypeInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		tt, err := s.CreateTicketType(r.Context(), partnerID, userID, id, in)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, tt)
	}))

	mux.Handle("PATCH /v1/partner/listings/{id}/ticket-types/{ticketTypeID}", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		ttID, err := uuid.Parse(r.PathValue("ticketTypeID"))
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrNotFound)
			return
		}
		var in TicketTypePatch
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		tt, err := s.UpdateTicketType(r.Context(), partnerID, userID, id, ttID, in)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, tt)
	}))

	mux.Handle("POST /v1/partner/listings/{id}/time-slots", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in TimeSlotInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		slot, err := s.CreateTimeSlot(r.Context(), partnerID, userID, id, in)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, slot)
	}))

	mux.Handle("PATCH /v1/partner/listings/{id}/time-slots/{slotID}", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		slotID, err := uuid.Parse(r.PathValue("slotID"))
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrNotFound)
			return
		}
		var in TimeSlotPatch
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		slot, err := s.UpdateTimeSlot(r.Context(), partnerID, userID, id, slotID, in)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, slot)
	}))

	// A path date that is not YYYY-MM-DD names no resource: 404.
	withDate := func(h func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID, date time.Time)) http.Handler {
		return withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
			date, err := time.ParseInLocation(dateLayout, r.PathValue("date"), clock.VN)
			if err != nil {
				httpx.WriteError(w, r, httpx.ErrNotFound)
				return
			}
			h(w, r, partnerID, userID, id, date)
		})
	}
	mux.Handle("PUT /v1/partner/listings/{id}/closed-dates/{date}", withDate(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID, date time.Time) {
		var in struct {
			Reason *string `json:"reason"`
		}
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if err := s.AddClosedDate(r.Context(), partnerID, userID, id, date, in.Reason); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.Handle("DELETE /v1/partner/listings/{id}/closed-dates/{date}", withDate(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID, date time.Time) {
		if err := s.RemoveClosedDate(r.Context(), partnerID, userID, id, date); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
