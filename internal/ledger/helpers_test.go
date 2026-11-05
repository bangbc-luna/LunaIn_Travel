package ledger_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
)

var testNow = time.Date(2026, 11, 5, 9, 0, 0, 0, time.FixedZone("ICT", 7*3600))

const attraction = "attraction_ticket"

// fixture holds the minimal rows ledger entries need: the composite FK
// (booking_id, partner_id) -> bookings requires real bookings of a real
// partner. Rows are inserted with raw SQL so this package does not depend on
// the booking module.
type fixture struct {
	pool     *pgxpool.Pool
	svc      *ledger.Service
	clock    *clock.Fixed
	customer uuid.UUID
	owner    uuid.UUID
	partner  uuid.UUID
	listing  uuid.UUID
	checkout uuid.UUID
}

func newID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	require.NoError(t, err)
	return id
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

func (f *fixture) mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := &clock.Fixed{T: testNow}
	pool := testutil.Postgres(t)
	f := &fixture{pool: pool, svc: ledger.NewService(pool, clk), clock: clk}
	f.customer = newID(t)
	f.mustExec(t, `INSERT INTO users (id, email, role) VALUES ($1, 'a@customer.test', 'customer')`, f.customer)
	f.owner, f.partner = f.newPartner(t, "alpha")
	f.listing = f.newListing(t, f.partner)
	f.checkout = newID(t)
	f.mustExec(t, `INSERT INTO checkouts (id, code, customer_id, total_amount, status)
		VALUES ($1, $2, $3, 0, 'open')`, f.checkout, randCode(t, "CO"), f.customer)
	return f
}

// newPartner inserts an owner user (member of the partner) and a partner.
func (f *fixture) newPartner(t *testing.T, name string) (owner, partner uuid.UUID) {
	t.Helper()
	owner, partner = newID(t), newID(t)
	f.mustExec(t, `INSERT INTO users (id, email, role) VALUES ($1, $2, 'partner')`, owner, name+"@partner.test")
	f.mustExec(t, `INSERT INTO partners (id, legal_name, display_name, created_by) VALUES ($1, $2, $2, $3)`,
		partner, name, owner)
	f.mustExec(t, `INSERT INTO partner_members (partner_id, user_id, role) VALUES ($1, $2, 'owner')`, partner, owner)
	return owner, partner
}

func (f *fixture) newListing(t *testing.T, partner uuid.UUID) uuid.UUID {
	t.Helper()
	id := newID(t)
	f.mustExec(t, `INSERT INTO listings (id, partner_id, service_type, title, slug)
		VALUES ($1, $2, 'attraction_ticket', 'Vé tham quan', $3)`, id, partner, "slug-"+id.String())
	return id
}

// newBooking inserts a confirmed instant booking of the fixture's partner and
// returns the money the ledger is told about.
func (f *fixture) newBooking(t *testing.T, gross, commission int64) ledger.BookingMoney {
	t.Helper()
	return f.newBookingOf(t, f.partner, f.listing, gross, commission)
}

// newBookingOf is newBooking for any partner and one of its listings.
func (f *fixture) newBookingOf(t *testing.T, partner, listing uuid.UUID, gross, commission int64) ledger.BookingMoney {
	t.Helper()
	id := newID(t)
	bps := 0
	if gross > 0 {
		bps = int(commission * 10000 / gross)
	}
	f.mustExec(t, `INSERT INTO bookings (
		id, code, checkout_id, customer_id, partner_id, listing_id, service_type, booking_mode, status,
		service_start_at, service_end_at, original_service_start_at,
		gross_amount, commission_rate_bps, commission_amount, cancellation_policy_snapshot,
		contact_name, contact_phone, contact_email, confirmed_at, qr_token
	) VALUES (
		$1, $2, $3, $4, $5, $6, 'attraction_ticket', 'instant', 'confirmed',
		'2026-12-01 02:00:00Z', '2026-12-01 16:59:59Z', '2026-12-01 02:00:00Z',
		$7, $8, $9, '[]',
		'Nguyễn Văn A', '0900000000', 'a@customer.test', now(), $10
	)`, id, randCode(t, "LT"), f.checkout, f.customer, partner, listing, gross, bps, commission, randToken(t))
	return ledger.BookingMoney{BookingID: id, PartnerID: partner, ServiceType: attraction, Gross: gross, Commission: commission}
}

// inTx runs fn in a committed transaction and fails the test on error.
func (f *fixture) inTx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	require.NoError(t, f.tryTx(fn))
}

func (f *fixture) tryTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	ctx := context.Background()
	return db.InTx(ctx, f.pool, func(tx pgx.Tx) error { return fn(ctx, tx) })
}

type entryRow struct {
	Seq            int64
	Kind           string
	Bucket         string
	Amount         int64
	AvailableAfter int64
	BookingID      uuid.UUID
}

// entries returns the partner's ledger entries in seq order.
func (f *fixture) entries(t *testing.T, partner uuid.UUID) []entryRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT seq, kind, bucket, amount, available_after, booking_id
		FROM ledger_entries WHERE partner_id = $1 ORDER BY seq`, partner)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (entryRow, error) {
		var e entryRow
		err := r.Scan(&e.Seq, &e.Kind, &e.Bucket, &e.Amount, &e.AvailableAfter, &e.BookingID)
		return e, err
	})
	require.NoError(t, err)
	return out
}

type commissionRow struct {
	Kind        string
	Amount      int64
	ServiceType string
}

// commissions returns the platform commission entries of booking, commission first.
func (f *fixture) commissions(t *testing.T, booking uuid.UUID) []commissionRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT kind, amount, service_type
		FROM platform_commission_entries WHERE booking_id = $1 ORDER BY kind`, booking)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[commissionRow])
	require.NoError(t, err)
	return out
}

type settlementRow struct {
	NetPending   int64
	ReleaseDueAt *time.Time
	ReleasedAt   *time.Time
}

func (f *fixture) settlement(t *testing.T, booking uuid.UUID) settlementRow {
	t.Helper()
	var s settlementRow
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT net_pending, release_due_at, released_at
		FROM booking_settlements WHERE booking_id = $1`, booking).Scan(&s.NetPending, &s.ReleaseDueAt, &s.ReleasedAt))
	return s
}

func (f *fixture) requireBalance(t *testing.T, partner uuid.UUID, pending, available int64) {
	t.Helper()
	b, err := f.svc.Balance(context.Background(), partner)
	require.NoError(t, err)
	require.Equal(t, ledger.Balance{Pending: pending, Available: available}, b)
	require.NoError(t, f.svc.Verify(context.Background(), partner))
}

func (f *fixture) confirm(t *testing.T, b ledger.BookingMoney) {
	t.Helper()
	f.inTx(t, func(ctx context.Context, tx pgx.Tx) error { return f.svc.PostBookingConfirmed(ctx, tx, b) })
}

func (f *fixture) refund(t *testing.T, b ledger.BookingMoney, amount int64) {
	t.Helper()
	f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostRefund(ctx, tx, b.BookingID, b.PartnerID, amount)
	})
}

func (f *fixture) waive(t *testing.T, b ledger.BookingMoney) {
	t.Helper()
	f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostCommissionWaiver(ctx, tx, b.BookingID, b.PartnerID, b.ServiceType, b.Commission)
	})
}

func (f *fixture) release(t *testing.T, b ledger.BookingMoney) {
	t.Helper()
	f.inTx(t, func(ctx context.Context, tx pgx.Tx) error { return f.svc.Release(ctx, tx, b.BookingID) })
}
