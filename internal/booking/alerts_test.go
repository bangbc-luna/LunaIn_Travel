package booking_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	require.NoError(t, err)
	return id
}

func (e *env) count64(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, e.Pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

// adminAlerts counts the queued emails to super admins (any status).
func (e *env) adminAlerts(t *testing.T) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' IN (SELECT email FROM users WHERE role = 'super_admin')`)
}

// superAdmins adds an active and a locked super admin to the fixture's one.
func (e *env) superAdmins(t *testing.T) {
	t.Helper()
	e.exec(t, `INSERT INTO users (id, email, role, status) VALUES ($1, 'ops@lunain.vn', 'super_admin', 'active')`,
		uuid.Must(uuid.NewV7()))
	e.exec(t, `INSERT INTO users (id, email, role, status) VALUES ($1, 'old@lunain.vn', 'super_admin', 'locked')`,
		uuid.Must(uuid.NewV7()))
}

// M-3: a mismatch alerts every active super admin, in the webhook's tx.
func TestUnderpaidAlertsSuperAdmins(t *testing.T) {
	e := newEnv(t)
	e.superAdmins(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, 300_000)

	require.Equal(t, 2, e.adminAlerts(t))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM river_job WHERE args->>'to' = 'old@lunain.vn'`))
	co := e.str(t, `SELECT c.code FROM checkouts c JOIN bookings b ON b.checkout_id = c.id WHERE b.id = $1`, v.ID)
	text := e.str(t, `SELECT args->>'text' FROM river_job WHERE kind = 'send_email' AND args->>'to' = 'ops@lunain.vn'`)
	require.Contains(t, text, co)
	require.Contains(t, text, "300.000 đ")
	require.Contains(t, text, "380.000 đ")
	require.Contains(t, text, "underpaid")
	require.Contains(t, text, strconv.FormatInt(oc, 10))
}

func TestOverpaidAndDuplicateAlert(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, 400_000)
	require.Equal(t, 1, e.adminAlerts(t))
	rec := e.webhook(t, e.Fake.MarkPaidRef(oc, gross, "SECOND"))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, 2, e.adminAlerts(t))
}

func TestConfirmedNoAlert(t *testing.T) {
	e := newEnv(t)
	e.superAdmins(t)
	v := e.create(t, "k")
	e.pay(t, e.orderCode(t, v.ID), gross)
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, 0, e.adminAlerts(t))
}
