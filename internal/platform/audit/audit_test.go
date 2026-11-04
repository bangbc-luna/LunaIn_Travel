package audit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
)

func TestRecord_StoresJSONAndRollsBackWithTx(t *testing.T) {
	pool := testutil.Postgres(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

	actor := uuid.Must(uuid.NewV7())
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, role) VALUES ($1, 'a@x.vn', 'super_admin')`, actor)
	require.NoError(t, err)

	count := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_log`).Scan(&n))
		return n
	}

	boom := errors.New("boom")
	err = db.InTx(ctx, pool, func(tx pgx.Tx) error {
		require.NoError(t, audit.Record(ctx, tx, now, audit.Entry{ActorUserID: actor, Action: "x.y", TargetKind: "x"}))
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Equal(t, 0, count())

	target := uuid.Must(uuid.NewV7())
	require.NoError(t, db.InTx(ctx, pool, func(tx pgx.Tx) error {
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor, Action: "x.update", TargetKind: "x", TargetID: &target,
			Before: map[string]int{"n": 1}, After: map[string]int{"n": 2}, Reason: "because",
		})
	}))
	require.NoError(t, db.InTx(ctx, pool, func(tx pgx.Tx) error {
		return audit.Record(ctx, tx, now, audit.Entry{ActorUserID: actor, Action: "x.noop", TargetKind: "x"})
	}))
	require.Equal(t, 2, count())

	var gotTarget *uuid.UUID
	var before, after, reason *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT target_id, before::text, after::text, reason FROM admin_audit_log WHERE action = 'x.update'`).
		Scan(&gotTarget, &before, &after, &reason))
	require.Equal(t, target, *gotTarget)
	require.JSONEq(t, `{"n":1}`, *before)
	require.JSONEq(t, `{"n":2}`, *after)
	require.Equal(t, "because", *reason)

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT target_id, before::text, after::text, reason FROM admin_audit_log WHERE action = 'x.noop'`).
		Scan(&gotTarget, &before, &after, &reason))
	require.Nil(t, gotTarget)
	require.Nil(t, before)
	require.Nil(t, after)
	require.Nil(t, reason)
}
