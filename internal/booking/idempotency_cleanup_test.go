package booking_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPurgeIdempotencyKeys_TTL(t *testing.T) {
	e := newEnv(t)
	old := e.create(t, "old-key")
	fresh := e.create(t, "fresh-key")
	e.exec(t, `UPDATE idempotency_keys SET created_at = $1 WHERE key = 'old-key'`, testNow.Add(-25*time.Hour))
	e.exec(t, `UPDATE idempotency_keys SET created_at = $1 WHERE key = 'fresh-key'`, testNow.Add(-23*time.Hour))

	n, err := e.Svc.PurgeIdempotencyKeys(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM idempotency_keys WHERE key = 'old-key'`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM idempotency_keys WHERE key = 'fresh-key'`))

	// The 23h key still replays; the purged one creates a new booking.
	require.Equal(t, fresh.ID, e.create(t, "fresh-key").ID)
	again := e.create(t, "old-key")
	require.NotEqual(t, old.ID, again.ID)

	n, err = e.Svc.PurgeIdempotencyKeys(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 0, n)
}
