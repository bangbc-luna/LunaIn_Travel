package settings

import (
	"net/http"
	"strconv"
	"time"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

type holidayReq struct {
	Date string `json:"date"`
	Name string `json:"name"`
}

// Routes registers the Super Admin settings and holidays API on mux.
func (s *Service) Routes(mux httpx.Router, iss *auth.Issuer) {
	admin := func(h http.HandlerFunc) http.Handler {
		return httpx.Chain(h, auth.Authenticate(iss), auth.RequireRole(auth.RoleSuperAdmin))
	}

	mux.Handle("GET /v1/admin/settings", admin(func(w http.ResponseWriter, r *http.Request) {
		cur, err := s.Get(r.Context())
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, cur)
	}))

	mux.Handle("PATCH /v1/admin/settings", admin(func(w http.ResponseWriter, r *http.Request) {
		var p Patch
		if err := httpx.DecodeJSON(r, &p); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		pr, _ := auth.PrincipalFrom(r.Context())
		cur, err := s.Update(r.Context(), pr.UserID, p)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, cur)
	}))

	mux.Handle("GET /v1/admin/holidays", admin(func(w http.ResponseWriter, r *http.Request) {
		year, err := strconv.Atoi(r.URL.Query().Get("year"))
		if err != nil || year < 1 || year > 9999 {
			httpx.WriteError(w, r, httpx.ErrInvalidRequest)
			return
		}
		list, err := s.ListHolidays(r.Context(), year)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, list)
	}))

	mux.Handle("POST /v1/admin/holidays", admin(func(w http.ResponseWriter, r *http.Request) {
		var req holidayReq
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		date, err := time.Parse(dateLayout, req.Date)
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrInvalidRequest)
			return
		}
		pr, _ := auth.PrincipalFrom(r.Context())
		if err := s.AddHoliday(r.Context(), pr.UserID, date, req.Name); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, Holiday{Date: date.Format(dateLayout), Name: req.Name})
	}))

	mux.Handle("DELETE /v1/admin/holidays/{date}", admin(func(w http.ResponseWriter, r *http.Request) {
		// A malformed {date} cannot name a holiday: 404 (Ruling 11).
		date, err := time.Parse(dateLayout, r.PathValue("date"))
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrNotFound)
			return
		}
		pr, _ := auth.PrincipalFrom(r.Context())
		if err := s.RemoveHoliday(r.Context(), pr.UserID, date); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
