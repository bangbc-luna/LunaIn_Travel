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
	return f.insertBookingFull(t, id, listing, partner, f.checkout, f.customer, "instant", "confirmed")
}

// insertBookingFull inserts a booking with the given checkout, customer, mode
// and status; confirmed_at / response_due_at are set when the status needs them.
func (f *fixture) insertBookingFull(t *testing.T, id, listing, partner, checkout, customer uuid.UUID, mode, status string) error {
	t.Helper()
	return f.exec(t, `INSERT INTO bookings (
		id, code, checkout_id, customer_id, partner_id, listing_id, service_type, booking_mode, status,
		service_start_at, service_end_at, original_service_start_at,
		gross_amount, commission_rate_bps, commission_amount, cancellation_policy_snapshot,
		contact_name, contact_phone, contact_email, confirmed_at, response_due_at, qr_token
	) VALUES (
		$1, $2, $3, $4, $5, $6, 'attraction_ticket', $7, $8,
		'2026-12-01 02:00:00Z', '2026-12-01 16:59:59Z', '2026-12-01 02:00:00Z',
		200000, 1000, 20000, '[]',
		'Nguyễn Văn A', '0900000000', 'a@customer.test',
		CASE WHEN $8 = 'confirmed' THEN now() END, CASE WHEN $8 = 'requested' THEN now() END, $9
	)`, id, randCode(t, "LT"), checkout, customer, partner, listing, mode, status, randToken(t))
}

// newCheckout inserts an open checkout of customer.
func (f *fixture) newCheckout(t *testing.T, customer uuid.UUID) uuid.UUID {
	t.Helper()
	id := newID(t)
	f.mustExec(t, `INSERT INTO checkouts (id, code, customer_id, total_amount, status)
		VALUES ($1, $2, $3, 200000, 'open')`, id, randCode(t, "CO"), customer)
	return id
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{pool: testutil.Postgres(t)}
	f.customer = newID(t)
	f.mustExec(t, `INSERT INTO users (id, email, role) VALUES ($1, 'a@customer.test', 'customer')`, f.customer)
	f.owner, f.partner = f.newPartner(t, "alpha")
	f.listing, f.ticketType, f.slot = f.newListing(t, f.partner)
	f.checkout = f.newCheckout(t, f.customer)
	f.booking = newID(t)
	require.NoError(t, f.insertBooking(t, f.booking, f.listing, f.partner))
	return f
}

func (f *fixture) insertLedger(t *testing.T, seq int64, kind string, booking any, amount int64) error {
	t.Helper()
	return f.insertLedgerBucket(t, seq, kind, "pending", booking, amount)
}

func (f *fixture) insertLedgerBucket(t *testing.T, seq int64, kind, bucket string, booking any, amount int64) error {
	t.Helper()
	return f.exec(t, `INSERT INTO ledger_entries (id, partner_id, seq, available_after, booking_id, kind, bucket, amount)
		VALUES ($1, $2, $3, 0, $4, $5, $6, $7)`, newID(t), f.partner, seq, booking, kind, bucket, amount)
}

func (f *fixture) insertPayment(t *testing.T, status string) (uuid.UUID, error) {
	t.Helper()
	id := newID(t)
	err := f.exec(t, `INSERT INTO payments (id, checkout_id, amount, status, expires_at, paid_at)
		VALUES ($1, $2, 200000, $3, now() + interval '15 minutes',
		        CASE WHEN $3 = 'paid' THEN now() END)`, id, f.checkout, status)
	return id, err
}

// insertRefund inserts an awaiting_bank_info refund; booking and ref may be nil.
func (f *fixture) insertRefund(t *testing.T, payment, checkout uuid.UUID, booking, ref any) error {
	t.Helper()
	return f.exec(t, `INSERT INTO refunds (id, payment_id, checkout_id, booking_id, provider_reference, amount, reason, status)
		VALUES ($1, $2, $3, $4, $5, 1000, 'customer_cancel', 'awaiting_bank_info')`,
		newID(t), payment, checkout, booking, ref)
}

func TestLedgerEntriesImmutable(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.insertLedger(t, 1, "booking_earning", f.booking, 180000))

	for _, sql := range []string{
		`UPDATE ledger_entries SET amount = 1`,
		`DELETE FROM ledger_entries`,
		`TRUNCATE ledger_entries`,
	} {
		err := f.exec(t, sql)
		requireViolation(t, err, "23001", "")
		require.Contains(t, pgErr(t, err).Message, "append-only", sql)
	}

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

func TestLedgerBucketAndSignChecks(t *testing.T) {
	f := newFixture(t)
	// booking_earning, refund_debit and commission_waiver only touch pending.
	requireViolation(t, f.insertLedgerBucket(t, 1, "booking_earning", "available", f.booking, 1000),
		"23514", "ledger_entries_pending_bucket_check")
	requireViolation(t, f.insertLedgerBucket(t, 1, "refund_debit", "available", f.booking, -1000),
		"23514", "ledger_entries_pending_bucket_check")
	// Signs.
	requireViolation(t, f.insertLedger(t, 1, "booking_earning", f.booking, -1000),
		"23514", "ledger_entries_earning_sign_check")
	requireViolation(t, f.insertLedger(t, 1, "refund_debit", f.booking, 1000),
		"23514", "ledger_entries_refund_sign_check")
	requireViolation(t, f.insertLedger(t, 1, "commission_waiver", f.booking, -1000),
		"23514", "ledger_entries_waiver_sign_check")
	// release moves money out of pending (negative) and into available (positive).
	require.NoError(t, f.insertLedgerBucket(t, 1, "release", "pending", f.booking, -1000))
	require.NoError(t, f.insertLedgerBucket(t, 2, "release", "available", f.booking, 1000))
}

func TestBookingCheckoutMustBelongToCustomer(t *testing.T) {
	f := newFixture(t)
	other := newID(t)
	f.mustExec(t, `INSERT INTO users (id, email, role) VALUES ($1, 'b@customer.test', 'customer')`, other)
	otherCheckout := f.newCheckout(t, other)
	err := f.insertBookingFull(t, newID(t), f.listing, f.partner, otherCheckout, f.customer, "instant", "confirmed")
	requireViolation(t, err, "23503", "bookings_checkout_id_customer_id_fkey")
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
	err := f.exec(t, `UPDATE bookings SET status = 'requested', response_due_at = now() WHERE id = $1`, f.booking)
	requireViolation(t, err, "23514", "bookings_request_only_status_check")
	// pending_payment only exists for Instant Booking.
	req := newID(t)
	require.NoError(t, f.insertBookingFull(t, req, f.listing, f.partner, f.checkout, f.customer, "request", "requested"))
	err = f.exec(t, `UPDATE bookings SET status = 'pending_payment',
		hold_expires_at = now(), hold_release_at = now() WHERE id = $1`, req)
	requireViolation(t, err, "23514", "bookings_instant_only_status_check")
	// The same transition is fine on an instant booking.
	f.mustExec(t, `UPDATE bookings SET status = 'pending_payment', confirmed_at = NULL,
		hold_expires_at = now(), hold_release_at = now() WHERE id = $1`, f.booking)
	// pending_payment without a hold is rejected.
	err = f.exec(t, `UPDATE bookings SET hold_expires_at = NULL, hold_release_at = NULL WHERE id = $1`, f.booking)
	requireViolation(t, err, "23514", "")
	f.mustExec(t, `UPDATE bookings SET status = 'confirmed', confirmed_at = now() WHERE id = $1`, f.booking)
	// confirmed without confirmed_at is rejected.
	err = f.exec(t, `UPDATE bookings SET confirmed_at = NULL WHERE id = $1`, f.booking)
	requireViolation(t, err, "23514", "")
	// The policy snapshot must be a JSON array.
	err = f.exec(t, `UPDATE bookings SET cancellation_policy_snapshot = '{}' WHERE id = $1`, f.booking)
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

}

func TestBookingTicketLineListingFKs(t *testing.T) {
	f := newFixture(t)
	item := func() uuid.UUID {
		id := newID(t)
		f.mustExec(t, `INSERT INTO booking_items (id, booking_id, line_kind, description, unit_price, quantity, line_total)
			VALUES ($1, $2, 'ticket', 'Người lớn (adult)', 100000, 1, 100000)`, id, f.booking)
		return id
	}
	line := func(itemID, listing, ticket uuid.UUID) error {
		return f.exec(t, `INSERT INTO booking_ticket_lines (booking_item_id, booking_id, listing_id, ticket_type_id)
			VALUES ($1, $2, $3, $4)`, itemID, f.booking, listing, ticket)
	}
	require.NoError(t, line(item(), f.listing, f.ticketType))

	_, otherPartner := f.newPartner(t, "beta")
	otherListing, otherTicket, _ := f.newListing(t, otherPartner)
	// Booking's listing with another listing's ticket type -> ticket-type FK.
	requireViolation(t, line(item(), f.listing, otherTicket),
		"23503", "booking_ticket_lines_ticket_type_id_listing_id_fkey")
	// Another listing (with its own ticket type) than the booking's -> booking FK.
	requireViolation(t, line(item(), otherListing, otherTicket),
		"23503", "booking_ticket_lines_booking_id_listing_id_fkey")
}

func TestRefundsOnePerBooking(t *testing.T) {
	f := newFixture(t)
	pay, err := f.insertPayment(t, "paid")
	require.NoError(t, err)
	require.NoError(t, f.insertRefund(t, pay, f.checkout, f.booking, nil))
	requireViolation(t, f.insertRefund(t, pay, f.checkout, f.booking, nil), "23505", "refunds_booking_idx")

	// Payment-level refunds (booking_id NULL) are one per incoming transfer.
	require.NoError(t, f.insertRefund(t, pay, f.checkout, nil, "FT-1"))
	require.NoError(t, f.insertRefund(t, pay, f.checkout, nil, "FT-2"))
	requireViolation(t, f.insertRefund(t, pay, f.checkout, nil, "FT-1"), "23505", "refunds_payment_transfer_idx")
	// A payment-level refund needs the transfer reference.
	requireViolation(t, f.insertRefund(t, pay, f.checkout, nil, nil), "23514", "")
}

func TestRefundPaymentAndBookingSameCheckout(t *testing.T) {
	f := newFixture(t)
	pay, err := f.insertPayment(t, "paid")
	require.NoError(t, err)
	// A second checkout of the same customer with its own booking.
	checkout2 := f.newCheckout(t, f.customer)
	booking2 := newID(t)
	require.NoError(t, f.insertBookingFull(t, booking2, f.listing, f.partner, checkout2, f.customer, "instant", "confirmed"))

	// Booking of checkout2 refunded from a payment of f.checkout.
	requireViolation(t, f.insertRefund(t, pay, f.checkout, booking2, nil),
		"23503", "refunds_booking_id_checkout_id_fkey")
	requireViolation(t, f.insertRefund(t, pay, checkout2, booking2, nil),
		"23503", "refunds_payment_id_checkout_id_fkey")
	require.NoError(t, f.insertRefund(t, pay, f.checkout, f.booking, nil))
}

func TestBookingSettlementReleasedMeansZeroPending(t *testing.T) {
	f := newFixture(t)
	f.mustExec(t, `INSERT INTO booking_settlements (booking_id, partner_id, net_pending, release_due_at)
		VALUES ($1, $2, 180000, now())`, f.booking, f.partner)
	err := f.exec(t, `UPDATE booking_settlements SET released_at = now() WHERE booking_id = $1`, f.booking)
	requireViolation(t, err, "23514", "")
	f.mustExec(t, `UPDATE booking_settlements SET released_at = now(), net_pending = 0 WHERE booking_id = $1`, f.booking)
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
		return f.exec(t, `INSERT INTO refunds (id, payment_id, checkout_id, booking_id, amount, reason, status,
			bank_bin, account_number, account_holder, transfer_reference, completed_by, completed_at)
			VALUES ($1, $2, $3, $4, 1000, 'customer_cancel', $5, $6, $7, $8, $9,
			        CASE WHEN $5 = 'completed' THEN $10::uuid END, CASE WHEN $5 = 'completed' THEN now() END)`,
			newID(t), pay, f.checkout, f.booking, status, bin, acct, holder, transferRef, f.owner)
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

// --- Scheduled ride schema (migration 00017) ---

// wardPair returns the (ward_code, province_code) of the offset-th seeded ward.
func (f *fixture) wardPair(t *testing.T, offset int) (ward, province string) {
	t.Helper()
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT code, province_code FROM wards ORDER BY code OFFSET $1 LIMIT 1`, offset).Scan(&ward, &province))
	return ward, province
}

// newRideListing inserts a scheduled_ride listing of partner with its route and
// one schedule.
func (f *fixture) newRideListing(t *testing.T, partner uuid.UUID) (listing, schedule uuid.UUID) {
	t.Helper()
	listing, schedule = newID(t), newID(t)
	f.mustExec(t, `INSERT INTO listings (id, partner_id, service_type, title, slug)
		VALUES ($1, $2, 'scheduled_ride', 'Xe tuyến', $3)`, listing, partner, "slug-"+listing.String())
	ow, op := f.wardPair(t, 0)
	dw, dp := f.wardPair(t, 1)
	f.mustExec(t, `INSERT INTO ride_routes (
		listing_id, origin_province_code, origin_ward_code, origin_name, origin_address,
		dest_province_code, dest_ward_code, dest_name, dest_address, duration_minutes, vehicle_description)
		VALUES ($1, $2, $3, 'Bến A', '1 A', $4, $5, 'Bến B', '2 B', 180, 'Limousine 9 chỗ')`,
		listing, op, ow, dp, dw)
	f.mustExec(t, `INSERT INTO ride_schedules (id, listing_id, departure_time, weekdays, seat_capacity, price_per_seat, valid_from)
		VALUES ($1, $2, '08:00', 127, 9, 150000, '2026-12-01')`, schedule, listing)
	return listing, schedule
}

func (f *fixture) insertTrip(t *testing.T, id, listing uuid.UUID, schedule any, departure string, capacity, reserved int) error {
	t.Helper()
	return f.exec(t, `INSERT INTO ride_trips (id, listing_id, schedule_id, departure_at, seat_capacity, seats_reserved, price_per_seat)
		VALUES ($1, $2, $3, $4::timestamptz, $5, $6, 150000)`, id, listing, schedule, departure, capacity, reserved)
}

func TestRideTripSeatsCheck(t *testing.T) {
	f := newFixture(t)
	listing, _ := f.newRideListing(t, f.partner)

	err := f.insertTrip(t, newID(t), listing, nil, "2026-12-02 01:00:00Z", 9, 10)
	requireViolation(t, err, "23514", "ride_trips_seats_check")
	err = f.insertTrip(t, newID(t), listing, nil, "2026-12-02 02:00:00Z", 9, -1)
	requireViolation(t, err, "23514", "ride_trips_seats_check")
	require.NoError(t, f.insertTrip(t, newID(t), listing, nil, "2026-12-02 03:00:00Z", 9, 9))

	trip := newID(t)
	require.NoError(t, f.insertTrip(t, trip, listing, nil, "2026-12-02 04:00:00Z", 9, 0))
	err = f.exec(t, `UPDATE ride_trips SET seats_reserved = seats_reserved + 10 WHERE id = $1`, trip)
	requireViolation(t, err, "23514", "ride_trips_seats_check")
}

func TestRideTripUniqueDeparture(t *testing.T) {
	f := newFixture(t)
	listing, _ := f.newRideListing(t, f.partner)
	require.NoError(t, f.insertTrip(t, newID(t), listing, nil, "2026-12-02 01:00:00Z", 9, 0))
	err := f.insertTrip(t, newID(t), listing, nil, "2026-12-02 01:00:00Z", 9, 0)
	requireViolation(t, err, "23505", "ride_trips_listing_id_departure_at_key")
}

func TestRideTripTriggerBlocksBookedChanges(t *testing.T) {
	f := newFixture(t)
	listing, _ := f.newRideListing(t, f.partner)

	booked := newID(t)
	require.NoError(t, f.insertTrip(t, booked, listing, nil, "2026-12-02 01:00:00Z", 9, 1))
	for _, sql := range []string{
		`UPDATE ride_trips SET departure_at = departure_at + interval '1 hour' WHERE id = $1`,
		`UPDATE ride_trips SET price_per_seat = 1 WHERE id = $1`,
	} {
		err := f.exec(t, sql, booked)
		require.Error(t, err)
		e := pgErr(t, err)
		require.Equal(t, "P0001", e.Code)
		require.Equal(t, "ride_trip_booked", e.Message)
	}
	// Capacity may still change on a booked trip.
	require.NoError(t, f.exec(t, `UPDATE ride_trips SET seat_capacity = 12 WHERE id = $1`, booked))

	free := newID(t)
	require.NoError(t, f.insertTrip(t, free, listing, nil, "2026-12-03 01:00:00Z", 9, 0))
	require.NoError(t, f.exec(t, `UPDATE ride_trips SET departure_at = departure_at + interval '1 hour' WHERE id = $1`, free))
	require.NoError(t, f.exec(t, `UPDATE ride_trips SET price_per_seat = 200000 WHERE id = $1`, free))
}

func TestRideDetailTripMustMatchListing(t *testing.T) {
	f := newFixture(t)
	listingA, _ := f.newRideListing(t, f.partner)
	listingB, _ := f.newRideListing(t, f.partner)
	tripB := newID(t)
	require.NoError(t, f.insertTrip(t, tripB, listingB, nil, "2026-12-02 01:00:00Z", 9, 1))

	// A ride booking of listing A pointing at a trip of listing B.
	rideBooking := newID(t)
	f.mustExec(t, `INSERT INTO bookings (
		id, code, checkout_id, customer_id, partner_id, listing_id, service_type, booking_mode, status,
		service_start_at, service_end_at, original_service_start_at,
		gross_amount, commission_rate_bps, commission_amount, cancellation_policy_snapshot,
		contact_name, contact_phone, contact_email, confirmed_at, qr_token
	) VALUES ($1, $2, $3, $4, $5, $6, 'scheduled_ride', 'instant', 'confirmed',
		'2026-12-02 01:00:00Z', '2026-12-02 04:00:00Z', '2026-12-02 01:00:00Z',
		150000, 1000, 15000, '[]', 'A', '0900000000', 'a@customer.test', now(), $7)`,
		rideBooking, randCode(t, "LT"), f.checkout, f.customer, f.partner, listingA, randToken(t))

	err := f.exec(t, `INSERT INTO booking_ride_details (booking_id, listing_id, trip_id, seats, direction, origin_name, dest_name)
		VALUES ($1, $2, $3, 1, 'outbound', 'Bến A', 'Bến B')`, rideBooking, listingA, tripB)
	requireViolation(t, err, "23503", "booking_ride_details_trip_id_listing_id_fkey")

	tripA := newID(t)
	require.NoError(t, f.insertTrip(t, tripA, listingA, nil, "2026-12-02 02:00:00Z", 9, 1))
	require.NoError(t, f.exec(t, `INSERT INTO booking_ride_details (booking_id, listing_id, trip_id, seats, direction, origin_name, dest_name)
		VALUES ($1, $2, $3, 1, 'outbound', 'Bến A', 'Bến B')`, rideBooking, listingA, tripA))
}

func TestRideScheduleOtherListing(t *testing.T) {
	f := newFixture(t)
	listingA, _ := f.newRideListing(t, f.partner)
	_, scheduleB := f.newRideListing(t, f.partner)

	err := f.insertTrip(t, newID(t), listingA, scheduleB, "2026-12-02 01:00:00Z", 9, 0)
	requireViolation(t, err, "23503", "ride_trips_schedule_id_listing_id_fkey")
}

func TestBookingCheckedInNeedsTimestamp(t *testing.T) {
	f := newFixture(t)
	err := f.exec(t, `UPDATE bookings SET status = 'checked_in' WHERE id = $1`, f.booking)
	requireViolation(t, err, "23514", "bookings_checked_in_at_check")
	require.NoError(t, f.exec(t, `UPDATE bookings SET status = 'checked_in', checked_in_at = now() WHERE id = $1`, f.booking))
}

// --- Self-drive rental schema (migration 00018) ---

// newRentalStore inserts a self_drive_rental listing of partner with its store.
func (f *fixture) newRentalStore(t *testing.T, partner uuid.UUID) uuid.UUID {
	t.Helper()
	listing := newID(t)
	f.mustExec(t, `INSERT INTO listings (id, partner_id, service_type, title, slug)
		VALUES ($1, $2, 'self_drive_rental', 'Cho thuê xe', $3)`, listing, partner, "slug-"+listing.String())
	f.mustExec(t, `INSERT INTO rental_stores (listing_id, handover_open, handover_close) VALUES ($1, '07:00', '21:00')`, listing)
	return listing
}

func (f *fixture) insertRentalModel(t *testing.T, id, listing uuid.UUID, price, quantity int64) error {
	t.Helper()
	return f.exec(t, `INSERT INTO rental_vehicle_models (id, listing_id, vehicle_kind, name, model_year, seats, price_per_day, quantity)
		VALUES ($1, $2, 'car', 'Vios', 2023, 5, $3, $4)`, id, listing, price, quantity)
}

func (f *fixture) insertRentalPoint(t *testing.T, id, listing uuid.UUID) {
	t.Helper()
	w, p := f.wardPair(t, 0)
	f.mustExec(t, `INSERT INTO rental_pickup_points (id, listing_id, name, address, province_code, ward_code)
		VALUES ($1, $2, 'Cửa hàng', '1 A', $3, $4)`, id, listing, p, w)
}

// insertRentalBooking inserts a request-mode self_drive_rental booking.
func (f *fixture) insertRentalBooking(t *testing.T, id, listing uuid.UUID) {
	t.Helper()
	f.mustExec(t, `INSERT INTO bookings (
		id, code, checkout_id, customer_id, partner_id, listing_id, service_type, booking_mode, status,
		service_start_at, service_end_at, original_service_start_at,
		gross_amount, commission_rate_bps, commission_amount, cancellation_policy_snapshot,
		contact_name, contact_phone, contact_email, response_due_at, qr_token
	) VALUES ($1, $2, $3, $4, $5, $6, 'self_drive_rental', 'request', 'requested',
		'2026-12-01 01:00:00Z', '2026-12-02 03:00:00Z', '2026-12-01 01:00:00Z',
		1000000, 1000, 100000, '[]', 'A', '0900000000', 'a@customer.test', now(), $7)`,
		id, randCode(t, "LT"), f.checkout, f.customer, f.partner, listing, randToken(t))
}

func (f *fixture) insertRentalDetail(t *testing.T, booking, listing, model, point uuid.UUID, pickup, ret string, holds bool, holdUntil any) error {
	t.Helper()
	return f.exec(t, `INSERT INTO booking_rental_details (
		booking_id, listing_id, vehicle_model_id, pickup_point_id, pickup_at, return_at, days,
		model_name, pickup_point_name, pickup_address, destination_province_code,
		holds_vehicle, hold_until)
		VALUES ($1, $2, $3, $4, $5::timestamptz, $6::timestamptz, 2, 'Vios', 'Cửa hàng', '1 A',
		        (SELECT province_code FROM wards ORDER BY code LIMIT 1), $7, $8)`,
		booking, listing, model, point, pickup, ret, holds, holdUntil)
}

func TestRentalModelQuantityCheck(t *testing.T) {
	f := newFixture(t)
	listing := f.newRentalStore(t, f.partner)
	requireViolation(t, f.insertRentalModel(t, newID(t), listing, 500000, -1), "23514", "")
	requireViolation(t, f.insertRentalModel(t, newID(t), listing, 0, 1), "23514", "")
	require.NoError(t, f.insertRentalModel(t, newID(t), listing, 500000, 0))
}

func TestRentalStoreHandoverOrder(t *testing.T) {
	f := newFixture(t)
	listing := newID(t)
	f.mustExec(t, `INSERT INTO listings (id, partner_id, service_type, title, slug)
		VALUES ($1, $2, 'self_drive_rental', 'Cho thuê xe', $3)`, listing, f.partner, "slug-"+listing.String())
	err := f.exec(t, `INSERT INTO rental_stores (listing_id, handover_open, handover_close) VALUES ($1, '08:00', '08:00')`, listing)
	requireViolation(t, err, "23514", "")
}

// rentalDetailFixture returns a rental booking with a model and pickup point of one listing.
func rentalDetailFixture(t *testing.T) (f *fixture, booking, listing, model, point uuid.UUID) {
	f = newFixture(t)
	listing = f.newRentalStore(t, f.partner)
	model, point, booking = newID(t), newID(t), newID(t)
	require.NoError(t, f.insertRentalModel(t, model, listing, 500000, 2))
	f.insertRentalPoint(t, point, listing)
	f.insertRentalBooking(t, booking, listing)
	return
}

func TestRentalDetailModelMustMatchListing(t *testing.T) {
	f, booking, listing, _, point := rentalDetailFixture(t)
	other := f.newRentalStore(t, f.partner)
	otherModel := newID(t)
	require.NoError(t, f.insertRentalModel(t, otherModel, other, 500000, 2))
	err := f.insertRentalDetail(t, booking, listing, otherModel, point, "2026-12-01 01:00:00Z", "2026-12-02 03:00:00Z", true, nil)
	requireViolation(t, err, "23503", "booking_rental_details_vehicle_model_id_listing_id_fkey")
}

func TestRentalDetailPointMustMatchListing(t *testing.T) {
	f, booking, listing, model, _ := rentalDetailFixture(t)
	other := f.newRentalStore(t, f.partner)
	otherPoint := newID(t)
	f.insertRentalPoint(t, otherPoint, other)
	err := f.insertRentalDetail(t, booking, listing, model, otherPoint, "2026-12-01 01:00:00Z", "2026-12-02 03:00:00Z", true, nil)
	requireViolation(t, err, "23503", "booking_rental_details_pickup_point_id_listing_id_fkey")
}

func TestRentalDetailPeriodGenerated(t *testing.T) {
	f, booking, listing, model, point := rentalDetailFixture(t)
	// 08:00 VN day 1 to 10:00 VN day 2.
	require.NoError(t, f.insertRentalDetail(t, booking, listing, model, point, "2026-12-01 01:00:00Z", "2026-12-02 03:00:00Z", true, nil))
	var touching, overlapping bool
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT
		period && tstzrange('2026-12-02 03:00:00Z', '2026-12-02 04:00:00Z', '[)'),
		period && tstzrange('2026-12-02 02:59:00Z', '2026-12-02 04:00:00Z', '[)')
		FROM booking_rental_details WHERE booking_id = $1`, booking).Scan(&touching, &overlapping))
	require.False(t, touching)
	require.True(t, overlapping)
}

func TestRentalDetailHoldUntilNeedsHolds(t *testing.T) {
	f, booking, listing, model, point := rentalDetailFixture(t)
	err := f.insertRentalDetail(t, booking, listing, model, point, "2026-12-01 01:00:00Z", "2026-12-02 03:00:00Z", false, "2026-12-01 00:00:00Z")
	requireViolation(t, err, "23514", "")
	require.NoError(t, f.insertRentalDetail(t, booking, listing, model, point, "2026-12-01 01:00:00Z", "2026-12-02 03:00:00Z", true, "2026-12-01 00:00:00Z"))
}

func TestRentalDetailReturnAfterPickup(t *testing.T) {
	f, booking, listing, model, point := rentalDetailFixture(t)
	err := f.insertRentalDetail(t, booking, listing, model, point, "2026-12-01 01:00:00Z", "2026-12-01 01:00:00Z", true, nil)
	requireViolation(t, err, "23514", "")
}

func TestBookingInServiceNeedsTimestamp(t *testing.T) {
	f := newFixture(t)
	err := f.exec(t, `UPDATE bookings SET status = 'in_service' WHERE id = $1`, f.booking)
	requireViolation(t, err, "23514", "bookings_in_service_at_check")
	require.NoError(t, f.exec(t, `UPDATE bookings SET status = 'in_service', in_service_at = now() WHERE id = $1`, f.booking))
}

func TestBookingAwaitingNeedsAcceptance(t *testing.T) {
	f := newFixture(t)
	id := newID(t)
	require.NoError(t, f.insertBookingFull(t, id, f.listing, f.partner, f.checkout, f.customer, "request", "requested"))
	err := f.exec(t, `UPDATE bookings SET status = 'awaiting_payment', hold_expires_at = now(), hold_release_at = now() WHERE id = $1`, id)
	requireViolation(t, err, "23514", "bookings_accepted_check")
	require.NoError(t, f.exec(t, `UPDATE bookings SET status = 'awaiting_payment', hold_expires_at = now(), hold_release_at = now(),
		accepted_at = now(), payment_due_at = now() WHERE id = $1`, id))
}

func TestBookingRejectedNeedsReason(t *testing.T) {
	f := newFixture(t)
	id := newID(t)
	require.NoError(t, f.insertBookingFull(t, id, f.listing, f.partner, f.checkout, f.customer, "request", "requested"))
	err := f.exec(t, `UPDATE bookings SET status = 'rejected', rejected_at = now() WHERE id = $1`, id)
	requireViolation(t, err, "23514", "bookings_reject_reason_check")
	require.NoError(t, f.exec(t, `UPDATE bookings SET status = 'rejected', rejected_at = now(), reject_reason = 'hết xe' WHERE id = $1`, id))
}

// --- private charter (00019) ---

func (f *fixture) newCharterService(t *testing.T, partner uuid.UUID) uuid.UUID {
	t.Helper()
	listing := newID(t)
	f.mustExec(t, `INSERT INTO listings (id, partner_id, service_type, title, slug)
		VALUES ($1, $2, 'private_charter', 'Xe bao chuyến', $3)`, listing, partner, "slug-"+listing.String())
	f.mustExec(t, `INSERT INTO charter_services (listing_id) VALUES ($1)`, listing)
	return listing
}

func (f *fixture) insertCharterClass(t *testing.T, id, listing uuid.UUID, seats int, baseFare, perKm int64) error {
	t.Helper()
	return f.exec(t, `INSERT INTO charter_vehicle_classes (id, listing_id, name, seats, base_fare, price_per_km)
		VALUES ($1, $2, 'Xe 4 chỗ', $3, $4, $5)`, id, listing, seats, baseFare, perKm)
}

func (f *fixture) insertCharterSurcharge(t *testing.T, class uuid.UUID, kind, calc string, amount int64) error {
	t.Helper()
	return f.exec(t, `INSERT INTO charter_surcharges (id, vehicle_class_id, kind, calc, amount)
		VALUES ($1, $2, $3, $4, $5)`, newID(t), class, kind, calc, amount)
}

// insertCharterBooking inserts a request-mode private_charter booking.
func (f *fixture) insertCharterBooking(t *testing.T, id, listing uuid.UUID) {
	t.Helper()
	f.mustExec(t, `INSERT INTO bookings (
		id, code, checkout_id, customer_id, partner_id, listing_id, service_type, booking_mode, status,
		service_start_at, service_end_at, original_service_start_at,
		gross_amount, commission_rate_bps, commission_amount, cancellation_policy_snapshot,
		contact_name, contact_phone, contact_email, response_due_at, qr_token
	) VALUES ($1, $2, $3, $4, $5, $6, 'private_charter', 'request', 'requested',
		'2026-12-01 01:00:00Z', '2026-12-01 05:00:00Z', '2026-12-01 01:00:00Z',
		1000000, 1000, 100000, '[]', 'A', '0900000000', 'a@customer.test', now(), $7)`,
		id, randCode(t, "LT"), f.checkout, f.customer, f.partner, listing, randToken(t))
}

func (f *fixture) insertCharterDetail(t *testing.T, booking, listing, class uuid.UUID, distance string, roundTrip bool, returnAt any) error {
	t.Helper()
	return f.exec(t, `INSERT INTO booking_charter_details (
		booking_id, listing_id, vehicle_class_id, pickup_at, pickup_address, pickup_lat, pickup_lng,
		dropoff_address, dropoff_lat, dropoff_lng, distance_km, is_round_trip, return_at, quote,
		pickup_province_code, duration_minutes, vehicle_class_name, seats)
		VALUES ($1, $2, $3, '2026-12-01 01:00:00Z', '1 A', 21.028511, 105.804817,
		        '2 B', 20.8449, 106.6881, $4::numeric, $5, $6::timestamptz, '{}',
		        (SELECT code FROM provinces ORDER BY code LIMIT 1), 120, 'Xe 4 chỗ', 4)`,
		booking, listing, class, distance, roundTrip, returnAt)
}

func charterDetailFixture(t *testing.T) (f *fixture, booking, listing, class uuid.UUID) {
	f = newFixture(t)
	listing = f.newCharterService(t, f.partner)
	class, booking = newID(t), newID(t)
	require.NoError(t, f.insertCharterClass(t, class, listing, 4, 0, 12000))
	f.insertCharterBooking(t, booking, listing)
	return
}

func TestCharterSurchargeCalcRules(t *testing.T) {
	f := newFixture(t)
	listing := f.newCharterService(t, f.partner)
	class := newID(t)
	require.NoError(t, f.insertCharterClass(t, class, listing, 4, 0, 12000))
	requireViolation(t, f.insertCharterSurcharge(t, class, "night", "percent", 1000), "23514", "")
	requireViolation(t, f.insertCharterSurcharge(t, class, "round_trip_waiting", "percent", 1000), "23514", "")
	requireViolation(t, f.insertCharterSurcharge(t, class, "weekend", "percent", 10001), "23514", "")
	require.NoError(t, f.insertCharterSurcharge(t, class, "weekend", "percent", 10000))
	requireViolation(t, f.insertCharterSurcharge(t, class, "weekend", "fixed", 5), "23505", "")
}

func TestCharterClassPricePerKmPositive(t *testing.T) {
	f := newFixture(t)
	listing := f.newCharterService(t, f.partner)
	requireViolation(t, f.insertCharterClass(t, newID(t), listing, 4, 500000, 0), "23514", "")
	require.NoError(t, f.insertCharterClass(t, newID(t), listing, 4, 0, 12000))
	requireViolation(t, f.insertCharterClass(t, newID(t), listing, 0, 0, 12000), "23514", "")
}

func TestCharterServiceNightPair(t *testing.T) {
	f := newFixture(t)
	mk := func(sql string) error {
		listing := newID(t)
		f.mustExec(t, `INSERT INTO listings (id, partner_id, service_type, title, slug)
			VALUES ($1, $2, 'private_charter', 'Xe', $3)`, listing, f.partner, "slug-"+listing.String())
		return f.exec(t, sql, listing)
	}
	requireViolation(t, mk(`INSERT INTO charter_services (listing_id, night_start) VALUES ($1, '22:00')`), "23514", "")
	requireViolation(t, mk(`INSERT INTO charter_services (listing_id, night_start, night_end) VALUES ($1, '22:00', '22:00')`), "23514", "")
	require.NoError(t, mk(`INSERT INTO charter_services (listing_id, night_start, night_end) VALUES ($1, '22:00', '05:00')`))
}

func TestCharterDetailClassMustMatchListing(t *testing.T) {
	f, booking, listing, _ := charterDetailFixture(t)
	other := f.newCharterService(t, f.partner)
	otherClass := newID(t)
	require.NoError(t, f.insertCharterClass(t, otherClass, other, 4, 0, 12000))
	err := f.insertCharterDetail(t, booking, listing, otherClass, "30", false, nil)
	requireViolation(t, err, "23503", "booking_charter_details_vehicle_class_id_listing_id_fkey")
}

func TestCharterDetailRoundTripNeedsReturn(t *testing.T) {
	f, booking, listing, class := charterDetailFixture(t)
	requireViolation(t, f.insertCharterDetail(t, booking, listing, class, "30", true, nil), "23514", "")
	requireViolation(t, f.insertCharterDetail(t, booking, listing, class, "30", false, "2026-12-01 05:00:00Z"), "23514", "")
	requireViolation(t, f.insertCharterDetail(t, booking, listing, class, "30", true, "2026-12-01 01:00:00Z"), "23514", "")
	require.NoError(t, f.insertCharterDetail(t, booking, listing, class, "30", true, "2026-12-01 05:00:00Z"))
}

func TestCharterDetailDistancePositive(t *testing.T) {
	f, booking, listing, class := charterDetailFixture(t)
	requireViolation(t, f.insertCharterDetail(t, booking, listing, class, "0", false, nil), "23514", "")
	require.NoError(t, f.insertCharterDetail(t, booking, listing, class, "30.25", false, nil))
	var km string
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT distance_km::text FROM booking_charter_details WHERE booking_id = $1`, booking).Scan(&km))
	require.Equal(t, "30.3", km)
}
