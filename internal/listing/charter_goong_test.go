package listing_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/platform/maps"
)

// Goong answering HTTP 200 with a denied key (or an exhausted quota) is a
// 503 map_unavailable for the customer, not a 422 no_route, and it is logged
// as a warning carrying the status but never the API key.
func TestQuoteCharter_GoongDeniedKeyIs503AndLoggedWithoutKey(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l, _, _ := e.charterPublished(t, p)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"REQUEST_DENIED","error_message":"The provided API key secret-goong-key is invalid.","rows":[]}`))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	svc := listing.NewService(listing.Deps{
		Pool: e.Pool, Clock: e.Clock, Partners: e.Partner, Catalog: catalog.NewService(e.Pool, e.Clock),
		Geo: geo.NewService(e.Pool), Files: e.Files, BookingGuard: booking.Guard{}, Holidays: e.Settings,
		Maps: maps.NewGoong(maps.GoongConfig{APIKey: "secret-goong-key", BaseURL: srv.URL}, nil),
		Log:  slog.New(slog.NewTextHandler(&logs, nil)),
	})

	_, err := svc.QuoteCharter(ctx, listing.CharterQuoteInput{ListingID: l.ID, Pickup: airport, Dropoff: hoiAn, PickupAt: wed0800})
	requireErr(t, err, http.StatusServiceUnavailable, "map_unavailable")
	require.Contains(t, logs.String(), "level=WARN")
	require.Contains(t, logs.String(), "REQUEST_DENIED")
	require.NotContains(t, logs.String(), "secret-goong-key")
	require.NotContains(t, logs.String(), "api_key")
}
