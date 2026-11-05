package reconcile_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

type recJSON struct {
	ID              string `json:"id"`
	RunDate         string `json:"run_date"`
	Status          string `json:"status"`
	CheckedPayments int    `json:"checked_payments"`
	Mismatches      []struct {
		Kind      string  `json:"kind"`
		PartnerID *string `json:"partner_id"`
	} `json:"mismatches"`
	RunBy *string `json:"run_by"`
}

func TestHTTP_RunAndGet(t *testing.T) {
	e := newEnv(t)
	tok := e.token(t, e.Admin, auth.RoleSuperAdmin)
	e.breakBalance(t)

	rec := e.do(t, http.MethodPost, "/v1/admin/reconciliations/run", tok, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var run recJSON
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &run))
	require.Equal(t, "2026-11-05", run.RunDate)
	require.Equal(t, "mismatch", run.Status)
	require.Len(t, run.Mismatches, 1)
	require.Equal(t, "balance_drift", run.Mismatches[0].Kind)
	require.Equal(t, e.Admin.String(), *run.RunBy)

	rec = e.do(t, http.MethodGet, "/v1/admin/reconciliations/2026-11-05", tok, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got recJSON
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, run, got)

	e.Clock.T = testNow.Add(24 * time.Hour)
	tok = e.token(t, e.Admin, auth.RoleSuperAdmin) // the first one expired
	rec = e.do(t, http.MethodPost, "/v1/admin/reconciliations/run", tok, "{}")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = e.do(t, http.MethodGet, "/v1/admin/reconciliations", tok, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list struct {
		Items []recJSON `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Items, 2)
	require.Equal(t, "2026-11-06", list.Items[0].RunDate)

	rec = e.do(t, http.MethodGet, "/v1/admin/reconciliations?limit=1&before=2026-11-06", tok, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Items, 1)
	require.Equal(t, "2026-11-05", list.Items[0].RunDate)

	for _, q := range []string{"?limit=0", "?limit=x", "?before=2026-13-01", "?before=yesterday"} {
		rec = e.do(t, http.MethodGet, "/v1/admin/reconciliations"+q, tok, "")
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", q, rec.Body.String())
	}
	rec = e.do(t, http.MethodPost, "/v1/admin/reconciliations/run", tok, `{"x":1}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestHTTP_BadRunDate404(t *testing.T) {
	e := newEnv(t)
	tok := e.token(t, e.Admin, auth.RoleSuperAdmin)
	for _, d := range []string{"2026-11-05", "2026-02-30", "05-11-2026", "x"} {
		rec := e.do(t, http.MethodGet, "/v1/admin/reconciliations/"+d, tok, "")
		require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", d, rec.Body.String())
	}
}

func TestHTTP_PartnerRole403(t *testing.T) {
	e := newEnv(t)
	tok := e.token(t, e.Owner, auth.RolePartner)
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/v1/admin/reconciliations"},
		{http.MethodGet, "/v1/admin/reconciliations/2026-11-05"},
		{http.MethodPost, "/v1/admin/reconciliations/run"},
	} {
		rec := e.do(t, r.method, r.path, tok, "")
		require.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", r.method, r.path, rec.Body.String())
		require.Equal(t, http.StatusUnauthorized, e.do(t, r.method, r.path, "", "").Code)
	}
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM payment_reconciliations`))
}

// A manual run while another one is running answers 409; only the first
// one stores a result and emails the admins.
func TestHTTP_ConcurrentRunsConflict(t *testing.T) {
	e := newEnv(t)
	tok := e.token(t, e.Admin, auth.RoleSuperAdmin)
	e.book(t, "a")
	e.breakBalance(t)
	e.Fake.SetStatusDelay(time.Second) // keeps the first run busy

	first := make(chan int, 1)
	go func() {
		first <- e.do(t, http.MethodPost, "/v1/admin/reconciliations/run", tok, "").Code
	}()
	require.Eventually(t, func() bool {
		return e.count(t, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`) == 1
	}, 5*time.Second, 10*time.Millisecond, "the first run holds the lock")

	rec := e.do(t, http.MethodPost, "/v1/admin/reconciliations/run", tok, "")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"reconciliation_running"`)
	require.Equal(t, http.StatusOK, <-first)

	require.Equal(t, 1, e.alerts(t), "one set of alert emails")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM admin_audit_log WHERE action = 'reconciliation.run'`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`), "lock released")
}
