package booking

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestJobBackoff_Doubling(t *testing.T) {
	var b jobBackoff
	id := uuid.New()
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	require.Empty(t, b.skip(now))
	require.NotNil(t, b.skip(now), "never nil: a NULL array would filter every row")

	waits := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range waits {
		require.Equal(t, i+1, b.fail(id, now))
		wait := w * time.Minute
		require.Equal(t, []uuid.UUID{id}, b.skip(now.Add(wait-time.Second)), "failure %d waits %v", i+1, wait)
		require.Empty(t, b.skip(now.Add(wait)), "failure %d waits %v", i+1, wait)
	}
	for range 100 { // no shift overflow
		b.fail(id, now)
	}
	require.Equal(t, []uuid.UUID{id}, b.skip(now.Add(59*time.Minute)))
	require.Empty(t, b.skip(now.Add(time.Hour)))

	b.ok(id)
	require.Equal(t, 1, b.fail(id, now), "ok clears the state")
	require.Empty(t, b.skip(now.Add(time.Minute)))
}
