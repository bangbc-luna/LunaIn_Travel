package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/ledger"
)

// A second posting of the same kind for a booking is ErrAlreadyPosted and
// changes nothing.
func TestDuplicatePostsAreErrAlreadyPosted(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)
	f.confirm(t, b)

	require.ErrorIs(t, f.tryTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostBookingConfirmed(ctx, tx, b)
	}), ledger.ErrAlreadyPosted)

	f.refund(t, b, 500_000)
	require.ErrorIs(t, f.tryTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostRefund(ctx, tx, b.BookingID, b.PartnerID, 500_000)
	}), ledger.ErrAlreadyPosted)

	f.waive(t, b)
	require.ErrorIs(t, f.tryTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostCommissionWaiver(ctx, tx, b.BookingID, b.PartnerID, attraction, 100_000)
	}), ledger.ErrAlreadyPosted)

	f.requireBalance(t, f.partner, 500_000, 0)
	require.Len(t, f.entries(t, f.partner), 3)
	require.Equal(t, int64(500_000), f.settlement(t, b.BookingID).NetPending)
}

// The waiver must give back exactly the commission the platform took.
func TestWaiverMustMatchCommission(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)
	f.confirm(t, b)
	require.ErrorIs(t, f.tryTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostCommissionWaiver(ctx, tx, b.BookingID, b.PartnerID, attraction, 99_999)
	}), ledger.ErrInvalidAmount)
	require.ErrorIs(t, f.tryTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostCommissionWaiver(ctx, tx, b.BookingID, b.PartnerID, attraction, 0)
	}), ledger.ErrInvalidAmount)

	// 0% commission: there is nothing to waive.
	z := f.newBooking(t, 200_000, 0)
	f.confirm(t, z)
	require.ErrorIs(t, f.tryTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostCommissionWaiver(ctx, tx, z.BookingID, z.PartnerID, attraction, 1)
	}), ledger.ErrInvalidAmount)
	f.requireBalance(t, f.partner, 1_100_000, 0)
}

// Verify recomputes available_after for every entry, not only the last.
func TestVerifyDetectsAvailableAfterDriftMidStream(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)
	f.confirm(t, b)
	require.NoError(t, f.svc.Verify(context.Background(), f.partner))

	// seq 2 claims 50 instead of 100; seq 3 brings the running total back to
	// 0, so the cache and the last available_after still agree.
	f.mustExec(t, `INSERT INTO ledger_entries (id, partner_id, seq, available_after, kind, bucket, amount, reason, created_by)
		VALUES ($1, $2, 2, 50, 'adjustment', 'available', 100, 'test', $3),
		       ($4, $2, 3, 0, 'adjustment', 'available', -100, 'test', $3)`,
		newID(t), f.partner, f.owner, newID(t))
	f.mustExec(t, `UPDATE partner_balances SET last_seq = 3 WHERE partner_id = $1`, f.partner)
	err := f.svc.Verify(context.Background(), f.partner)
	require.Error(t, err)
	require.Contains(t, err.Error(), "available_after")
}

// Scheduling the release of an already released booking does nothing.
func TestScheduleReleaseAfterReleaseIsNoop(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)
	f.confirm(t, b)
	f.release(t, b)
	f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.ScheduleRelease(ctx, tx, b.BookingID, testNow.Add(72*time.Hour))
	})
	require.Nil(t, f.settlement(t, b.BookingID).ReleaseDueAt)
}
