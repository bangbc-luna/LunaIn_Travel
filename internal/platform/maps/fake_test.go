package maps_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bangbc-luna/lunain-travel/internal/platform/maps"
)

var (
	hanoi    = maps.Point{Lat: 21.028511, Lng: 105.804817}
	haiPhong = maps.Point{Lat: 20.844912, Lng: 106.688084}
)

func TestFake_DeterministicAndOverrides(t *testing.T) {
	ctx := context.Background()
	f := maps.NewFake()
	if f.Name() != "fake" {
		t.Fatalf("name = %q", f.Name())
	}
	r1, err := f.Route(ctx, hanoi, haiPhong)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := f.Route(ctx, hanoi, haiPhong)
	if r1 != r2 {
		t.Fatalf("not deterministic: %v vs %v", r1, r2)
	}
	if r1.DistanceMeters < 110000 || r1.DistanceMeters > 130000 {
		t.Fatalf("distance = %d", r1.DistanceMeters)
	}
	if want := (r1.DistanceMeters*3600 + 20000) / 40000; r1.DurationSeconds < want-1 || r1.DurationSeconds > want+1 {
		t.Fatalf("duration = %d, want ~%d", r1.DurationSeconds, want)
	}
	if f.Calls() != 2 {
		t.Fatalf("calls = %d", f.Calls())
	}

	f.SetRoute(hanoi, haiPhong, maps.Route{DistanceMeters: 5, DurationSeconds: 6})
	if r, _ := f.Route(ctx, hanoi, haiPhong); r != (maps.Route{DistanceMeters: 5, DurationSeconds: 6}) {
		t.Fatalf("override lost: %v", r)
	}

	f.SetErr(maps.ErrUnavailable)
	if _, err := f.Route(ctx, hanoi, haiPhong); !errors.Is(err, maps.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	f.SetErr(nil)
	if _, err := f.Route(ctx, hanoi, hanoi); !errors.Is(err, maps.ErrNoRoute) {
		t.Fatalf("same point err = %v", err)
	}
}
