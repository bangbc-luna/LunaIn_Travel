package listing_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
)

func TestValidateTiers(t *testing.T) {
	tiers := func(n int) []listing.CancellationTier {
		out := make([]listing.CancellationTier, n)
		for i := range out {
			out[i] = listing.CancellationTier{HoursBefore: i + 1, RefundBps: 0}
		}
		return out
	}
	cases := []struct {
		name string
		in   []listing.CancellationTier
		ok   bool
	}{
		{"valid", []listing.CancellationTier{{HoursBefore: 24, RefundBps: 5000}, {HoursBefore: 48, RefundBps: 10000}}, true},
		{"empty", nil, true},
		{"equal bps", []listing.CancellationTier{{HoursBefore: 48, RefundBps: 5000}, {HoursBefore: 24, RefundBps: 5000}}, true},
		{"duplicate", []listing.CancellationTier{{HoursBefore: 48, RefundBps: 10000}, {HoursBefore: 48, RefundBps: 5000}}, false},
		{"increasing refund closer to start", []listing.CancellationTier{{HoursBefore: 48, RefundBps: 5000}, {HoursBefore: 24, RefundBps: 10000}}, false},
		{"11 tiers", tiers(11), false},
		{"10 tiers", tiers(10), true},
		{"bps too big", []listing.CancellationTier{{HoursBefore: 48, RefundBps: 10001}}, false},
		{"bps negative", []listing.CancellationTier{{HoursBefore: 48, RefundBps: -1}}, false},
		{"hours negative", []listing.CancellationTier{{HoursBefore: -1, RefundBps: 0}}, false},
		{"hours too big", []listing.CancellationTier{{HoursBefore: 8761, RefundBps: 0}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := listing.ValidateTiers(c.in)
			if c.ok {
				require.NoError(t, err)
				for i := 1; i < len(got); i++ {
					require.Greater(t, got[i-1].HoursBefore, got[i].HoursBefore)
				}
				return
			}
			status, code := appCode(t, err)
			require.Equal(t, http.StatusUnprocessableEntity, status)
			require.Equal(t, "invalid_cancellation_policy", code)
		})
	}
	got, err := listing.ValidateTiers([]listing.CancellationTier{{HoursBefore: 24, RefundBps: 5000}, {HoursBefore: 48, RefundBps: 10000}})
	require.NoError(t, err)
	require.Equal(t, []listing.CancellationTier{{HoursBefore: 48, RefundBps: 10000}, {HoursBefore: 24, RefundBps: 5000}}, got)
}

func TestRefundBps_Boundaries(t *testing.T) {
	tiers := []listing.CancellationTier{{HoursBefore: 48, RefundBps: 10000}, {HoursBefore: 24, RefundBps: 5000}}
	h := time.Hour
	require.Equal(t, 10000, listing.RefundBps(tiers, 48*h))
	require.Equal(t, 10000, listing.RefundBps(tiers, 100*h))
	require.Equal(t, 5000, listing.RefundBps(tiers, 48*h-time.Minute))
	require.Equal(t, 5000, listing.RefundBps(tiers, 24*h))
	require.Equal(t, 0, listing.RefundBps(tiers, 24*h-time.Minute))
	require.Equal(t, 0, listing.RefundBps(tiers, -time.Minute))
	require.Equal(t, 0, listing.RefundBps(nil, 100*h))
	// Order of the input slice does not matter.
	require.Equal(t, 10000, listing.RefundBps([]listing.CancellationTier{{HoursBefore: 24, RefundBps: 5000}, {HoursBefore: 48, RefundBps: 10000}}, 48*h))
	// A zero-hour tier applies up to the start itself, but not after it.
	zero := []listing.CancellationTier{{HoursBefore: 0, RefundBps: 3000}}
	require.Equal(t, 3000, listing.RefundBps(zero, 0))
	require.Equal(t, 0, listing.RefundBps(zero, -time.Second))
}

func TestSetCancellationPolicy_ReplacesAndAudits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")

	got, err := e.Svc.SetCancellationPolicy(ctx, p.ID, p.OwnerUserID, l.ID,
		[]listing.CancellationTier{{HoursBefore: 24, RefundBps: 5000}, {HoursBefore: 48, RefundBps: 10000}})
	require.NoError(t, err)
	require.Equal(t, []listing.CancellationTier{{HoursBefore: 48, RefundBps: 10000}, {HoursBefore: 24, RefundBps: 5000}}, got)

	got, err = e.Svc.SetCancellationPolicy(ctx, p.ID, p.OwnerUserID, l.ID, []listing.CancellationTier{{HoursBefore: 72, RefundBps: 8000}})
	require.NoError(t, err)
	require.Equal(t, []listing.CancellationTier{{HoursBefore: 72, RefundBps: 8000}}, got)
	require.Equal(t, got, e.policy(t, l.ID))

	// Invalid input changes nothing and writes no audit row.
	_, err = e.Svc.SetCancellationPolicy(ctx, p.ID, p.OwnerUserID, l.ID,
		[]listing.CancellationTier{{HoursBefore: 48, RefundBps: 100}, {HoursBefore: 24, RefundBps: 200}})
	require.Error(t, err)
	require.Equal(t, got, e.policy(t, l.ID))

	audits := e.audits(t, l.ID)
	require.Len(t, audits, 3) // create + 2 policy changes
	last := audits[2]
	require.Equal(t, "cancellation_policy", last.Action)
	require.Equal(t, p.OwnerUserID, last.Actor)
	require.Equal(t, []any{
		map[string]any{"hours_before": float64(48), "refund_bps": float64(10000)},
		map[string]any{"hours_before": float64(24), "refund_bps": float64(5000)},
	}, last.Changes["tiers"].Old)
	require.Equal(t, []any{map[string]any{"hours_before": float64(72), "refund_bps": float64(8000)}}, last.Changes["tiers"].New)

	// An empty list is valid: no refunds.
	got, err = e.Svc.SetCancellationPolicy(ctx, p.ID, p.OwnerUserID, l.ID, nil)
	require.NoError(t, err)
	require.Empty(t, got)
	require.Zero(t, e.count(t, `SELECT count(*) FROM listing_cancellation_tiers WHERE listing_id = $1`, l.ID))
}

func TestSetCancellationPolicy_SuspendedAndForeign(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.partner(t, "Alpha", attraction)
	b := e.partner(t, "Beta", attraction)
	l := e.create(t, a, "Vé A")
	tiers := []listing.CancellationTier{{HoursBefore: 24, RefundBps: 5000}}

	_, err := e.Svc.SetCancellationPolicy(ctx, b.ID, b.OwnerUserID, l.ID, tiers)
	require.ErrorIs(t, err, listing.ErrListingNotFound)

	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, a.ID, "test"))
	_, err = e.Svc.SetCancellationPolicy(ctx, a.ID, a.OwnerUserID, l.ID, tiers)
	require.ErrorIs(t, err, listing.ErrPartnerSuspended)
	require.Zero(t, e.count(t, `SELECT count(*) FROM listing_cancellation_tiers`))
}
