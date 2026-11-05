package payout

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/jobs"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
	"github.com/bangbc-luna/lunain-travel/internal/platform/storage"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

var testNow = time.Date(2026, 11, 5, 9, 0, 0, 0, time.FixedZone("ICT", 7*3600))

// env wires real identity, partner, settings, ledger and payout services on
// a fresh database with a fixed clock.
type env struct {
	pool     *pgxpool.Pool
	clock    *clock.Fixed
	issuer   *auth.Issuer
	svc      *Service
	ledger   *ledger.Service
	partners *partner.Service
	settings *settings.Service
	files    *files.Service
	identity *identity.Service
	admin    uuid.UUID
	customer uuid.UUID
}

// testPartner is an active partner with a listing and a checkout to hang
// SQL-built bookings on.
type testPartner struct {
	ID, Owner, listing, checkout uuid.UUID
	bank                         *partner.BankAccount
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testutil.Postgres(t)
	clk := &clock.Fixed{T: testNow}
	issuer := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk)
	idSvc := identity.NewService(identity.Deps{
		Pool: pool, Issuer: issuer, Clock: clk,
		CodePepper: []byte("test-pepper"), RefreshTTL: 30 * 24 * time.Hour,
	})
	fileSvc := files.NewService(pool, storage.NewMemory(), clk, "http://files.test")
	partners := partner.NewService(partner.Deps{
		Pool: pool, Clock: clk, Users: idSvc, Catalog: catalog.NewService(pool, clk),
		Geo: geo.NewService(pool), Files: fileSvc,
	})
	led := ledger.NewService(pool, clk)
	set := settings.NewService(pool, clk)
	// Never started: emails stay queued in river_job, where tests count them.
	workers := river.NewWorkers()
	river.AddWorker(workers, &mailer.SendEmailWorker{Mailer: &mailer.Recorder{}})
	jobClient, err := jobs.NewClient(pool, workers, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	e := &env{
		pool: pool, clock: clk, issuer: issuer, ledger: led, partners: partners, settings: set,
		files: fileSvc, identity: idSvc,
		svc: NewService(Deps{
			Pool: pool, Clock: clk, Ledger: led, Partners: partners, Settings: set, Files: fileSvc,
			Jobs: jobClient, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}),
	}
	ctx := context.Background()
	require.NoError(t, db.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		e.admin, err = idSvc.CreateInternalUser(ctx, tx, "admin@lunain.vn", "adminpass123", auth.RoleSuperAdmin)
		return err
	}))
	e.customer = uuid.Must(uuid.NewV7())
	e.mustExec(t, `INSERT INTO users (id, email, role) VALUES ($1, 'khach@customer.test', 'customer')`, e.customer)
	return e
}

func (e *env) mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := e.pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

func (e *env) inTx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, db.InTx(ctx, e.pool, func(tx pgx.Tx) error { return fn(ctx, tx) }))
}

// newPartner creates an active partner through the partner service; withBank
// also sets its current bank account.
func (e *env) newPartner(t *testing.T, key string, withBank bool) testPartner {
	t.Helper()
	ctx := context.Background()
	p, err := e.partners.Create(ctx, e.admin, partner.CreateInput{
		LegalName: "Công ty " + key, DisplayName: key, ContactName: "Nguyễn Văn An",
		ContactPhone: "0901234567", ContactEmail: "lienhe@" + key + ".vn",
		OwnerEmail: "owner@" + key + ".vn", OwnerPassword: "matkhau123",
		ServiceTypes: []string{"attraction_ticket"},
	})
	require.NoError(t, err)
	tp := testPartner{ID: p.ID, Owner: p.OwnerUserID, listing: uuid.Must(uuid.NewV7()), checkout: uuid.Must(uuid.NewV7())}
	if withBank {
		ba, err := e.partners.SetBankAccount(ctx, p.ID, p.OwnerUserID, partner.BankAccountInput{
			BankBIN: "970436", BankName: "Vietcombank", AccountNumber: "0011001234567", AccountHolder: "Nguyễn Văn An",
		})
		require.NoError(t, err)
		tp.bank = &ba
	}
	e.mustExec(t, `INSERT INTO listings (id, partner_id, service_type, title, slug)
		VALUES ($1, $2, 'attraction_ticket', 'Vé tham quan', $3)`, tp.listing, tp.ID, "slug-"+tp.listing.String())
	e.mustExec(t, `INSERT INTO checkouts (id, code, customer_id, total_amount, status)
		VALUES ($1, $2, $3, 0, 'open')`, tp.checkout, randCode(t, "CO"), e.customer)
	return tp
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func randCode(t *testing.T, prefix string) string {
	t.Helper()
	var b [8]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	out := []byte(prefix)
	for _, c := range b {
		out = append(out, crockford[int(c)%len(crockford)])
	}
	return string(out)
}

func randToken(t *testing.T) string {
	t.Helper()
	var b [32]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// confirmBooking inserts a confirmed booking of p (raw SQL, like the ledger
// helpers) and posts its earning: net goes to pending.
func (e *env) confirmBooking(t *testing.T, p testPartner, net int64) ledger.BookingMoney {
	t.Helper()
	return e.confirmBookingWithCommission(t, p, net, 0)
}

// confirmBookingWithCommission is confirmBooking of a booking of gross with
// the platform commission taken: net gross − commission goes to pending.
func (e *env) confirmBookingWithCommission(t *testing.T, p testPartner, gross, commission int64) ledger.BookingMoney {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.mustExec(t, `INSERT INTO bookings (
		id, code, checkout_id, customer_id, partner_id, listing_id, service_type, booking_mode, status,
		service_start_at, service_end_at, original_service_start_at,
		gross_amount, commission_rate_bps, commission_amount, cancellation_policy_snapshot,
		contact_name, contact_phone, contact_email, confirmed_at, qr_token
	) VALUES (
		$1, $2, $3, $4, $5, $6, 'attraction_ticket', 'instant', 'confirmed',
		'2026-12-01 02:00:00Z', '2026-12-01 16:59:59Z', '2026-12-01 02:00:00Z',
		$7, 0, $9, '[]',
		'Nguyễn Văn A', '0900000000', 'khach@customer.test', now(), $8
	)`, id, randCode(t, "LT"), p.checkout, e.customer, p.ID, p.listing, gross, randToken(t), commission)
	b := ledger.BookingMoney{
		BookingID: id, PartnerID: p.ID, ServiceType: "attraction_ticket", Gross: gross, Commission: commission,
	}
	e.inTx(t, func(ctx context.Context, tx pgx.Tx) error { return e.ledger.PostBookingConfirmed(ctx, tx, b) })
	return b
}

// releaseTx releases b inside tx, holding the booking row FOR UPDATE first
// as the booking flows do.
func (e *env) releaseTx(ctx context.Context, tx pgx.Tx, b ledger.BookingMoney) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM bookings WHERE id = $1 FOR UPDATE`, b.BookingID); err != nil {
		return err
	}
	return e.ledger.Release(ctx, tx, b.BookingID)
}

func (e *env) release(t *testing.T, b ledger.BookingMoney) {
	t.Helper()
	e.inTx(t, func(ctx context.Context, tx pgx.Tx) error { return e.releaseTx(ctx, tx, b) })
}

// fund brings net into p's available balance (confirm, then release).
func (e *env) fund(t *testing.T, p testPartner, net int64) ledger.BookingMoney {
	t.Helper()
	b := e.confirmBooking(t, p, net)
	e.release(t, b)
	return b
}

// fundedPartner is an active partner with a bank account and 900.000 đ
// available.
func (e *env) fundedPartner(t *testing.T, key string) testPartner {
	t.Helper()
	p := e.newPartner(t, key, true)
	e.fund(t, p, 900_000)
	e.requireAvailable(t, p, 900_000)
	return p
}

func (e *env) requireAvailable(t *testing.T, p testPartner, want int64) {
	t.Helper()
	b, err := e.ledger.Balance(context.Background(), p.ID)
	require.NoError(t, err)
	require.Equal(t, want, b.Available)
	require.NoError(t, e.ledger.Verify(context.Background(), p.ID))
}

// reject rejects an open payout the way the admin flow will (Task 4): lock
// the payout, mark it rejected and post the reversal, in one tx.
func (e *env) reject(t *testing.T, v PayoutView) {
	t.Helper()
	e.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM payouts WHERE id = $1 FOR UPDATE`, v.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payouts SET status = 'rejected', rejected_at = now(), rejected_by = $2,
			reject_reason = 'sai thông tin', updated_at = now() WHERE id = $1`, v.ID, e.admin); err != nil {
			return err
		}
		_, err := e.ledger.PostPayoutReversal(ctx, tx, v.PartnerID, v.ID, v.Amount)
		return err
	})
}

// requireBalanced checks a statement: opening + Σ lines = closing and every
// line lies inside [seq_from, seq_to].
func requireBalanced(t *testing.T, v PayoutView) {
	t.Helper()
	sum := v.OpeningAvailable
	for _, l := range v.Statement {
		require.GreaterOrEqual(t, l.Seq, v.StatementSeqFrom)
		require.LessOrEqual(t, l.Seq, v.StatementSeqTo)
		sum += l.Amount
	}
	require.Equal(t, v.ClosingAvailable, sum)
}

func appCode(t *testing.T, err error) (int, string) {
	t.Helper()
	var ae *httpx.AppError
	require.True(t, errors.As(err, &ae), "not an AppError: %v", err)
	return ae.Status, ae.Code
}

func requireCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	gotStatus, gotCode := appCode(t, err)
	require.Equal(t, status, gotStatus, "%v", err)
	require.Equal(t, code, gotCode, "%v", err)
}

func (e *env) token(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	s, _, err := e.issuer.Issue(auth.Principal{UserID: userID, Role: auth.RolePartner})
	require.NoError(t, err)
	return s
}

func (e *env) adminToken(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	s, _, err := e.issuer.Issue(auth.Principal{UserID: userID, Role: auth.RoleSuperAdmin})
	require.NoError(t, err)
	return s
}

// server serves the partner and admin payout routes the way cmd/api wires
// them.
func (e *env) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	e.svc.PartnerRoutes(mux, e.issuer, e.partners.RequireMember())
	e.svc.AdminRoutes(mux, e.issuer)
	mux.HandleFunc("/", httpx.NotFound)
	srv := httptest.NewServer(httpx.Chain(mux, httpx.RequestID, httpx.Recover))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, tok, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, b
}
