package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/ledger/store"
)

// Drift is one disagreement VerifyAll found. BookingID is set when the
// problem is a booking settlement, nil when it is the partner's balance cache
// or entry sequence.
type Drift struct {
	PartnerID uuid.UUID  `json:"partner_id"`
	BookingID *uuid.UUID `json:"booking_id"`
	Problem   string     `json:"problem"`
}

// VerifyAll checks the whole ledger in one read-only snapshot:
//  1. Verify for every partner with a balance row or an entry;
//  2. every open booking settlement's net_pending equals the sum of the
//     booking's pending entries;
//  3. every released settlement has its pending entries summing to 0
//     (spec §9);
//  4. no pending entry belongs to a booking without a settlement row.
//
// Mismatches are returned as Drifts; err is only for a failed query.
func (s *Service) VerifyAll(ctx context.Context) ([]Drift, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin verify all tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := store.New(tx)

	partners, err := q.ListLedgerPartners(ctx)
	if err != nil {
		return nil, fmt.Errorf("list ledger partners: %w", err)
	}
	var drifts []Drift
	for _, p := range partners {
		problem, err := verifyPartner(ctx, q, p)
		if err != nil {
			return nil, err
		}
		if problem != "" {
			drifts = append(drifts, Drift{PartnerID: p, Problem: problem})
		}
	}

	settlements, err := q.SettlementDrifts(ctx)
	if err != nil {
		return nil, fmt.Errorf("settlement drifts: %w", err)
	}
	for _, st := range settlements {
		var problem string
		if st.Released {
			problem = fmt.Sprintf("ledger: booking %s released with %d still in pending entries",
				st.BookingID, st.EntriesPending)
		} else {
			problem = fmt.Sprintf("ledger: booking %s net_pending %d, pending entries %d",
				st.BookingID, st.NetPending, st.EntriesPending)
		}
		bookingID := st.BookingID
		drifts = append(drifts, Drift{PartnerID: st.PartnerID, BookingID: &bookingID, Problem: problem})
	}

	orphans, err := q.OrphanPendingEntries(ctx)
	if err != nil {
		return nil, fmt.Errorf("orphan pending entries: %w", err)
	}
	for _, o := range orphans {
		bookingID := o.BookingID
		drifts = append(drifts, Drift{PartnerID: o.PartnerID, BookingID: &bookingID,
			Problem: fmt.Sprintf("ledger: booking %s has %d in pending entries but no settlement", o.BookingID, o.Pending)})
	}
	return drifts, nil
}
