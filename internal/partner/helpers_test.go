package partner_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/storage"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
)

var testNow = time.Date(2026, 11, 4, 9, 0, 0, 0, time.FixedZone("ICT", 7*3600))

const ownerPassword = "matkhau123"

// env wires real identity, catalog, geo, files and partner services on a
// fresh Postgres database with a fixed clock.
type env struct {
	Svc      *partner.Service
	Identity *identity.Service
	Catalog  *catalog.Service
	Geo      *geo.Service
	Files    *files.Service
	Pool     *pgxpool.Pool
	Clock    *clock.Fixed
	Admin    uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testutil.Postgres(t)
	clk := &clock.Fixed{T: testNow}
	idSvc := identity.NewService(identity.Deps{
		Pool:       pool,
		Issuer:     auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk),
		Clock:      clk,
		CodePepper: []byte("test-pepper"),
		RefreshTTL: 30 * 24 * time.Hour,
	})
	cat := catalog.NewService(pool, clk)
	geoSvc := geo.NewService(pool)
	fileSvc := files.NewService(pool, storage.NewMemory(), clk, "http://files.test")
	e := &env{
		Svc: partner.NewService(partner.Deps{
			Pool: pool, Clock: clk, Users: idSvc, Catalog: cat, Geo: geoSvc, Files: fileSvc,
		}),
		Identity: idSvc, Catalog: cat, Geo: geoSvc, Files: fileSvc,
		Pool: pool, Clock: clk,
	}
	e.Admin = e.superAdmin(t, "admin@lunain.vn")
	return e
}

// superAdmin creates a verified super_admin account.
func (e *env) superAdmin(t *testing.T, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.InTx(context.Background(), e.Pool, func(tx pgx.Tx) error {
		var err error
		id, err = e.Identity.CreateInternalUser(context.Background(), tx, email, "adminpass123", auth.RoleSuperAdmin)
		return err
	}))
	return id
}

// input returns a valid CreateInput whose names and owner email derive from n.
func input(n string, serviceTypes ...string) partner.CreateInput {
	key := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, strings.ToLower(n))
	return partner.CreateInput{
		LegalName:     "Công ty " + n,
		DisplayName:   n,
		ContactName:   "Nguyễn Văn An",
		ContactPhone:  "0901234567",
		ContactEmail:  "lienhe@" + key + ".vn",
		OwnerEmail:    "owner@" + key + ".vn",
		OwnerPassword: ownerPassword,
		ServiceTypes:  serviceTypes,
	}
}

func (e *env) create(t *testing.T, in partner.CreateInput) partner.Partner {
	t.Helper()
	p, err := e.Svc.Create(context.Background(), e.Admin, in)
	require.NoError(t, err)
	return p
}

func (e *env) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	require.NoError(t, e.Pool.QueryRow(context.Background(), fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n))
	return n
}

// counts snapshots every table Create writes to.
func (e *env) counts(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"users", "partners", "partner_members", "partner_service_types", "admin_audit_log"} {
		out[tbl] = e.count(t, tbl)
	}
	return out
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
