package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/payout"
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
	Listing  *listing.Service
	Ledger   *ledger.Service
	Payment  *payment.Service
	Booking  *booking.Service
	Payout   *payout.Service
	// AppEnv gates the dev-only payment simulation route (never in prod).
	AppEnv string
}

// buildMux registers every HTTP route and returns the mux plus every pattern
// registered on it (sorted). Unmatched paths get the JSON 404.
func buildMux(d muxDeps) (*http.ServeMux, []string) {
	mux := http.NewServeMux()
	rr := httpx.NewRecordingRouter(mux)
	d.Identity.Routes(rr, d.Issuer, d.ClientIP)
	d.Settings.Routes(rr, d.Issuer)
	d.Files.Routes(rr, d.Issuer, d.ClientIP)
	d.Geo.Routes(rr)
	d.Catalog.Routes(rr, d.Issuer)
	d.Partner.Routes(rr, d.Issuer)
	d.Partner.AdminRoutes(rr, d.Issuer)
	d.Listing.PartnerRoutes(rr, d.Issuer, d.Partner.RequireMember())
	d.Listing.AdminRoutes(rr, d.Issuer)
	d.Listing.PublicRoutes(rr)
	d.Ledger.PartnerRoutes(rr, d.Issuer, d.Partner.RequireMember())
	d.Ledger.AdminRoutes(rr, d.Issuer, d.Partner)
	d.Booking.CustomerRoutes(rr, d.Issuer)
	d.Booking.PartnerRoutes(rr, d.Issuer, d.Partner.RequireMember())
	d.Booking.AdminRoutes(rr, d.Issuer)
	d.Payout.PartnerRoutes(rr, d.Issuer, d.Partner.RequireMember())
	d.Payment.Routes(rr, payment.RouteDeps{Handler: d.Booking, ClientIP: d.ClientIP, AppEnv: d.AppEnv})
	rr.HandleFunc("GET /healthz", healthz)
	rr.HandleFunc("GET /readyz", readyz(d.Pool))
	rr.HandleFunc("/", httpx.NotFound)
	return mux, rr.Patterns()
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
