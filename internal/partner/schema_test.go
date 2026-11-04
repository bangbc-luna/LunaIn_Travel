package partner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

func pgCode(t *testing.T, err error) string {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "not a PgError: %v", err)
	return pgErr.Code
}

// TestSchema_PartnerLocationAndSuspensionChecks pins the partners CHECKs and
// the composite (ward_code, province_code) FK: the DB itself rejects
// inconsistent rows that bypass the service.
func TestSchema_PartnerLocationAndSuspensionChecks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.create(t, input("alpha"))

	// A ward and a province it does not belong to.
	var wardA, provA, provB string
	require.NoError(t, e.Pool.QueryRow(ctx,
		`SELECT code, province_code FROM wards ORDER BY code LIMIT 1`).Scan(&wardA, &provA))
	require.NoError(t, e.Pool.QueryRow(ctx,
		`SELECT code FROM provinces WHERE code <> $1 ORDER BY code LIMIT 1`, provA).Scan(&provB))

	exec := func(sql string, args ...any) error {
		_, err := e.Pool.Exec(ctx, sql, args...)
		return err
	}

	// Ward of another province → FK violation.
	err := exec(`UPDATE partners SET province_code = $2, ward_code = $3 WHERE id = $1`, p.ID, provB, wardA)
	require.Equal(t, "23503", pgCode(t, err))
	// Province without ward (and vice versa) → CHECK violation.
	err = exec(`UPDATE partners SET province_code = $2, ward_code = NULL WHERE id = $1`, p.ID, provA)
	require.Equal(t, "23514", pgCode(t, err))
	err = exec(`UPDATE partners SET province_code = NULL, ward_code = $2 WHERE id = $1`, p.ID, wardA)
	require.Equal(t, "23514", pgCode(t, err))
	// The matching pair is accepted.
	require.NoError(t, exec(`UPDATE partners SET province_code = $2, ward_code = $3 WHERE id = $1`, p.ID, provA, wardA))

	// Active partner with suspended_at set → CHECK violation.
	err = exec(`UPDATE partners SET suspended_at = now() WHERE id = $1`, p.ID)
	require.Equal(t, "23514", pgCode(t, err))
	// Suspended without suspended_at, or without a reason → CHECK violation.
	err = exec(`UPDATE partners SET status = 'suspended', suspended_reason = 'x' WHERE id = $1`, p.ID)
	require.Equal(t, "23514", pgCode(t, err))
	err = exec(`UPDATE partners SET status = 'suspended', suspended_at = now() WHERE id = $1`, p.ID)
	require.Equal(t, "23514", pgCode(t, err))

	// Suspend then Unsuspend keeps the row valid and clears both fields.
	require.NoError(t, e.Svc.Suspend(ctx, e.Admin, p.ID, "Vi phạm"))
	require.NoError(t, e.Svc.Unsuspend(ctx, e.Admin, p.ID))
	got, err := e.Svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "active", got.Status)
	require.Nil(t, got.SuspendedAt)
	require.Nil(t, got.SuspendedReason)
}
