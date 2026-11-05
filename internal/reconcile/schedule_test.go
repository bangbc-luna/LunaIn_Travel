package reconcile_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/reconcile"
)

func TestDailyAt_Next(t *testing.T) {
	vn := time.FixedZone("ICT", 7*3600)
	s := reconcile.DailyAt(2, 30, vn)
	cases := []struct {
		name string
		from time.Time
		want time.Time
	}{
		{"before, same day", time.Date(2026, 11, 5, 2, 29, 0, 0, vn), time.Date(2026, 11, 5, 2, 30, 0, 0, vn)},
		{"exactly at, next day", time.Date(2026, 11, 5, 2, 30, 0, 0, vn), time.Date(2026, 11, 6, 2, 30, 0, 0, vn)},
		{"just after, next day", time.Date(2026, 11, 5, 2, 30, 0, 1, vn), time.Date(2026, 11, 6, 2, 30, 0, 0, vn)},
		{"23:00 UTC is 06:00 VN next day", time.Date(2026, 11, 5, 23, 0, 0, 0, time.UTC), time.Date(2026, 11, 7, 2, 30, 0, 0, vn)},
		{"19:00 UTC is 02:00 VN next day", time.Date(2026, 11, 5, 19, 0, 0, 0, time.UTC), time.Date(2026, 11, 6, 2, 30, 0, 0, vn)},
		{"month end", time.Date(2026, 12, 31, 3, 0, 0, 0, vn), time.Date(2027, 1, 1, 2, 30, 0, 0, vn)},
	}
	for _, c := range cases {
		got := s.Next(c.from)
		require.True(t, got.Equal(c.want), "%s: got %s want %s", c.name, got, c.want)
		require.True(t, got.After(c.from), c.name)
	}
}

func TestPeriodicJobs(t *testing.T) {
	require.Len(t, reconcile.PeriodicJobs(), 2)
	require.Equal(t, "reconcile_daily", reconcile.DailyArgs{}.Kind())
	require.Equal(t, "payment_remind_refunds", reconcile.RemindRefundsArgs{}.Kind())
}
