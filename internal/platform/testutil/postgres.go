// Package testutil holds shared helpers for tests that need real infrastructure.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

const templateDB = "lunain_template"

var (
	once    sync.Once
	baseDSN string        // DSN of the maintenance database "postgres"
	setupE  error         // result of the one-time setup
	admin   *pgxpool.Pool // connected to "postgres"; lives until process exit

	cloneMu sync.Mutex // CREATE DATABASE ... TEMPLATE needs the template idle
)

// Postgres returns a pool on a fresh database cloned from a template that was
// migrated once per test process (Postgres 17 container shared by the
// process). Seed data lives only in migrations: each test gets a migrated and
// seeded database of its own, dropped on cleanup. t.Parallel is safe but not
// required.
func Postgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Postgres test in short mode")
	}
	ctx := context.Background()

	once.Do(func() { setupE = setup(ctx) })
	require.NoError(t, setupE, "start test postgres")

	var b [8]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	name := "t_" + hex.EncodeToString(b[:])

	cloneMu.Lock()
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, name, templateDB))
	cloneMu.Unlock()
	require.NoError(t, err)

	pool, err := db.Connect(ctx, withDB(baseDSN, name))
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		_, err := admin.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
		if err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})
	return pool
}

// setup starts the container, then creates and migrates the template database.
func setup(ctx context.Context) error {
	// The container lives until the test process exits; Ryuk reaps it.
	c, err := tcpostgres.Run(ctx, "postgres:17",
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		return err
	}
	baseDSN, err = c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return err
	}
	admin, err = db.Connect(ctx, baseDSN)
	if err != nil {
		return err
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+templateDB); err != nil {
		return err
	}
	tpl, err := db.Connect(ctx, withDB(baseDSN, templateDB))
	if err != nil {
		return err
	}
	// A template must have no open connections when it is cloned.
	defer tpl.Close()
	return db.Migrate(ctx, tpl)
}

func withDB(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + name
	return u.String()
}
