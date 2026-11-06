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

type goongResponse struct {
	Rows []struct {
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
	if len(body.Rows) == 0 || len(body.Rows[0].Elements) == 0 {
		return Route{}, ErrNoRoute
	}
	el := body.Rows[0].Elements[0]
	if el.Status != "OK" || el.Distance.Value <= 0 {
		return Route{}, ErrNoRoute
	}
	return Route{DistanceMeters: el.Distance.Value, DurationSeconds: el.Duration.Value}, nil
}
