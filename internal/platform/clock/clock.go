// Package clock abstracts time so business code never calls time.Now directly.
package clock

import (
	"time"
	_ "time/tzdata" // Asia/Ho_Chi_Minh must resolve on hosts without system tzdata
)

// VN is Vietnam time (Asia/Ho_Chi_Minh, UTC+7, no DST). Business dates
// (holidays, dates of birth, booking days) are calendar dates in this zone.
var VN = mustLoadLocation("Asia/Ho_Chi_Minh")

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// Clock returns the current time.
type Clock interface {
	Now() time.Time
}

// Real is the wall clock, always in UTC.
type Real struct{}

// Now returns the current UTC time.
func (Real) Now() time.Time { return time.Now().UTC() }

// Fixed is a controllable clock for tests.
type Fixed struct {
	T time.Time
}

// Now returns the fixed time.
func (f *Fixed) Now() time.Time { return f.T }

// Advance moves the fixed time forward by d.
func (f *Fixed) Advance(d time.Duration) { f.T = f.T.Add(d) }
