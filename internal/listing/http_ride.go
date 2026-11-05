package listing

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// rideRoutes registers the scheduled ride routes: route, schedules and trips. Each
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

	tripID := func(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
		id, err := uuid.Parse(r.PathValue("tripID"))
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrNotFound)
			return uuid.Nil, false
		}
		return id, true
	}

	mux.Handle("GET /v1/partner/listings/{id}/ride-trips", withID(func(w http.ResponseWriter, r *http.Request, partnerID, _, id uuid.UUID) {
		from, to, err := tripRange(r, s.clock.Now())
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		trips, err := s.ListTrips(r.Context(), partnerID, id, from, to)
		reply(w, r, http.StatusOK, trips, err)
	}))

	mux.Handle("POST /v1/partner/listings/{id}/ride-trips", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		var in TripInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		trip, err := s.CreateTrip(r.Context(), partnerID, userID, id, in)
		reply(w, r, http.StatusCreated, trip, err)
	}))

	mux.Handle("PATCH /v1/partner/listings/{id}/ride-trips/{tripID}", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		tid, ok := tripID(w, r)
		if !ok {
			return
		}
		var in TripPatch
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		trip, err := s.UpdateTrip(r.Context(), partnerID, userID, id, tid, in)
		reply(w, r, http.StatusOK, trip, err)
	}))

	mux.Handle("POST /v1/partner/listings/{id}/ride-trips/{tripID}/cancel", withID(func(w http.ResponseWriter, r *http.Request, partnerID, userID, id uuid.UUID) {
		tid, ok := tripID(w, r)
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
		trip, err := s.CancelTripByPartner(r.Context(), partnerID, userID, id, tid, in.Reason)
		reply(w, r, http.StatusOK, trip, err)
	}))
}

// defaultTripDays is the listing window when ?to is missing.
const defaultTripDays = 31

// tripRange reads ?from and ?to, each RFC 3339 or a YYYY-MM-DD Vietnam date.
// A date is its 00:00 for from and the end of that day for to, so
// from=2026-11-06&to=2026-11-06 is the whole day. A missing from is today
// (00:00 Vietnam), a missing to is 31 days after from.
func tripRange(r *http.Request, now time.Time) (time.Time, time.Time, error) {
	q := r.URL.Query()
	vn := now.In(clock.VN)
	from := time.Date(vn.Year(), vn.Month(), vn.Day(), 0, 0, 0, 0, clock.VN)
	if v := q.Get("from"); v != "" {
		t, err := tripBound(v, false)
		if err != nil {
			return from, from, err
		}
		from = t
	}
	to := from.AddDate(0, 0, defaultTripDays)
	if v := q.Get("to"); v != "" {
		t, err := tripBound(v, true)
		if err != nil {
			return from, from, err
		}
		to = t
	}
	return from, to, nil
}

func tripBound(v string, end bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	d, err := time.ParseInLocation(dateLayout, v, clock.VN)
	if err != nil {
		return time.Time{}, invalidRide("from và to phải có dạng RFC 3339 hoặc YYYY-MM-DD")
	}
	if end {
		d = d.AddDate(0, 0, 1)
	}
	return d, nil
}
