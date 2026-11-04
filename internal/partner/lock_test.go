package partner_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStatusLock_BlocksSuspendUntilCommit pins the Status(lock=true) contract
// (DB §1.1): its FOR SHARE makes a concurrent Suspend (FOR NO KEY UPDATE on
// the same partners row) wait until the caller's tx ends.
func TestStatusLock_BlocksSuspendUntilCommit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.create(t, input("alpha", "attraction_ticket"))

	txA, err := e.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = txA.Rollback(ctx) }()
	st, err := e.Svc.Status(ctx, txA, p.ID, true)
	require.NoError(t, err)
	require.Equal(t, "active", st)
	ok, err := e.Svc.IsServiceTypeEnabled(ctx, txA, p.ID, "attraction_ticket")
	require.NoError(t, err)
	require.True(t, ok)

	done := make(chan error, 1)
	go func() { done <- e.Svc.Suspend(ctx, e.Admin, p.ID, "Vi phạm") }()

	select {
	case err := <-done:
		t.Fatalf("Suspend completed while Status(lock=true) tx was open: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	require.NoError(t, txA.Commit(ctx))

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Suspend did not complete within 5s after the Status(lock=true) tx committed")
	}
	require.Equal(t, "suspended", e.status(t, p.ID, false))
}
