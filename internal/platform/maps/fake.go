package maps

import (
	"context"
	"math"
	"sync"
)

const (
	earthRadiusMeters = 6371000.0
	fakeRoadFactor    = 1.3
	fakeSpeedKmh      = 40.0
)

type fakeKey struct{ from, to Point }

// Fake is a deterministic in-memory Provider for dev and tests. Without an
// override the distance is the haversine distance times 1.3 and the duration
// assumes 40 km/h. It is safe for concurrent use.
type Fake struct {
	mu     sync.Mutex
	routes map[fakeKey]Route
	err    error
	calls  int
}

// NewFake returns an empty Fake.
func NewFake() *Fake { return &Fake{routes: map[fakeKey]Route{}} }

// Name implements Provider.
func (*Fake) Name() string { return "fake" }

// SetRoute pins the result for an exact (from, to) pair.
func (f *Fake) SetRoute(from, to Point, r Route) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[fakeKey{from, to}] = r
}

// SetErr makes every Route call fail with err; nil clears it.
func (f *Fake) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Calls returns how many times Route was called.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Route implements Provider.
func (f *Fake) Route(_ context.Context, from, to Point) (Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return Route{}, f.err
	}
	if r, ok := f.routes[fakeKey{from, to}]; ok {
		return r, nil
	}
	if from == to {
		return Route{}, ErrNoRoute
	}
	d := int64(math.Round(haversineMeters(from, to) * fakeRoadFactor))
	return Route{
		DistanceMeters:  d,
		DurationSeconds: int64(math.Round(float64(d) / (fakeSpeedKmh * 1000 / 3600))),
	}, nil
}

func haversineMeters(a, b Point) float64 {
	rad := math.Pi / 180
	dLat := (b.Lat - a.Lat) * rad
	dLng := (b.Lng - a.Lng) * rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(a.Lat*rad)*math.Cos(b.Lat*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthRadiusMeters * math.Asin(math.Sqrt(h))
}
