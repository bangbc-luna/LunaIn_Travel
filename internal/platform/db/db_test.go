package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
)

func TestMigrate_Idempotent(t *testing.T) {
	pool := testutil.Postgres(t)
	ctx := context.Background()

	require.NoError(t, db.Migrate(ctx, pool))
	require.NoError(t, db.Migrate(ctx, pool))

	var exists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('public.river_job') IS NOT NULL`).Scan(&exists))
	require.True(t, exists)
}

// probeTable creates t_probe and returns a row counter.
func probeTable(t *testing.T, pool *pgxpool.Pool) func() int {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DROP TABLE IF EXISTS t_probe`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `CREATE TABLE t_probe(id int)`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS t_probe`) })
	return func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM t_probe`).Scan(&n))
		return n
	}
}

func TestInTx_RollbackOnError(t *testing.T) {
	pool := testutil.Postgres(t)
	count := probeTable(t, pool)
	ctx := context.Background()
	boom := errors.New("boom")

	err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO t_probe VALUES (1)`)
		require.NoError(t, err)
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Equal(t, 0, count())
}

func TestInTx_RollbackOnPanic(t *testing.T) {
	pool := testutil.Postgres(t)
	count := probeTable(t, pool)
	ctx := context.Background()

	require.PanicsWithValue(t, "x", func() {
		_ = db.InTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO t_probe VALUES (1)`)
			require.NoError(t, err)
			panic("x")
		})
	})
	require.Equal(t, 0, count())
}

func TestInTx_Commit(t *testing.T) {
	pool := testutil.Postgres(t)
	count := probeTable(t, pool)
	ctx := context.Background()

	err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO t_probe VALUES (1)`)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, 1, count())
}
