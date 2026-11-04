package catalog

import (
	"errors"
	"net/http"
	"time"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

type createRateReq struct {
	ServiceType   string     `json:"service_type"`
	RateBps       *int       `json:"rate_bps"`
	EffectiveFrom *time.Time `json:"effective_from"`
}

// errMissingRate is returned when rate_bps is absent, so a forgotten field
// never silently becomes a 0% commission.
var errMissingRate = httpx.NewError(http.StatusUnprocessableEntity, "invalid_commission_rate", "Thiếu trường rate_bps (tỷ lệ hoa hồng)")

type scheduledRate struct {
	RateBps       int       `json:"rate_bps"`
	EffectiveFrom time.Time `json:"effective_from"`
}

type rateOverview struct {
	ServiceType    string          `json:"service_type"`
	CurrentRateBps *int            `json:"current_rate_bps"`
	Scheduled      []scheduledRate `json:"scheduled"`
}

// overview returns, per service type, the rate in force now and the rates
// scheduled to start later (soonest first).
func (s *Service) overview(r *http.Request) ([]rateOverview, error) {
	types, err := s.ServiceTypes(r.Context())
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	out := make([]rateOverview, 0, len(types))
	for _, st := range types {
		hist, err := s.CommissionHistory(r.Context(), st.Code)
		if err != nil {
			return nil, err
		}
		o := rateOverview{ServiceType: st.Code, Scheduled: []scheduledRate{}}
		// hist is newest first: the first row not in the future is current.
		for _, cr := range hist {
			if cr.EffectiveFrom.After(now) {
				o.Scheduled = append([]scheduledRate{{RateBps: cr.RateBps, EffectiveFrom: cr.EffectiveFrom}}, o.Scheduled...)
				continue
			}
			if o.CurrentRateBps == nil {
				bps := cr.RateBps
				o.CurrentRateBps = &bps
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// Routes registers the public service-type and Super Admin commission API.
func (s *Service) Routes(mux httpx.Router, iss *auth.Issuer) {
	admin := func(h http.HandlerFunc) http.Handler {
		return httpx.Chain(h, auth.Authenticate(iss), auth.RequireRole(auth.RoleSuperAdmin))
	}

	mux.HandleFunc("GET /v1/service-types", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.ServiceTypes(r.Context())
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, list)
	})

	mux.Handle("GET /v1/admin/commission-rates", admin(func(w http.ResponseWriter, r *http.Request) {
		o, err := s.overview(r)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, o)
	}))

	mux.Handle("GET /v1/admin/commission-rates/{service_type}/history", admin(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.CommissionHistory(r.Context(), r.PathValue("service_type"))
		// An unknown code in the path names no resource: 404, not 422
		// (Ruling 11). A bad service_type in a body stays 422.
		if errors.Is(err, ErrInvalidServiceType) {
			err = httpx.ErrNotFound
		}
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, list)
	}))

	mux.Handle("POST /v1/admin/commission-rates", admin(func(w http.ResponseWriter, r *http.Request) {
		var req createRateReq
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if req.ServiceType == "" {
			httpx.WriteError(w, r, ErrInvalidServiceType)
			return
		}
		if req.RateBps == nil {
			httpx.WriteError(w, r, errMissingRate)
			return
		}
		pr, _ := auth.PrincipalFrom(r.Context())
		cr, err := s.CreateCommissionRate(r.Context(), pr.UserID, req.ServiceType, *req.RateBps, req.EffectiveFrom)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, cr)
	}))
}
