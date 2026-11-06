// Package maps abstracts the road-distance provider used to price private
// charters. Implementations: Fake (deterministic, dev/test), Goong (real) and
// Cached (in-memory wrapper). Callers must never invoke it inside a database
// transaction.
package maps

import (
	"context"
	"errors"
)

// Point is a WGS84 coordinate.
type Point struct{ Lat, Lng float64 }

// Route is the driving distance and duration between two points.
type Route struct{ DistanceMeters, DurationSeconds int64 }

// ErrNoRoute means the provider found no drivable route between the points.
var ErrNoRoute = errors.New("maps: no route")

// ErrUnavailable means the provider failed (network, timeout, HTTP or decode
// error). Every error a Provider returns satisfies errors.Is for one of
// ErrNoRoute or ErrUnavailable.
var ErrUnavailable = errors.New("maps: provider unavailable")

// Provider computes a driving route.
type Provider interface {
	Name() string
	Route(ctx context.Context, from, to Point) (Route, error)
}
