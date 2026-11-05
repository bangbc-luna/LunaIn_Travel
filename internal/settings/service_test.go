package settings_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

type env struct {
	Svc   *settings.Service
	Pool  *pgxpool.Pool
	Admin uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testutil.Postgres(t)
	clk := &clock.Fixed{T: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	admin := uuid.Must(uuid.NewV7())
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, role) VALUES ($1, 'admin@lunain.vn', $2)`, admin, string(auth.RoleSuperAdmin))
	require.NoError(t, err)
	return &env{Svc: settings.NewService(pool, clk), Pool: pool, Admin: admin}
}

func (e *env) auditCount(t *testing.T, action string) int {
	t.Helper()
	var n int
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM admin_audit_log WHERE action = $1`, action).Scan(&n))
	return n
}

func appCode(t *testing.T, err error) (int, string) {
	t.Helper()
	var ae *httpx.AppError
	require.True(t, errors.As(err, &ae), "not an AppError: %v", err)
	return ae.Status, ae.Code
}

func ptr[T any](v T) *T { return &v }

func TestGet_ReturnsSeededDefaults(t *testing.T) {
	e := newEnv(t)
	s, err := e.Svc.Get(context.Background())
	require.NoError(t, err)
	require.Equal(t, 15, s.HoldMinutes)
	require.Equal(t, 5, s.HoldGraceMinutes)
	require.Equal(t, 12, s.RequestResponseHours)
	require.Equal(t, 2, s.RequestPaymentHours)
	require.Equal(t, 1, s.ReleaseDelayDays)
	require.Equal(t, int64(100000), s.MinPayoutAmount)
	require.Equal(t, 14, s.RescheduleWindowDays)
	require.Equal(t, 60, s.RideBookingCutoffMinutes)
}

func TestUpdate_PartialAndAudited(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	before, err := e.Svc.Get(ctx)
	require.NoError(t, err)

	got, err := e.Svc.Update(ctx, e.Admin, settings.Patch{HoldMinutes: ptr(20)})
	require.NoError(t, err)
	want := before
	want.HoldMinutes = 20
	require.Equal(t, want, got)
	again, err := e.Svc.Get(ctx)
	require.NoError(t, err)
	require.Equal(t, want, again)

	require.Equal(t, 1, e.auditCount(t, "platform_settings.update"))
	var b, a int
	var by uuid.UUID
	require.NoError(t, e.Pool.QueryRow(ctx,
		`SELECT (before->>'hold_minutes')::int, (after->>'hold_minutes')::int, actor_user_id
		 FROM admin_audit_log WHERE action = 'platform_settings.update'`).Scan(&b, &a, &by))
	require.Equal(t, 15, b)
	require.Equal(t, 20, a)
	require.Equal(t, e.Admin, by)
}

func TestUpdate_RejectsInvalid(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	before, err := e.Svc.Get(ctx)
	require.NoError(t, err)

	_, err = e.Svc.Update(ctx, e.Admin, settings.Patch{HoldMinutes: ptr(0)})
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_settings", code)
	require.Contains(t, err.Error(), "hold_minutes")

	_, err = e.Svc.Update(ctx, e.Admin, settings.Patch{HoldGraceMinutes: ptr(-1)})
	_, code = appCode(t, err)
	require.Equal(t, "invalid_settings", code)
	require.Contains(t, err.Error(), "hold_grace_minutes")

	after, err := e.Svc.Get(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, 0, e.auditCount(t, "platform_settings.update"))
}

func TestHolidays_AddListRemove(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	day := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)

	require.NoError(t, e.Svc.AddHoliday(ctx, e.Admin, day, "Quốc khánh"))
	status, code := appCode(t, e.Svc.AddHoliday(ctx, e.Admin, day, "Trùng"))
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "holiday_exists", code)

	list, err := e.Svc.ListHolidays(ctx, 2026)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "2026-09-02", list[0].Date)
	require.Equal(t, "Quốc khánh", list[0].Name)
	other, err := e.Svc.ListHolidays(ctx, 2027)
	require.NoError(t, err)
	require.Empty(t, other)

	require.NoError(t, db.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
		ok, err := e.Svc.IsHoliday(ctx, tx, day)
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = e.Svc.IsHoliday(ctx, tx, day.AddDate(0, 0, 1))
		require.NoError(t, err)
		require.False(t, ok)
		return nil
	}))

	require.NoError(t, e.Svc.RemoveHoliday(ctx, e.Admin, day))
	status, _ = appCode(t, e.Svc.RemoveHoliday(ctx, e.Admin, day))
	require.Equal(t, http.StatusNotFound, status)

	require.Equal(t, 1, e.auditCount(t, "holiday.create"))
	require.Equal(t, 1, e.auditCount(t, "holiday.delete"))

	status, code = appCode(t, e.Svc.AddHoliday(ctx, e.Admin, day, "  "))
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_holiday", code)
}

func TestIsHoliday_UsesVietnamDate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	require.NoError(t, e.Svc.AddHoliday(ctx, e.Admin, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), "Quốc khánh"))
	require.NoError(t, db.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
		// 2026-09-01T17:30Z is 2026-09-02 00:30 in Vietnam.
		ok, err := e.Svc.IsHoliday(ctx, tx, time.Date(2026, 9, 1, 17, 30, 0, 0, time.UTC))
		require.NoError(t, err)
		require.True(t, ok)
		// 2026-09-01T16:59Z is 2026-09-01 23:59 in Vietnam.
		ok, err = e.Svc.IsHoliday(ctx, tx, time.Date(2026, 9, 1, 16, 59, 0, 0, time.UTC))
		require.NoError(t, err)
		require.False(t, ok)
		return nil
	}))
}

func TestAddHoliday_NameLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	day := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	status, code := appCode(t, e.Svc.AddHoliday(ctx, e.Admin, day, strings.Repeat("ễ", 101)))
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_holiday", code)
	// Exactly 100 characters after trimming is accepted.
	require.NoError(t, e.Svc.AddHoliday(ctx, e.Admin, day, "  "+strings.Repeat("ễ", 100)+"  "))
	list, err := e.Svc.ListHolidays(ctx, 2026)
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("ễ", 100), list[0].Name)
}

func TestUpdate_RideBookingCutoff(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	got, err := e.Svc.Update(ctx, e.Admin, settings.Patch{RideBookingCutoffMinutes: ptr(0)})
	require.NoError(t, err)
	require.Equal(t, 0, got.RideBookingCutoffMinutes)

	got, err = e.Svc.Update(ctx, e.Admin, settings.Patch{RideBookingCutoffMinutes: ptr(120)})
	require.NoError(t, err)
	require.Equal(t, 120, got.RideBookingCutoffMinutes)
	again, err := e.Svc.Get(ctx)
	require.NoError(t, err)
	require.Equal(t, 120, again.RideBookingCutoffMinutes)

	_, err = e.Svc.Update(ctx, e.Admin, settings.Patch{RideBookingCutoffMinutes: ptr(-1)})
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_settings", code)
	require.Contains(t, err.Error(), "ride_booking_cutoff_minutes")
	again, err = e.Svc.Get(ctx)
	require.NoError(t, err)
	require.Equal(t, 120, again.RideBookingCutoffMinutes)
}
