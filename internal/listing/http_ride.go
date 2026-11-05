package listing

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// rideRoutes registers the scheduled ride routes: route and schedules. Each
// one answers ErrNotRideListing for a listing of another service type.
func (s *Service) rideRoutes(mux httpx.Router, withID func(h func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID)) http.Handler) {
	reply := func(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, status, v)
	}

	mux.Handle("GET /v1/partner/listings/{id}/ride", withID(func(w http.ResponseWriter, r *http.Request, partnerID, _, id uuid.UUID) {
		d, err := s.GetRide(r.Context(), partnerID, id)
		reply(w, r, http.StatusOK, d, err)
	}))

	mux.Handle("PUT /v1/partner/listings/{id}/ride-route", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in RideRouteInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		route, err := s.SetRideRoute(r.Context(), partnerID, userID, id, in)
		reply(w, r, http.StatusOK, route, err)
	}))

	mux.Handle("POST /v1/partner/listings/{id}/ride-schedules", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in RideScheduleInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		sched, err := s.CreateRideSchedule(r.Context(), partnerID, userID, id, in)
		reply(w, r, http.StatusCreated, sched, err)
	}))

	mux.Handle("PATCH /v1/partner/listings/{id}/ride-schedules/{scheduleID}", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		scheduleID, err := uuid.Parse(r.PathValue("scheduleID"))
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrNotFound)
			return
		}
		var in RideSchedulePatch
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		sched, err := s.UpdateRideSchedule(r.Context(), partnerID, userID, id, scheduleID, in)
		reply(w, r, http.StatusOK, sched, err)
	}))
}
