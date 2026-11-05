package payout

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// listPage is the JSON of a payout list page.
type listPage struct {
	Items      []Payout   `json:"items"`
	NextCursor *uuid.UUID `json:"next_cursor"`
}

// requestInput is the POST /v1/partner/payouts body.
type requestInput struct {
	Amount int64 `json:"amount"`
}

// parseListFilter reads ?status, ?limit and ?cursor; bad values are
// httpx.ErrInvalidRequest.
func parseListFilter(q url.Values) (ListFilter, error) {
	var f ListFilter
	if v := q.Get("status"); v != "" {
		f.Status = &v
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return f, httpx.ErrInvalidRequest
		}
		f.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		c, err := uuid.Parse(v)
		if err != nil {
			return f, httpx.ErrInvalidRequest
		}
		f.Cursor = &c
	}
	return f, nil
}

// pathID parses the {id} path value; a bad id is a 404 like an unknown one.
func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, r, ErrPayoutNotFound)
		return uuid.Nil, false
	}
	return id, true
}

func write(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, status, v)
}

// PartnerRoutes registers the partner payout API. The chain is
// Authenticate -> RequireRole(partner) -> requireMember; the partner_id comes
// from partner.IDFrom, never the client. Another partner's payout is 404.
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

	mux.Handle("POST /v1/partner/payouts", member(func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID) {
		var in requestInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		v, err := s.Request(r.Context(), partnerID, p.UserID, in.Amount)
		write(w, r, http.StatusCreated, v, err)
	}))

	mux.Handle("GET /v1/partner/payouts", member(func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID) {
		f, err := parseListFilter(r.URL.Query())
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		items, next, err := s.ListMine(r.Context(), partnerID, f)
		write(w, r, http.StatusOK, listPage{Items: items, NextCursor: next}, err)
	}))

	mux.Handle("GET /v1/partner/payouts/preview", member(func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID) {
		pv, err := s.Preview(r.Context(), partnerID)
		write(w, r, http.StatusOK, pv, err)
	}))

	mux.Handle("GET /v1/partner/payouts/{id}", member(func(w http.ResponseWriter, r *http.Request, partnerID uuid.UUID) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		v, err := s.GetMine(r.Context(), partnerID, id)
		write(w, r, http.StatusOK, v, err)
	}))
}
