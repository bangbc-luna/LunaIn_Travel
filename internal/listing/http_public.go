package listing

import (
	"net/http"
	"strconv"

	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// publicCacheControl is set on successful public responses: a listing change
// (or a partner suspension) may take up to a minute to show.
const publicCacheControl = "public, max-age=60"

// PublicRoutes registers the public (unauthenticated) listing API: search and
// detail by slug. Errors are not cached.
func (s *Service) PublicRoutes(mux httpx.Router) {
	mux.HandleFunc("GET /v1/listings", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var f SearchFilter
		for key, dst := range map[string]**string{"service_type": &f.ServiceType, "province": &f.ProvinceCode, "q": &f.Q} {
			if v := q.Get(key); v != "" {
				*dst = &v
			}
		}
		for key, dst := range map[string]*int{"limit": &f.Limit, "offset": &f.Offset} {
			if v := q.Get(key); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil {
					httpx.WriteError(w, r, httpx.ErrInvalidRequest)
					return
				}
				*dst = n
			}
		}
		items, err := s.Search(r.Context(), f)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", publicCacheControl)
		httpx.WriteJSON(w, http.StatusOK, struct {
			Items []PublicCard `json:"items"`
		}{items})
	})

	mux.HandleFunc("GET /v1/listings/{slug}", func(w http.ResponseWriter, r *http.Request) {
		d, err := s.PublicDetail(r.Context(), r.PathValue("slug"))
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", publicCacheControl)
		httpx.WriteJSON(w, http.StatusOK, d)
	})
}
