package money

import "testing"

func TestMulBps(t *testing.T) {
	cases := []struct {
		amount int64
		bps    int
		want   int64
	}{
		{1_000_000, 1000, 100_000},
		{12_345, 1000, 1_235},
		{5, 1000, 1},
		{4, 1000, 0},
		{0, 10000, 0},
		{999, 10000, 999},
		{10_000_000_000_000, 10000, 10_000_000_000_000},
	}
	for _, c := range cases {
		if got := MulBps(c.amount, c.bps); got != c.want {
			t.Errorf("MulBps(%d,%d)=%d want %d", c.amount, c.bps, got, c.want)
		}
	}
}

func TestMulBpsPanics(t *testing.T) {
	cases := []struct {
		name   string
		amount int64
		bps    int
	}{
		{"bps negative", 100, -1},
		{"bps too large", 100, 10001},
		{"amount negative", -1, 1000},
		{"amount too large", 10_000_000_000_001, 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			MulBps(c.amount, c.bps)
		})
	}
}
