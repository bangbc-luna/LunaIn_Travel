package geo

import (
	"net/http"

	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// cacheControl is set on successful responses: the catalog changes only with
// a migration, so clients and CDNs may cache it for a day.
const cacheControl = "public, max-age=86400"

// Routes registers the public (unauthenticated) geo catalog API on mux.
func (s *Service) Routes(mux httpx.Router) {
	mux.HandleFunc("GET /v1/geo/provinces", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Provinces(r.Context())
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", cacheControl)
		httpx.WriteJSON(w, http.StatusOK, list)
	})

	mux.HandleFunc("GET /v1/geo/provinces/{code}/wards", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Wards(r.Context(), r.PathValue("code"))
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", cacheControl)
		httpx.WriteJSON(w, http.StatusOK, list)
	})
}
