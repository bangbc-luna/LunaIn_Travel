package ledger_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/ledger"
)

// twoPartners gives both the fixture partner and a second one entries: a
// released booking, an open booking and an adjustment.
func (f *fixture) twoPartners(t *testing.T) (p2 ledger.BookingMoney, open ledger.BookingMoney) {
	t.Helper()
	b := f.newBooking(t, 1_000_000, 100_000)
	f.confirm(t, b)
	f.release(t, b)
	open = f.newBooking(t, 500_000, 50_000)
	f.confirm(t, open)
	f.refund(t, open, 100_000)

	owner2, partner2 := f.newPartner(t, "beta")
	p2 = f.newBookingOf(t, partner2, f.newListing(t, partner2), 300_000, 30_000)
	f.confirm(t, p2)
	_, err := f.adjust(ledger.Adjustment{PartnerID: partner2, Amount: 10_000, Reason: "Thưởng", CreatedBy: owner2})
	require.NoError(t, err)
	return p2, open
}

func TestVerifyAll_Clean(t *testing.T) {
	f := newFixture(t)
	f.twoPartners(t)
	drifts, err := f.svc.VerifyAll(context.Background())
	require.NoError(t, err)
	require.Empty(t, drifts)
}

func TestVerifyAll_DetectsCacheDrift(t *testing.T) {
	f := newFixture(t)
	p2, _ := f.twoPartners(t)
	f.mustExec(t, `UPDATE partner_balances SET available = available + 1 WHERE partner_id = $1`, p2.PartnerID)

	drifts, err := f.svc.VerifyAll(context.Background())
	require.NoError(t, err)
	require.Len(t, drifts, 1)
	require.Equal(t, p2.PartnerID, drifts[0].PartnerID)
	require.Nil(t, drifts[0].BookingID)
	require.NotEmpty(t, drifts[0].Problem)
}

func TestVerifyAll_DetectsSettlementDrift(t *testing.T) {
	f := newFixture(t)
	_, open := f.twoPartners(t)
	f.mustExec(t, `UPDATE booking_settlements SET net_pending = net_pending + 1 WHERE booking_id = $1`, open.BookingID)

	drifts, err := f.svc.VerifyAll(context.Background())
	require.NoError(t, err)
	require.Len(t, drifts, 1)
	require.Equal(t, f.partner, drifts[0].PartnerID)
	require.NotNil(t, drifts[0].BookingID)
	require.Equal(t, open.BookingID, *drifts[0].BookingID)
	require.NotEmpty(t, drifts[0].Problem)
}

// A released settlement must have no money left in pending (spec §9).
func TestVerifyAll_DetectsPendingLeftAfterRelease(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)
	f.confirm(t, b)
	// Close the settlement behind the ledger's back: 900.000 stays in pending.
	f.mustExec(t, `UPDATE booking_settlements SET net_pending = 0, released_at = now() WHERE booking_id = $1`, b.BookingID)

	drifts, err := f.svc.VerifyAll(context.Background())
	require.NoError(t, err)
	require.Len(t, drifts, 1)
	require.NotNil(t, drifts[0].BookingID)
	require.Equal(t, b.BookingID, *drifts[0].BookingID)
}
