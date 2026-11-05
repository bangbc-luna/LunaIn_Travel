package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/ledger"
)

// newPayout inserts a bare requested payout (with the bank account its
// snapshot points at) so payout entries have a payouts row to reference.
func (f *fixture) newPayout(t *testing.T, partner, requestedBy uuid.UUID, amount int64) uuid.UUID {
	t.Helper()
	bank, id := newID(t), newID(t)
	f.mustExec(t, `INSERT INTO partner_bank_accounts (id, partner_id, bank_bin, bank_name, account_number, account_holder, is_current)
		VALUES ($1, $2, '970436', 'Vietcombank', '0123456789', 'NGUYEN VAN A', false)`, bank, partner)
	f.mustExec(t, `INSERT INTO payouts (id, partner_id, amount, status, bank_bin, bank_name, account_number, account_holder,
			bank_account_id, requested_by, requested_at)
		VALUES ($1, $2, $3, 'requested', '970436', 'Vietcombank', '0123456789', 'NGUYEN VAN A', $4, $5, now())`,
		id, partner, amount, bank, requestedBy)
	return id
}

// withAvailable900k is TestConfirmThenRelease: available 900.000, last seq 3.
func (f *fixture) withAvailable900k(t *testing.T) ledger.BookingMoney {
	t.Helper()
	b := f.newBooking(t, 1_000_000, 100_000)
	f.confirm(t, b)
	f.release(t, b)
	f.requireBalance(t, f.partner, 0, 900_000)
	return b
}

func (f *fixture) hold(partner, payout uuid.UUID, amount int64) (ledger.Posted, error) {
	var p ledger.Posted
	err := f.tryTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = f.svc.PostPayoutHold(ctx, tx, partner, payout, amount)
		return err
	})
	return p, err
}

func (f *fixture) reverse(partner, payout uuid.UUID, amount int64) (ledger.Posted, error) {
	var p ledger.Posted
	err := f.tryTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = f.svc.PostPayoutReversal(ctx, tx, partner, payout, amount)
		return err
	})
	return p, err
}

func (f *fixture) adjust(a ledger.Adjustment) (ledger.Posted, error) {
	var p ledger.Posted
	err := f.tryTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = f.svc.PostAdjustment(ctx, tx, a)
		return err
	})
	return p, err
}

func (f *fixture) lastSeq(t *testing.T, partner uuid.UUID) int64 {
	t.Helper()
	n, err := f.svc.LastSeq(context.Background(), partner)
	require.NoError(t, err)
	return n
}

func TestPayoutHold_TakesAvailable(t *testing.T) {
	f := newFixture(t)
	f.withAvailable900k(t)
	before := f.lastSeq(t, f.partner)
	payout := f.newPayout(t, f.partner, f.owner, 300_000)

	p, err := f.hold(f.partner, payout, 300_000)
	require.NoError(t, err)
	require.Equal(t, ledger.Posted{Seq: before + 1, AvailableAfter: 600_000}, p)
	f.requireBalance(t, f.partner, 0, 600_000)

	got := f.entries(t, f.partner)
	require.Len(t, got, 4)
	require.Equal(t, int64(4), got[3].Seq)
	require.Equal(t, "payout_hold", got[3].Kind)
	require.Equal(t, "available", got[3].Bucket)
	require.Equal(t, int64(-300_000), got[3].Amount)
	var payoutID uuid.UUID
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT payout_id FROM ledger_entries WHERE partner_id = $1 AND seq = 4`, f.partner).Scan(&payoutID))
	require.Equal(t, payout, payoutID)
}

func TestPayoutHold_ExceedsAvailable(t *testing.T) {
	f := newFixture(t)
	f.withAvailable900k(t)
	payout := f.newPayout(t, f.partner, f.owner, 900_001)

	_, err := f.hold(f.partner, payout, 900_001)
	require.ErrorIs(t, err, ledger.ErrExceedsAvailable)
	_, err = f.hold(f.partner, payout, 0)
	require.ErrorIs(t, err, ledger.ErrInvalidAmount)
	_, err = f.hold(f.partner, payout, -1)
	require.ErrorIs(t, err, ledger.ErrInvalidAmount)
	f.requireBalance(t, f.partner, 0, 900_000)

	// Exactly the available amount is allowed.
	p, err := f.hold(f.partner, payout, 900_000)
	require.NoError(t, err)
	require.Equal(t, int64(0), p.AvailableAfter)
	f.requireBalance(t, f.partner, 0, 0)
}

func TestPayoutHold_NothingAvailable(t *testing.T) {
	f := newFixture(t)
	// 100% refund then release: available −100.000.
	b := f.newBooking(t, 1_000_000, 100_000)
	f.confirm(t, b)
	f.refund(t, b, 1_000_000)
	f.release(t, b)
	f.requireBalance(t, f.partner, 0, -100_000)
	payout := f.newPayout(t, f.partner, f.owner, 100_000)
	_, err := f.hold(f.partner, payout, 100_000)
	require.ErrorIs(t, err, ledger.ErrNothingAvailable)
	f.requireBalance(t, f.partner, 0, -100_000)

	// Available 0, including a partner without any balance row yet.
	owner2, p2 := f.newPartner(t, "beta")
	payout2 := f.newPayout(t, p2, owner2, 100_000)
	_, err = f.hold(p2, payout2, 100_000)
	require.ErrorIs(t, err, ledger.ErrNothingAvailable)
	f.requireBalance(t, p2, 0, 0)
}

func TestPayoutHold_Twice(t *testing.T) {
	f := newFixture(t)
	f.withAvailable900k(t)
	payout := f.newPayout(t, f.partner, f.owner, 300_000)
	_, err := f.hold(f.partner, payout, 300_000)
	require.NoError(t, err)

	_, err = f.hold(f.partner, payout, 300_000)
	require.ErrorIs(t, err, ledger.ErrAlreadyPosted)
	f.requireBalance(t, f.partner, 0, 600_000)
	require.Len(t, f.entries(t, f.partner), 4)
}

func TestPayoutReversal_RestoresAvailable(t *testing.T) {
	f := newFixture(t)
	f.withAvailable900k(t)
	payout := f.newPayout(t, f.partner, f.owner, 300_000)
	_, err := f.hold(f.partner, payout, 300_000)
	require.NoError(t, err)

	p, err := f.reverse(f.partner, payout, 300_000)
	require.NoError(t, err)
	require.Equal(t, ledger.Posted{Seq: 5, AvailableAfter: 900_000}, p)
	f.requireBalance(t, f.partner, 0, 900_000)
	got := f.entries(t, f.partner)
	require.Equal(t, "payout_reversal", got[4].Kind)
	require.Equal(t, int64(300_000), got[4].Amount)

	_, err = f.reverse(f.partner, payout, 300_000)
	require.ErrorIs(t, err, ledger.ErrAlreadyPosted)
	_, err = f.reverse(f.partner, payout, 0)
	require.ErrorIs(t, err, ledger.ErrInvalidAmount)
	f.requireBalance(t, f.partner, 0, 900_000)
}

func TestAdjustment_PlusMinus(t *testing.T) {
	f := newFixture(t)
	b := f.withAvailable900k(t)
	admin := newID(t)
	f.mustExec(t, `INSERT INTO users (id, email, role) VALUES ($1, 'admin@lunain.test', 'super_admin')`, admin)

	p, err := f.adjust(ledger.Adjustment{PartnerID: f.partner, Amount: 50_000, Reason: "  Bù phí vận chuyển  ", CreatedBy: admin})
	require.NoError(t, err)
	require.Equal(t, ledger.Posted{Seq: 4, AvailableAfter: 950_000}, p)
	p, err = f.adjust(ledger.Adjustment{PartnerID: f.partner, BookingID: &b.BookingID, Amount: -70_000, Reason: "Trừ phí", CreatedBy: admin})
	require.NoError(t, err)
	require.Equal(t, ledger.Posted{Seq: 5, AvailableAfter: 880_000}, p)
	f.requireBalance(t, f.partner, 0, 880_000)

	rows, err := f.pool.Query(context.Background(), `SELECT kind, bucket, amount, reason, created_by, booking_id
		FROM ledger_entries WHERE partner_id = $1 AND seq >= 4 ORDER BY seq`, f.partner)
	require.NoError(t, err)
	type adj struct {
		Kind, Bucket string
		Amount       int64
		Reason       string
		CreatedBy    uuid.UUID
		BookingID    *uuid.UUID
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[adj])
	require.NoError(t, err)
	require.Equal(t, []adj{
		{"adjustment", "available", 50_000, "Bù phí vận chuyển", admin, nil},
		{"adjustment", "available", -70_000, "Trừ phí", admin, &b.BookingID},
	}, got)
}

func TestAdjustment_OtherPartnersBooking(t *testing.T) {
	f := newFixture(t)
	owner2, p2 := f.newPartner(t, "beta")
	other := f.newBookingOf(t, p2, f.newListing(t, p2), 500_000, 50_000)

	_, err := f.adjust(ledger.Adjustment{PartnerID: f.partner, BookingID: &other.BookingID, Amount: 10_000, Reason: "x", CreatedBy: owner2})
	require.ErrorIs(t, err, ledger.ErrBookingNotOfPartner)
	unknown := uuid.New()
	_, err = f.adjust(ledger.Adjustment{PartnerID: f.partner, BookingID: &unknown, Amount: 10_000, Reason: "x", CreatedBy: owner2})
	require.ErrorIs(t, err, ledger.ErrBookingNotOfPartner)
	f.requireBalance(t, f.partner, 0, 0)
	require.Empty(t, f.entries(t, f.partner))
}

func TestAdjustment_ZeroOrNoReason(t *testing.T) {
	f := newFixture(t)
	ok := ledger.Adjustment{PartnerID: f.partner, Amount: 1, Reason: "ok", CreatedBy: f.owner}

	a := ok
	a.Amount = 0
	_, err := f.adjust(a)
	require.ErrorIs(t, err, ledger.ErrInvalidAmount)
	a.Amount = 1_000_000_000_001
	_, err = f.adjust(a)
	require.ErrorIs(t, err, ledger.ErrInvalidAmount)
	a.Amount = -1_000_000_000_001
	_, err = f.adjust(a)
	require.ErrorIs(t, err, ledger.ErrInvalidAmount)

	a = ok
	a.Reason = "   "
	_, err = f.adjust(a)
	require.ErrorIs(t, err, ledger.ErrInvalidReason)
	a.Reason = strings.Repeat("ạ", 501)
	_, err = f.adjust(a)
	require.ErrorIs(t, err, ledger.ErrInvalidReason)
	f.requireBalance(t, f.partner, 0, 0)

	// Bounds are inclusive: 500 runes and ±1.000.000.000.000.
	a.Reason = strings.Repeat("ạ", 500)
	a.Amount = 1_000_000_000_000
	_, err = f.adjust(a)
	require.NoError(t, err)
	a.Amount = -1_000_000_000_000
	_, err = f.adjust(a)
	require.NoError(t, err)
	f.requireBalance(t, f.partner, 0, 0)
}
