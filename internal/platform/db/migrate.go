package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationLockKey is the Postgres session advisory lock key that serializes
// Migrate across processes (e.g. several API replicas starting together).
// It is the ASCII bytes of "LUNAIN" read as a big-endian integer.
const migrationLockKey int64 = 0x4C554E41494E

// Migrate applies the embedded goose migrations, then River's migrations up
// to the latest version. Concurrent callers, in this or other processes, are
// serialized by a session advisory lock held on a dedicated connection, so
// only one runs migrations at a time; the others then find nothing to do.
// It is safe to call repeatedly.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration lock conn: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Background ctx: the lock must be released even if ctx is done.
		if _, uerr := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey); uerr != nil {
			// Closing the session releases the lock; the pool drops the conn.
			_ = conn.Conn().Close(context.Background())
			if err == nil {
				err = fmt.Errorf("release migration lock: %w", uerr)
			}
		}
	}()

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrations fs: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys)
	if err != nil {
		return fmt.Errorf("create goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("create river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}
	return nil
}
