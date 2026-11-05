package identity

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

func TestSuperAdminEmails_ActiveOnly(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	for _, u := range []struct{ email, role, status string }{
		{"zeta@lunain.vn", "super_admin", "active"},
		{"alpha@lunain.vn", "super_admin", "active"},
		{"locked@lunain.vn", "super_admin", "locked"},
		{"partner@alpha.vn", "partner", "active"},
		{"khach@gmail.com", "customer", "active"},
	} {
		_, err := env.Pool.Exec(ctx, `INSERT INTO users (id, email, role, status) VALUES ($1, $2, $3, $4)`,
			uuid.Must(uuid.NewV7()), u.email, u.role, u.status)
		require.NoError(t, err)
	}
	var got []string
	require.NoError(t, db.InTx(ctx, env.Pool, func(tx pgx.Tx) error {
		var err error
		got, err = env.Svc.SuperAdminEmails(ctx, tx)
		return err
	}))
	require.Equal(t, []string{"alpha@lunain.vn", "zeta@lunain.vn"}, got)
}

func TestEmailOf(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7())
	_, err := env.Pool.Exec(ctx, `INSERT INTO users (id, email, role) VALUES ($1, 'khach@gmail.com', 'customer')`, id)
	require.NoError(t, err)
	require.NoError(t, db.InTx(ctx, env.Pool, func(tx pgx.Tx) error {
		got, err := env.Svc.EmailOf(ctx, tx, id)
		require.NoError(t, err)
		require.Equal(t, "khach@gmail.com", got)
		_, err = env.Svc.EmailOf(ctx, tx, uuid.Must(uuid.NewV7()))
		require.Error(t, err)
		return nil
	}))
}
