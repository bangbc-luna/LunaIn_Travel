package booking_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
)

// failFor makes the job hook fail the given ids and records every id tried.
func (e *env) failFor(batch int32, bad ...uuid.UUID) *[]uuid.UUID {
	var mu sync.Mutex
	tried := &[]uuid.UUID{}
	e.Svc.SetJobHooks(batch, func(id uuid.UUID) error {
		mu.Lock()
		defer mu.Unlock()
		*tried = append(*tried, id)
		if slices.Contains(bad, id) {
			return errors.New("poison")
		}
		return nil
	})
	return tried
}

// A row that always fails backs off instead of being retried first every
// run: the rest of the batch gets through, and the row comes back after its
// wait.
func TestRelease_PoisonRowDoesNotStarve(t *testing.T) {
	e := newEnv(t)
	var ids []uuid.UUID
	for _, k := range []string{"k1", "k2", "k3"} {
		v := e.create(t, k)
		e.pay(t, e.orderCode(t, v.ID), gross)
		ids = append(ids, v.ID)
	}
	noShowAt := serviceEnd.Add(time.Minute)
	e.Clock.T = noShowAt
	require.Equal(t, 3, e.markNoShows(t))
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }) // list order: same due time, by id
	e.Clock.T = noShowAt.Add(24 * time.Hour)

	tried := e.failFor(2, ids[0], ids[1])
	require.Equal(t, 0, e.releaseDue(t), "run 1: the first two fail")
	require.Equal(t, ids[:2], *tried)

	*tried = nil
	require.Equal(t, 1, e.releaseDue(t), "run 2, same now: the failing rows are skipped")
	require.Equal(t, []uuid.UUID{ids[2]}, *tried)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_settlements WHERE booking_id = $1 AND released_at IS NOT NULL`, ids[2]))

	*tried = nil
	e.Clock.Advance(2 * time.Minute)
	require.Equal(t, 0, e.releaseDue(t))
	require.Equal(t, ids[:2], *tried, "retried after the backoff")
}

func TestExpireHolds_PoisonCheckoutSkipped(t *testing.T) {
	e := newEnv(t)
	var vs []booking.BookingView
	for _, k := range []string{"k1", "k2", "k3"} {
		vs = append(vs, e.create(t, k))
		e.Clock.Advance(time.Minute)
	}
	checkout := func(id uuid.UUID) uuid.UUID {
		var c uuid.UUID
		require.NoError(t, e.Pool.QueryRow(context.Background(), `SELECT checkout_id FROM bookings WHERE id = $1`, id).Scan(&c))
		return c
	}
	c1, c2, c3 := checkout(vs[0].ID), checkout(vs[1].ID), checkout(vs[2].ID)
	e.Clock.T = vs[2].HoldExpiresAt.Add(6 * time.Minute)

	tried := e.failFor(2, c1, c2)
	require.Equal(t, 0, e.expireHolds(t))
	require.Equal(t, []uuid.UUID{c1, c2}, *tried)

	*tried = nil
	require.Equal(t, 1, e.expireHolds(t), "the poison checkouts are skipped")
	require.Equal(t, []uuid.UUID{c3}, *tried)
	require.Equal(t, "expired", e.bookingStatus(t, vs[2].ID))
	require.Equal(t, "pending_payment", e.bookingStatus(t, vs[0].ID))

	*tried = nil
	e.Clock.Advance(2 * time.Minute)
	require.Equal(t, 0, e.expireHolds(t))
	require.Equal(t, []uuid.UUID{c1, c2}, *tried, "retried after the backoff")
}
