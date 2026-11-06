package maps_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bangbc-luna/lunain-travel/internal/platform/maps"
)

func TestCached_HitsAndExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	f := maps.NewFake()
	c := maps.NewCached(f, time.Hour, 2, clock)
	if c.Name() != "fake" {
		t.Fatal(c.Name())
	}

	a := maps.Point{Lat: 10.1234501, Lng: 106.1}
	a2 := maps.Point{Lat: 10.1234504, Lng: 106.1} // differs past the 5th digit
	if _, err := c.Route(ctx, a, hanoi); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Route(ctx, a2, hanoi); err != nil {
		t.Fatal(err)
	}
	if f.Calls() != 1 {
		t.Fatalf("calls = %d, want 1", f.Calls())
	}
	now = now.Add(time.Hour + time.Second)
	_, _ = c.Route(ctx, a, hanoi)
	if f.Calls() != 2 {
		t.Fatalf("calls after ttl = %d, want 2", f.Calls())
	}

	// Errors are not cached.
	f.SetErr(maps.ErrUnavailable)
	b := maps.Point{Lat: 11, Lng: 107}
	before := f.Calls()
	for i := 0; i < 2; i++ {
		if _, err := c.Route(ctx, b, hanoi); !errors.Is(err, maps.ErrUnavailable) {
			t.Fatal(err)
		}
	}
	if f.Calls() != before+2 {
		t.Fatalf("error was cached: calls = %d", f.Calls())
	}
	f.SetErr(nil)

	// max = 2: the oldest entry is evicted.
	f2 := maps.NewFake()
	c2 := maps.NewCached(f2, time.Hour, 2, clock)
	p1, p2, p3 := maps.Point{Lat: 1, Lng: 100}, maps.Point{Lat: 2, Lng: 100}, maps.Point{Lat: 3, Lng: 100}
	for _, p := range []maps.Point{p1, p2, p3} {
		if _, err := c2.Route(ctx, p, hanoi); err != nil {
			t.Fatal(err)
		}
	}
	if f2.Calls() != 3 {
		t.Fatalf("calls = %d", f2.Calls())
	}
	_, _ = c2.Route(ctx, p3, hanoi) // still cached
	_, _ = c2.Route(ctx, p2, hanoi) // still cached
	if f2.Calls() != 3 {
		t.Fatalf("calls = %d, want 3", f2.Calls())
	}
	_, _ = c2.Route(ctx, p1, hanoi) // evicted
	if f2.Calls() != 4 {
		t.Fatalf("calls = %d, want 4", f2.Calls())
	}
}
