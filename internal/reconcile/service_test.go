package reconcile_test

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/reconcile"
)

func (e *env) alerts(t *testing.T) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email' AND args->>'to' = $1
		AND args->>'subject' LIKE '[LunaIn] Đối soát %'`, adminEmail)
}

func TestRun_OkNoEmail(t *testing.T) {
	e := newEnv(t)
	r, err := e.Svc.Run(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, "ok", r.Status)
	require.Equal(t, "2026-11-05", r.RunDate)
	require.Empty(t, r.Mismatches)
	require.Nil(t, r.RunBy)
	require.Equal(t, 0, e.alerts(t))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_reconciliations WHERE run_date = '2026-11-05' AND status = 'ok'`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM admin_audit_log WHERE action = 'reconciliation.run'`))
}

func TestRun_BalanceDriftAlerts(t *testing.T) {
	e := newEnv(t)
	e.breakBalance(t)
	r, err := e.Svc.Run(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, "mismatch", r.Status)
	require.Len(t, r.Mismatches, 1)
	m := r.Mismatches[0]
	require.Equal(t, "balance_drift", m.Kind)
	require.Equal(t, e.PartnerID, *m.PartnerID)
	require.NotEmpty(t, m.Detail)
	require.Equal(t, 1, e.alerts(t))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'subject' = '[LunaIn] Đối soát 2026-11-05: 1 chênh lệch'
		AND args->>'text' LIKE '%balance_drift%'`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_reconciliations
		WHERE status = 'mismatch' AND mismatches->0->>'kind' = 'balance_drift'
		AND mismatches->0->>'partner_id' = $1`, e.PartnerID.String()))
}

// Two runs on one Vietnam day keep one row, holding the latest result.
func TestRun_SameDayUpserts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first, err := e.Svc.Run(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, "ok", first.Status)

	e.breakBalance(t)
	e.Clock.T = testNow.Add(20 * time.Hour) // 22:30 the same day in Vietnam
	second, err := e.Svc.Run(ctx, &e.Admin)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, "mismatch", second.Status)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_reconciliations`))

	e.Clock.T = testNow.Add(22 * time.Hour) // 00:30 the next day
	third, err := e.Svc.Run(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, "2026-11-06", third.RunDate)
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM payment_reconciliations`))

	got, err := e.Payments.GetReconciliation(ctx, time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, "mismatch", got.Status)
	list, err := e.Payments.ListReconciliations(ctx, 10, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"2026-11-06", "2026-11-05"}, []string{list[0].RunDate, list[1].RunDate})
	before := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	list, err = e.Payments.ListReconciliations(ctx, 10, &before)
	require.NoError(t, err)
	require.Len(t, list, 1)
	_, err = e.Payments.GetReconciliation(ctx, time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC))
	require.ErrorIs(t, err, payment.ErrReconciliationNotFound)
}

func TestRun_ManualRunAudited(t *testing.T) {
	e := newEnv(t)
	e.breakBalance(t)
	r, err := e.Svc.Run(context.Background(), &e.Admin)
	require.NoError(t, err)
	require.Equal(t, e.Admin, *r.RunBy)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM admin_audit_log
		WHERE action = 'reconciliation.run' AND actor_user_id = $1 AND target_id = $2
		AND target_kind = 'payment_reconciliation'`, e.Admin, r.ID))
	require.Equal(t, 1, e.alerts(t))
}

// The job's context expires while the provider hangs: the run stops calling
// it, but the result is still stored and the admins alerted.
func TestRun_TimeoutStillSavedAndAlerted(t *testing.T) {
	e := newEnv(t)
	e.book(t, "a")
	e.book(t, "b")
	e.Fake.SetStatusDelay(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r, err := e.Svc.Run(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, "mismatch", r.Status)
	require.Len(t, r.Mismatches, 1)
	require.Equal(t, "provider_error", r.Mismatches[0].Kind)
	require.Contains(t, r.Mismatches[0].Detail, "timeout/cancelled after 0/2 payments")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_reconciliations
		WHERE run_date = '2026-11-05' AND mismatches->0->>'kind' = 'provider_error'`))
	require.Equal(t, 1, e.alerts(t))
}

// The daily job skips quietly while another run holds the lock.
func TestDailyWorker_SkipsWhenLocked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	conn, err := e.Pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, reconcile.RunLockKey)
	require.NoError(t, err)

	_, err = e.Svc.Run(ctx, &e.Admin)
	require.ErrorIs(t, err, reconcile.ErrRunning)
	w := &reconcile.DailyWorker{Service: e.Svc}
	require.NoError(t, w.Work(ctx, &river.Job[reconcile.DailyArgs]{}))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM payment_reconciliations`))
	require.GreaterOrEqual(t, w.Timeout(nil), 15*time.Minute, "longer than River's 1 minute default")

	_, err = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, reconcile.RunLockKey)
	require.NoError(t, err)
	require.NoError(t, w.Work(ctx, &river.Job[reconcile.DailyArgs]{}))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_reconciliations`))
}
