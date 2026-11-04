package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

// muxDeps are the already-constructed services buildMux wires to routes.
type muxDeps struct {
	Pool     *pgxpool.Pool
	Issuer   *auth.Issuer
	ClientIP func(*http.Request) string
	Identity *identity.Service
	Settings *settings.Service
	Files    *files.Service
	Geo      *geo.Service
	Catalog  *catalog.Service
	Partner  *partner.Service
}

// buildMux registers every HTTP route. Unmatched paths get the JSON 404.
func buildMux(d muxDeps) *http.ServeMux {
	mux := http.NewServeMux()
	d.Identity.Routes(mux, d.Issuer, d.ClientIP)
	d.Settings.Routes(mux, d.Issuer)
	d.Files.Routes(mux, d.Issuer, d.ClientIP)
	d.Geo.Routes(mux)
	d.Catalog.Routes(mux, d.Issuer)
	d.Partner.Routes(mux, d.Issuer)
	d.Partner.AdminRoutes(mux, d.Issuer)
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyz(d.Pool))
	mux.HandleFunc("/", httpx.NotFound)
	return mux
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func readyz(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if err := pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unavailable"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	}
}
