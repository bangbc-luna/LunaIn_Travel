// Package money holds integer VND arithmetic helpers. Never use floats for money.
package money

import "fmt"

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
