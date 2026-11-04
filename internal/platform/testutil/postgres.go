// Package testutil holds shared helpers for tests that need real infrastructure.
package testutil

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

var (
	once   sync.Once
	dsn    string
	setupE error
)

// Postgres returns a pool on a migrated Postgres 17 container shared by the
// whole test process. Every call truncates all public tables (except the
// goose and river migration bookkeeping tables). Tests in a package must not
// call t.Parallel().
func Postgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Postgres test in short mode")
	}

	once.Do(func() {
		ctx := context.Background()
		// The container lives until the test process exits; Ryuk reaps it.
		c, err := tcpostgres.Run(ctx, "postgres:17",
			tcpostgres.WithDatabase("lunain_test"),
			tcpostgres.WithUsername("test"),
			tcpostgres.WithPassword("test"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(90*time.Second)),
		)
		if err != nil {
			setupE = err
			return
		}
		dsn, setupE = c.ConnectionString(ctx, "sslmode=disable")
		if setupE != nil {
			return
		}
		pool, err := db.Connect(ctx, dsn)
		if err != nil {
			setupE = err
			return
		}
		defer pool.Close()
		setupE = db.Migrate(ctx, pool)
	})
	require.NoError(t, setupE, "start test postgres")

	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	truncateAll(t, ctx, pool)
	return pool
}

func truncateAll(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT quote_ident(tablename) FROM pg_tables
		WHERE schemaname = 'public'
		  AND tablename NOT IN ('goose_db_version', 'river_migration')`)
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	if len(names) == 0 {
		return
	}
	_, err = pool.Exec(ctx, "TRUNCATE "+strings.Join(names, ", ")+" RESTART IDENTITY CASCADE")
	require.NoError(t, err)
}
