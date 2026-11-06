package maps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultGoongTimeout = 5 * time.Second

// GoongConfig configures the Goong Distance Matrix client. Timeout 0 means 5s.
type GoongConfig struct {
	APIKey, BaseURL string
	Timeout         time.Duration
}

// Goong is the real Provider backed by Goong's Distance Matrix API.
type Goong struct {
	cfg GoongConfig
	hc  *http.Client
}

// NewGoong builds a client; hc may be nil. The per-request timeout is applied
// through the request context, so a caller-supplied client is not mutated.
func NewGoong(cfg GoongConfig, hc *http.Client) *Goong {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultGoongTimeout
	}
	if hc == nil {
		hc = &http.Client{}
	}
	return &Goong{cfg: cfg, hc: hc}
}

// Name implements Provider.
func (*Goong) Name() string { return "goong" }

// goongResponse is the Distance Matrix answer. Status is the top-level
// status ("OK" or an error such as a denied key or an exhausted quota); Error
// is set by some error answers instead.
type goongResponse struct {
	Status string          `json:"status"`
	Error  json.RawMessage `json:"error"`
	Rows   []struct {
		Elements []struct {
			Status   string `json:"status"`
			Distance struct {
				Value int64 `json:"value"`
			} `json:"distance"`
			Duration struct {
				Value int64 `json:"value"`
			} `json:"duration"`
		} `json:"elements"`
	} `json:"rows"`
}

// Route implements Provider. Errors never contain the request URL because the
// API key travels in its query string.
func (g *Goong) Route(ctx context.Context, from, to Point) (Route, error) {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	q := url.Values{}
	q.Set("origins", fmt.Sprintf("%.6f,%.6f", from.Lat, from.Lng))
	q.Set("destinations", fmt.Sprintf("%.6f,%.6f", to.Lat, to.Lng))
	q.Set("vehicle", "car")
	q.Set("api_key", g.cfg.APIKey)
	u := strings.TrimRight(g.cfg.BaseURL, "/") + "/DistanceMatrix?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Route{}, fmt.Errorf("%w: build request", ErrUnavailable)
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		// err is a *url.Error carrying the full URL: drop it.
		if ctx.Err() != nil {
			return Route{}, fmt.Errorf("%w: request timed out or canceled", ErrUnavailable)
		}
		return Route{}, fmt.Errorf("%w: request failed", ErrUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return Route{}, fmt.Errorf("%w: http status %d", ErrUnavailable, resp.StatusCode)
	}
	var body goongResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return Route{}, fmt.Errorf("%w: invalid response", ErrUnavailable)
	}
	// A top-level error (denied key, quota) or an answer without any element
	// is the provider failing, not the trip: ErrUnavailable, which the caller
	// logs. Only the status code is kept, never a message (it could echo the
	// request).
	if (body.Status != "" && body.Status != "OK") || (len(body.Error) > 0 && string(body.Error) != "null") {
		return Route{}, fmt.Errorf("%w: status %q", ErrUnavailable, safeStatus(body.Status, g.cfg.APIKey))
	}
	if len(body.Rows) == 0 || len(body.Rows[0].Elements) == 0 {
		return Route{}, fmt.Errorf("%w: no element in the answer", ErrUnavailable)
	}
	el := body.Rows[0].Elements[0]
	switch el.Status {
	case "OK":
	case "ZERO_RESULTS", "NOT_FOUND":
		return Route{}, ErrNoRoute
	default:
		return Route{}, fmt.Errorf("%w: element status %q", ErrUnavailable, safeStatus(el.Status, g.cfg.APIKey))
	}
	if el.Distance.Value <= 0 {
		return Route{}, ErrNoRoute
	}
	return Route{DistanceMeters: el.Distance.Value, DurationSeconds: el.Duration.Value}, nil
}

// safeStatus is a provider status fit for a log line: at most 40 bytes and
// never containing the API key.
func safeStatus(status, key string) string {
	if key != "" && strings.Contains(status, key) {
		return "<redacted>"
	}
	if len(status) > 40 {
		status = status[:40]
	}
	return status
}
