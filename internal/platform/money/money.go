// Package money holds integer VND arithmetic helpers. Never use floats for money.
package money

import (
	"fmt"
	"math"
)

// MaxAmount is the largest amount MulBps accepts; amount*10000 stays far below
// the int64 limit for any amount up to this value.
const MaxAmount int64 = 10_000_000_000_000

// MulBps returns amount * bps / 10000 rounded half up, matching Postgres
// round() for positive numbers. It panics (programmer error) if amount is
// outside [0, MaxAmount] or bps is outside [0, 10000].
func MulBps(amount int64, bps int) int64 {
	if amount < 0 || amount > MaxAmount {
		panic(fmt.Sprintf("money.MulBps: amount %d out of range [0, %d]", amount, MaxAmount))
	}
	if bps < 0 || bps > 10000 {
		panic(fmt.Sprintf("money.MulBps: bps %d out of range [0, 10000]", bps))
	}
	return (amount*int64(bps) + 5000) / 10000
}

// MulTenths returns amount * tenths / 10 rounded half up: a unit price times
// a quantity with one decimal (tenths = quantity × 10), such as a price per km
// times a distance rounded to 0.1 km. It panics (programmer error) if either
// argument is negative or the product would overflow int64.
func MulTenths(amount, tenths int64) int64 {
	if amount < 0 || tenths < 0 {
		panic(fmt.Sprintf("money.MulTenths: negative argument (%d, %d)", amount, tenths))
	}
	if amount > 0 && tenths > (math.MaxInt64-5)/amount {
		panic(fmt.Sprintf("money.MulTenths: %d × %d overflows", amount, tenths))
	}
	return (amount*tenths + 5) / 10
}
