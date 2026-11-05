package booking

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
)

// Schema tests for migrations 00012-00014: rows are inserted with raw SQL so
// the DB constraints themselves are exercised, independent of any service.

func pgErr(t *testing.T, err error) *pgconn.PgError {
	t.Helper()
	var e *pgconn.PgError
	require.True(t, errors.As(err, &e), "not a PgError: %v", err)
	return e
}

func requireViolation(t *testing.T, err error, code, constraint string) {
	t.Helper()
	require.Error(t, err)
	e := pgErr(t, err)
	require.Equal(t, code, e.Code, "message: %s", e.Message)
	if constraint != "" {
		require.Equal(t, constraint, e.ConstraintName)
	}
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

type fixture struct {
	pool       *pgxpool.Pool
	customer   uuid.UUID
	owner      uuid.UUID
	partner    uuid.UUID
	listing    uuid.UUID
	ticketType uuid.UUID
	slot       uuid.UUID
	checkout   uuid.UUID
	booking    uuid.UUID
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) error {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), sql, args...)
	return err
}

func (f *fixture) mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	require.NoError(t, f.exec(t, sql, args...))
}

// newPartner inserts an owner user and a partner, returning both ids.
func (f *fixture) newPartner(t *testing.T, name string) (owner, partner uuid.UUID) {
	t.Helper()
	owner, partner = newID(t), newID(t)
	f.mustExec(t, `INSERT INTO users (id, email, role) VALUES ($1, $2, 'partner')`,
		owner, name+"@partner.test")
	f.mustExec(t, `INSERT INTO partners (id, legal_name, display_name, created_by) VALUES ($1, $2, $2, $3)`,
		partner, name, owner)
	return owner, partner
}

// newListing inserts an attraction_ticket listing of partner with one ticket
// type and one time slot.
func (f *fixture) newListing(t *testing.T, partner uuid.UUID) (listing, ticketType, slot uuid.UUID) {
	t.Helper()
	listing, ticketType, slot = newID(t), newID(t), newID(t)
	f.mustExec(t, `INSERT INTO listings (id, partner_id, service_type, title, slug)
		VALUES ($1, $2, 'attraction_ticket', 'Vé tham quan', $3)`, listing, partner, "slug-"+listing.String())
	f.mustExec(t, `INSERT INTO attractions (listing_id) VALUES ($1)`, listing)
	f.mustExec(t, `INSERT INTO attraction_ticket_types (id, listing_id, name, audience, price)
		VALUES ($1, $2, 'Người lớn', 'adult', 100000)`, ticketType, listing)
	f.mustExec(t, `INSERT INTO attraction_time_slots (id, listing_id, start_time, end_time, weekdays)
		VALUES ($1, $2, '09:00', '11:00', 127)`, slot, listing)
	return listing, ticketType, slot
}

// insertBooking inserts a confirmed instant booking of the fixture's checkout
// and customer on listing with the given partner_id.
func (f *fixture) insertBooking(t *testing.T, id, listing, partner uuid.UUID) error {
	t.Helper()
	return f.exec(t, `INSERT INTO bookings (
		id, code, checkout_id, customer_id, partner_id, listing_id, service_type, booking_mode, status,
		service_start_at, service_end_at, original_service_start_at,
		gross_amount, commission_rate_bps, commission_amount, cancellation_policy_snapshot,
		contact_name, contact_phone, contact_email, confirmed_at, qr_token
	) VALUES (
		$1, $2, $3, $4, $5, $6, 'attraction_ticket', 'instant', 'confirmed',
		'2026-12-01 02:00:00Z', '2026-12-01 16:59:59Z', '2026-12-01 02:00:00Z',
		200000, 1000, 20000, '[]',
		'Nguyễn Văn A', '0900000000', 'a@customer.test', now(), $7
	)`, id, randCode(t, "LT"), f.checkout, f.customer, partner, listing, randToken(t))
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{pool: testutil.Postgres(t)}
	f.customer = newID(t)
	f.mustExec(t, `INSERT INTO users (id, email, role) VALUES ($1, 'a@customer.test', 'customer')`, f.customer)
	f.owner, f.partner = f.newPartner(t, "alpha")
	f.listing, f.ticketType, f.slot = f.newListing(t, f.partner)
	f.checkout = newID(t)
	f.mustExec(t, `INSERT INTO checkouts (id, code, customer_id, total_amount, status)
		VALUES ($1, $2, $3, 200000, 'open')`, f.checkout, randCode(t, "CO"), f.customer)
	f.booking = newID(t)
	require.NoError(t, f.insertBooking(t, f.booking, f.listing, f.partner))
	return f
}

func (f *fixture) insertLedger(t *testing.T, seq int64, kind string, booking any, amount int64) error {
	t.Helper()
	return f.exec(t, `INSERT INTO ledger_entries (id, partner_id, seq, available_after, booking_id, kind, bucket, amount)
		VALUES ($1, $2, $3, 0, $4, $5, 'pending', $6)`, newID(t), f.partner, seq, booking, kind, amount)
}

func (f *fixture) insertPayment(t *testing.T, status string) (uuid.UUID, error) {
	t.Helper()
	id := newID(t)
	err := f.exec(t, `INSERT INTO payments (id, checkout_id, amount, status, expires_at, paid_at)
		VALUES ($1, $2, 200000, $3, now() + interval '15 minutes',
		        CASE WHEN $3 = 'paid' THEN now() END)`, id, f.checkout, status)
	return id, err
}

func TestLedgerEntriesImmutable(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.insertLedger(t, 1, "booking_earning", f.booking, 180000))

	err := f.exec(t, `UPDATE ledger_entries SET amount = 1`)
	require.Error(t, err)
	require.Contains(t, pgErr(t, err).Message, "append-only")

	err = f.exec(t, `DELETE FROM ledger_entries`)
	require.Error(t, err)
	require.Contains(t, pgErr(t, err).Message, "append-only")

	var amount int64
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT amount FROM ledger_entries WHERE booking_id = $1`, f.booking).Scan(&amount))
	require.Equal(t, int64(180000), amount)
}

func TestLedgerUniqueBookingKind(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.insertLedger(t, 1, "booking_earning", f.booking, 180000))
	err := f.insertLedger(t, 2, "booking_earning", f.booking, 180000)
	requireViolation(t, err, "23505", "ledger_entries_booking_kind_bucket_idx")

	// seq is unique per partner.
	err = f.insertLedger(t, 1, "refund_debit", f.booking, -100000)
	requireViolation(t, err, "23505", "ledger_entries_partner_id_seq_key")

	// Adjustments are outside the unique index: a booking can be adjusted many times.
	for seq := int64(2); seq <= 3; seq++ {
		f.mustExec(t, `INSERT INTO ledger_entries (id, partner_id, seq, available_after, booking_id, kind, bucket, amount, reason, created_by)
			VALUES ($1, $2, $3, 0, $4, 'adjustment', 'available', 500, 'Điều chỉnh', $5)`,
			newID(t), f.partner, seq, f.booking, f.owner)
	}
}

func TestLedgerKindChecks(t *testing.T) {
	f := newFixture(t)
	// A booking kind needs booking_id.
	requireViolation(t, f.insertLedger(t, 1, "release", nil, 1000), "23514", "")
	// A zero amount is never written.
	requireViolation(t, f.insertLedger(t, 1, "booking_earning", f.booking, 0), "23514", "")
	// A payout kind needs payout_id.
	err := f.exec(t, `INSERT INTO ledger_entries (id, partner_id, seq, available_after, kind, bucket, amount)
		VALUES ($1, $2, 1, 0, 'payout_hold', 'available', -1000)`, newID(t), f.partner)
	requireViolation(t, err, "23514", "")
	// An adjustment needs reason and created_by.
	err = f.exec(t, `INSERT INTO ledger_entries (id, partner_id, seq, available_after, kind, bucket, amount)
		VALUES ($1, $2, 1, 0, 'adjustment', 'available', 1000)`, newID(t), f.partner)
	requireViolation(t, err, "23514", "")
	// Booking of another partner → composite FK violation.
	_, other := f.newPartner(t, "beta")
	err = f.exec(t, `INSERT INTO ledger_entries (id, partner_id, seq, available_after, booking_id, kind, bucket, amount)
		VALUES ($1, $2, 1, 0, $3, 'booking_earning', 'pending', 1000)`, newID(t), other, f.booking)
	requireViolation(t, err, "23503", "")
}

func TestBookingPartnerMustMatchListing(t *testing.T) {
	f := newFixture(t)
	_, other := f.newPartner(t, "beta")
	err := f.insertBooking(t, newID(t), f.listing, other)
	requireViolation(t, err, "23503", "bookings_listing_id_partner_id_fkey")
}

func TestBookingStatusMatchesMode(t *testing.T) {
	f := newFixture(t)
	// requested only exists for Request Booking.
	err := f.exec(t, `UPDATE bookings SET status = 'requested' WHERE id = $1`, f.booking)
	requireViolation(t, err, "23514", "")
	// Commission above gross is rejected.
	err = f.exec(t, `UPDATE bookings SET commission_amount = gross_amount + 1 WHERE id = $1`, f.booking)
	requireViolation(t, err, "23514", "")
	// End before start is rejected.
	err = f.exec(t, `UPDATE bookings SET service_end_at = service_start_at - interval '1 second' WHERE id = $1`, f.booking)
	requireViolation(t, err, "23514", "")
}

func TestBookingItemsLineTotalCheck(t *testing.T) {
	f := newFixture(t)
	item := func(unit int64, qty string, total int64) (uuid.UUID, error) {
		id := newID(t)
		return id, f.exec(t, `INSERT INTO booking_items (id, booking_id, line_kind, description, unit_price, quantity, line_total)
			VALUES ($1, $2, 'ticket', 'Người lớn (adult)', $3, $4::numeric, $5)`, id, f.booking, unit, qty, total)
	}
	_, err := item(100000, "2", 200000)
	require.NoError(t, err)
	// round() rounds half up: 3 × 0.5 = 1.5 → 2.
	_, err = item(3, "0.5", 2)
	require.NoError(t, err)
	_, err = item(3, "0.5", 1)
	requireViolation(t, err, "23514", "")
	_, err = item(100000, "2", 199999)
	requireViolation(t, err, "23514", "")
	_, err = item(100000, "0", 0)
	requireViolation(t, err, "23514", "")

	// A ticket line must use a ticket type of the booking's own listing.
	id, err := item(100000, "1", 100000)
	require.NoError(t, err)
	f.mustExec(t, `INSERT INTO booking_ticket_lines (booking_item_id, booking_id, listing_id, ticket_type_id)
		VALUES ($1, $2, $3, $4)`, id, f.booking, f.listing, f.ticketType)
	_, otherPartner := f.newPartner(t, "beta")
	otherListing, otherTicket, _ := f.newListing(t, otherPartner)
	id2, err := item(100000, "1", 100000)
	require.NoError(t, err)
	err = f.exec(t, `INSERT INTO booking_ticket_lines (booking_item_id, booking_id, listing_id, ticket_type_id)
		VALUES ($1, $2, $3, $4)`, id2, f.booking, otherListing, otherTicket)
	requireViolation(t, err, "23503", "")
}

func TestRefundsOnePerBooking(t *testing.T) {
	f := newFixture(t)
	pay, err := f.insertPayment(t, "paid")
	require.NoError(t, err)
	refund := func(booking any) error {
		return f.exec(t, `INSERT INTO refunds (id, payment_id, booking_id, amount, reason, status)
			VALUES ($1, $2, $3, 1000, 'customer_cancel', 'awaiting_bank_info')`, newID(t), pay, booking)
	}
	require.NoError(t, refund(f.booking))
	requireViolation(t, refund(f.booking), "23505", "refunds_booking_idx")

	// One payment-level refund (booking_id NULL) per payment, independent of booking refunds.
	require.NoError(t, refund(nil))
	requireViolation(t, refund(nil), "23505", "refunds_payment_no_booking_idx")
}

func TestRefundStatusChecks(t *testing.T) {
	f := newFixture(t)
	pay, err := f.insertPayment(t, "paid")
	require.NoError(t, err)
	insert := func(status string, bank bool, transferRef any) error {
		var bin, acct, holder any
		if bank {
			bin, acct, holder = "970436", "0123456789", "NGUYEN VAN A"
		}
		return f.exec(t, `INSERT INTO refunds (id, payment_id, booking_id, amount, reason, status,
			bank_bin, account_number, account_holder, transfer_reference, completed_by, completed_at)
			VALUES ($1, $2, $3, 1000, 'customer_cancel', $4, $5, $6, $7, $8,
			        CASE WHEN $4 = 'completed' THEN $9::uuid END, CASE WHEN $4 = 'completed' THEN now() END)`,
			newID(t), pay, f.booking, status, bin, acct, holder, transferRef, f.owner)
	}
	// pending without bank info → CHECK.
	requireViolation(t, insert("pending", false, nil), "23514", "")
	// completed without transfer_reference → CHECK.
	requireViolation(t, insert("completed", true, nil), "23514", "")
	require.NoError(t, insert("completed", true, "FT123"))
}

func TestOnePendingPaymentPerCheckout(t *testing.T) {
	f := newFixture(t)
	first, err := f.insertPayment(t, "pending")
	require.NoError(t, err)
	_, err = f.insertPayment(t, "pending")
	requireViolation(t, err, "23505", "payments_checkout_pending_idx")

	// Once the first link expired, a new pending link is allowed.
	f.mustExec(t, `UPDATE payments SET status = 'expired' WHERE id = $1`, first)
	_, err = f.insertPayment(t, "pending")
	require.NoError(t, err)

	// provider_order_code comes from payment_order_code_seq (starts at 100000).
	var minCode, distinct int64
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT min(provider_order_code), count(DISTINCT provider_order_code) FROM payments`).Scan(&minCode, &distinct))
	require.GreaterOrEqual(t, minCode, int64(100000))
	require.Equal(t, int64(2), distinct)
}
