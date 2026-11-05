package ledger_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/ledger"
)

func TestStatement_OnlyAvailableInRange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	require.Equal(t, int64(0), f.lastSeq(t, f.partner))
	b := f.newBooking(t, 1_000_000, 100_000)
	// seq 1 pending; seq 2 pending + seq 3 available; seq 4 adjustment.
	f.confirm(t, b)
	f.release(t, b)
	_, err := f.adjust(ledger.Adjustment{PartnerID: f.partner, Amount: -20_000, Reason: "Phạt", CreatedBy: f.owner})
	require.NoError(t, err)

	last := f.lastSeq(t, f.partner)
	require.Equal(t, int64(4), last)
	lines, err := f.svc.Statement(ctx, f.partner, 1, last)
	require.NoError(t, err)
	require.Len(t, lines, 2)

	require.Equal(t, int64(3), lines[0].Seq)
	require.Equal(t, "release", lines[0].Kind)
	require.Equal(t, int64(900_000), lines[0].Amount)
	require.Equal(t, int64(900_000), lines[0].AvailableAfter)
	require.Equal(t, &b.BookingID, lines[0].BookingID)
	require.Nil(t, lines[0].PayoutID)
	require.Nil(t, lines[0].Reason)

	require.Equal(t, int64(4), lines[1].Seq)
	require.Equal(t, "adjustment", lines[1].Kind)
	require.Equal(t, int64(-20_000), lines[1].Amount)
	require.Equal(t, int64(880_000), lines[1].AvailableAfter)
	require.Nil(t, lines[1].BookingID)
	require.NotNil(t, lines[1].Reason)
	require.Equal(t, "Phạt", *lines[1].Reason)
	require.True(t, lines[1].CreatedAt.Equal(testNow))

	// The range is inclusive and bounded.
	lines, err = f.svc.Statement(ctx, f.partner, 4, 4)
	require.NoError(t, err)
	require.Len(t, lines, 1)
	lines, err = f.svc.Statement(ctx, f.partner, 1, 2)
	require.NoError(t, err)
	require.Empty(t, lines)

	// opening + Σ lines = closing.
	var opening, closing int64
	f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if opening, err = f.svc.AvailableAfter(ctx, tx, f.partner, 2); err != nil {
			return err
		}
		closing, err = f.svc.AvailableAfter(ctx, tx, f.partner, 4)
		return err
	})
	require.Equal(t, int64(0), opening)
	require.Equal(t, int64(880_000), closing)
	lines, err = f.svc.Statement(ctx, f.partner, 3, 4)
	require.NoError(t, err)
	sum := opening
	for _, l := range lines {
		sum += l.Amount
	}
	require.Equal(t, closing, sum)
}

func TestAvailableAfter_SeqZeroIsZero(t *testing.T) {
	f := newFixture(t)
	f.withAvailable900k(t)
	f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		v, err := f.svc.AvailableAfter(ctx, tx, f.partner, 0)
		require.NoError(t, err)
		require.Equal(t, int64(0), v)
		v, err = f.svc.AvailableAfter(ctx, tx, f.partner, 3)
		require.NoError(t, err)
		require.Equal(t, int64(900_000), v)
		return nil
	})
	// A seq that does not exist is an error.
	require.Error(t, f.tryTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.AvailableAfter(ctx, tx, f.partner, 4)
		return err
	}))
	require.Error(t, f.tryTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.AvailableAfter(ctx, tx, f.partner, -1)
		return err
	}))
}
