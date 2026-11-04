package catalog_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
)

var now = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

type env struct {
	Svc   *catalog.Service
	Pool  *pgxpool.Pool
	Admin uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testutil.Postgres(t)
	clk := &clock.Fixed{T: now}
	admin := uuid.Must(uuid.NewV7())
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, role) VALUES ($1, 'admin@lunain.vn', $2)`, admin, string(auth.RoleSuperAdmin))
	require.NoError(t, err)
	return &env{Svc: catalog.NewService(pool, clk), Pool: pool, Admin: admin}
}

func (e *env) currentRate(t *testing.T, st string, at time.Time) (int, error) {
	t.Helper()
	var rate int
	err := db.InTx(context.Background(), e.Pool, func(tx pgx.Tx) error {
		var err error
		rate, err = e.Svc.CurrentCommissionRate(context.Background(), tx, st, at)
		return err
	})
	return rate, err
}

func appCode(t *testing.T, err error) (int, string) {
	t.Helper()
	var ae *httpx.AppError
	require.True(t, errors.As(err, &ae), "not an AppError: %v", err)
	return ae.Status, ae.Code
}

func TestCurrentRate_NoneConfigured(t *testing.T) {
	e := newEnv(t)
	_, err := e.currentRate(t, "attraction_ticket", now)
	require.ErrorIs(t, err, catalog.ErrNoCommissionRate)
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "no_commission_rate", code)
}

func TestCurrentRate_PicksLatestEffective(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, err := e.Svc.CreateCommissionRate(ctx, e.Admin, "attraction_ticket", 1000, nil)
	require.NoError(t, err)
	later := now.Add(24 * time.Hour)
	_, err = e.Svc.CreateCommissionRate(ctx, e.Admin, "attraction_ticket", 1200, &later)
	require.NoError(t, err)

	r, err := e.currentRate(t, "attraction_ticket", now)
	require.NoError(t, err)
	require.Equal(t, 1000, r)
	r, err = e.currentRate(t, "attraction_ticket", later)
	require.NoError(t, err)
	require.Equal(t, 1200, r)
	// Another service type is unaffected.
	_, err = e.currentRate(t, "scheduled_ride", later)
	require.ErrorIs(t, err, catalog.ErrNoCommissionRate)

	hist, err := e.Svc.CommissionHistory(ctx, "attraction_ticket")
	require.NoError(t, err)
	require.Len(t, hist, 2)
	require.Equal(t, 1200, hist[0].RateBps)
	require.Equal(t, 1000, hist[1].RateBps)
}

func TestCreateRate_RejectsBackdated(t *testing.T) {
	e := newEnv(t)
	past := now.Add(-2 * time.Minute)
	_, err := e.Svc.CreateCommissionRate(context.Background(), e.Admin, "attraction_ticket", 1000, &past)
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_commission_rate", code)

	// Within the one-minute tolerance is accepted.
	ok := now.Add(-30 * time.Second)
	_, err = e.Svc.CreateCommissionRate(context.Background(), e.Admin, "attraction_ticket", 1000, &ok)
	require.NoError(t, err)
}

func TestCreateRate_RejectsOutOfRange(t *testing.T) {
	e := newEnv(t)
	for _, bps := range []int{10001, -1} {
		_, err := e.Svc.CreateCommissionRate(context.Background(), e.Admin, "attraction_ticket", bps, nil)
		status, code := appCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, status)
		require.Equal(t, "invalid_commission_rate", code)
	}
	// Boundaries are valid (distinct effective_from to avoid a duplicate).
	for i, bps := range []int{0, 10000} {
		from := now.Add(time.Duration(i) * time.Hour)
		_, err := e.Svc.CreateCommissionRate(context.Background(), e.Admin, "scheduled_ride", bps, &from)
		require.NoError(t, err)
	}
}

func TestCreateRate_DuplicateIsConflict(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, err := e.Svc.CreateCommissionRate(ctx, e.Admin, "attraction_ticket", 1000, nil)
	require.NoError(t, err)
	_, err = e.Svc.CreateCommissionRate(ctx, e.Admin, "attraction_ticket", 1500, nil)
	status, code := appCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "commission_rate_exists", code)

	var n int
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_log WHERE action = 'commission_rate.create'`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestCreateRate_Audited(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cr, err := e.Svc.CreateCommissionRate(ctx, e.Admin, "attraction_ticket", 1000, nil)
	require.NoError(t, err)
	require.Equal(t, now, cr.EffectiveFrom.UTC())

	var n, bps int
	require.NoError(t, e.Pool.QueryRow(ctx,
		`SELECT count(*), max((after->>'rate_bps')::int) FROM admin_audit_log WHERE action = 'commission_rate.create'`).Scan(&n, &bps))
	require.Equal(t, 1, n)
	require.Equal(t, 1000, bps)
}

func TestCreateRate_UnknownServiceType(t *testing.T) {
	e := newEnv(t)
	_, err := e.Svc.CreateCommissionRate(context.Background(), e.Admin, "nope", 1000, nil)
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_service_type", code)
}
