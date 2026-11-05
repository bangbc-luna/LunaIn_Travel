package reconcile_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/jobs"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
	"github.com/bangbc-luna/lunain-travel/internal/platform/storage"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
	"github.com/bangbc-luna/lunain-travel/internal/reconcile"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

// testNow is 2026-11-05 02:30 in Vietnam (2026-11-04 19:30 UTC).
var testNow = time.Date(2026, 11, 5, 2, 30, 0, 0, time.FixedZone("ICT", 7*3600))

const adminEmail = "admin@lunain.vn"

type env struct {
	Pool      *pgxpool.Pool
	Clock     *clock.Fixed
	Issuer    *auth.Issuer
	Fake      *payment.Fake
	Payments  *payment.Service
	Svc       *reconcile.Service
	Handler   http.Handler
	Admin     uuid.UUID
	PartnerID uuid.UUID
	Owner     uuid.UUID
}

// newEnv wires the real services with payment.Fake, a fixed clock and a
// never-started job client (emails stay in river_job).
func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	pool := testutil.Postgres(t)
	clk := &clock.Fixed{T: testNow}
	iss := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk)
	log := slog.New(slog.DiscardHandler)
	idSvc := identity.NewService(identity.Deps{
		Pool: pool, Issuer: iss, Clock: clk, CodePepper: []byte("test-pepper"), RefreshTTL: 30 * 24 * time.Hour,
	})
	cat := catalog.NewService(pool, clk)
	geoSvc := geo.NewService(pool)
	fileSvc := files.NewService(pool, storage.NewMemory(), clk, "http://files.test")
	pSvc := partner.NewService(partner.Deps{Pool: pool, Clock: clk, Users: idSvc, Catalog: cat, Geo: geoSvc, Files: fileSvc})
	lSvc := listing.NewService(listing.Deps{
		Pool: pool, Clock: clk, Partners: pSvc, Catalog: cat, Geo: geoSvc, Files: fileSvc, BookingGuard: booking.Guard{},
	})
	fake := payment.NewFake()
	fake.Now = clk.Now
	paySvc := payment.NewService(pool, clk, fake, log)
	ledgerSvc := ledger.NewService(pool, clk)
	workers := river.NewWorkers()
	river.AddWorker(workers, &mailer.SendEmailWorker{Mailer: &mailer.Recorder{}})
	jobClient, err := jobs.NewClient(pool, workers, log)
	require.NoError(t, err)
	paySvc.SetOps(payment.OpsDeps{Jobs: jobClient, Users: idSvc, Files: fileSvc})
	bSvc := booking.NewService(booking.Deps{
		Pool: pool, Clock: clk, Listings: lSvc, Catalog: cat, Settings: settings.NewService(pool, clk),
		Identity: idSvc, Payments: paySvc, Ledger: ledgerSvc, Jobs: jobClient, Log: log,
	})
	svc := reconcile.NewService(reconcile.Deps{
		Pool: pool, Clock: clk, Payments: paySvc, Ledger: ledgerSvc, Handler: bSvc,
		Identity: idSvc, Jobs: jobClient, Log: log,
	})
	mux := http.NewServeMux()
	svc.AdminRoutes(mux, iss)
	mux.HandleFunc("/", httpx.NotFound)
	e := &env{
		Pool: pool, Clock: clk, Issuer: iss, Fake: fake, Payments: paySvc, Svc: svc,
		Handler: httpx.Chain(mux, httpx.RequestID, httpx.Recover),
	}
	require.NoError(t, db.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		e.Admin, err = idSvc.CreateInternalUser(ctx, tx, adminEmail, "adminpass123", auth.RoleSuperAdmin)
		return err
	}))
	p, err := pSvc.Create(ctx, e.Admin, partner.CreateInput{
		LegalName: "Công ty Alpha", DisplayName: "Alpha", OwnerEmail: "owner@alpha.vn", OwnerPassword: "matkhau123",
		ServiceTypes: []string{"attraction_ticket"},
	})
	require.NoError(t, err)
	e.PartnerID, e.Owner = p.ID, p.OwnerUserID
	return e
}

func (e *env) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	_, err := e.Pool.Exec(context.Background(), q, args...)
	require.NoError(t, err)
}

func (e *env) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.Pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func (e *env) token(t *testing.T, userID uuid.UUID, role auth.Role) string {
	t.Helper()
	s, _, err := e.Issuer.Issue(auth.Principal{UserID: userID, Role: role})
	require.NoError(t, err)
	return s
}

func (e *env) do(t *testing.T, method, path, tok, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	e.Handler.ServeHTTP(rec, req)
	return rec
}

// breakBalance makes the partner's cached balance disagree with its ledger.
func (e *env) breakBalance(t *testing.T) {
	t.Helper()
	e.exec(t, `INSERT INTO partner_balances (partner_id, available) VALUES ($1, 5)
		ON CONFLICT (partner_id) DO UPDATE SET available = partner_balances.available + 5`, e.PartnerID)
}
