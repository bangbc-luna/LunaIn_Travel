package maps_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bangbc-luna/lunain-travel/internal/platform/maps"
)

var danang = maps.Point{Lat: 16.0544, Lng: 108.2022}

func TestGoong_ParsesDistanceMatrix(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		_, _ = w.Write([]byte(`{"rows":[{"elements":[{"status":"OK","distance":{"value":30240},"duration":{"value":2700}}]}]}`))
	}))
	defer srv.Close()
	g := maps.NewGoong(maps.GoongConfig{APIKey: "k", BaseURL: srv.URL}, nil)
	if g.Name() != "goong" {
		t.Fatal(g.Name())
	}
	r, err := g.Route(context.Background(), danang, hanoi)
	if err != nil {
		t.Fatal(err)
	}
	if r != (maps.Route{DistanceMeters: 30240, DurationSeconds: 2700}) {
		t.Fatalf("route = %v", r)
	}
	for _, want := range []string{
		"/DistanceMatrix?", "origins=16.054400%2C108.202200", "destinations=21.028511%2C105.804817",
		"vehicle=car", "api_key=k",
	} {
		if !strings.Contains(gotURL, want) {
			t.Errorf("request %q lacks %q", gotURL, want)
		}
	}
}

func TestGoong_NoRoute(t *testing.T) {
	for name, body := range map[string]string{
		"zero results": `{"rows":[{"elements":[{"status":"ZERO_RESULTS"}]}]}`,
		"empty rows":   `{"rows":[]}`,
		"zero meters":  `{"rows":[{"elements":[{"status":"OK","distance":{"value":0},"duration":{"value":0}}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			g := maps.NewGoong(maps.GoongConfig{APIKey: "k", BaseURL: srv.URL}, nil)
			if _, err := g.Route(context.Background(), danang, hanoi); !errors.Is(err, maps.ErrNoRoute) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestGoong_TimeoutIsUnavailable(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer slow.Close()
	g := maps.NewGoong(maps.GoongConfig{APIKey: "secret-key", BaseURL: slow.URL, Timeout: 50 * time.Millisecond}, nil)
	start := time.Now()
	_, err := g.Route(context.Background(), danang, hanoi)
	if !errors.Is(err, maps.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("took %v", d)
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("error leaks key: %v", err)
	}

	for name, h := range map[string]http.HandlerFunc{
		"500":      func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) },
		"bad json": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`<html>`)) },
	} {
		srv := httptest.NewServer(h)
		g := maps.NewGoong(maps.GoongConfig{APIKey: "secret-key", BaseURL: srv.URL}, nil)
		_, err := g.Route(context.Background(), danang, hanoi)
		srv.Close()
		if !errors.Is(err, maps.ErrUnavailable) || strings.Contains(err.Error(), "secret-key") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Connection refused: the URL (carrying the key) must not leak either.
	g = maps.NewGoong(maps.GoongConfig{APIKey: "secret-key", BaseURL: "http://127.0.0.1:1"}, nil)
	if _, err := g.Route(context.Background(), danang, hanoi); !errors.Is(err, maps.ErrUnavailable) || strings.Contains(err.Error(), "secret-key") {
		t.Errorf("refused: err = %v", err)
	}
}
