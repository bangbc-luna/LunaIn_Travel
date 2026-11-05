package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/ledger/store"
)

// StatementLine is one available-bucket entry of a statement.
type StatementLine struct {
	Seq            int64      `json:"seq"`
	Kind           string     `json:"kind"`
	Amount         int64      `json:"amount"`
	AvailableAfter int64      `json:"available_after"`
	BookingID      *uuid.UUID `json:"booking_id"`
	PayoutID       *uuid.UUID `json:"payout_id"`
	Reason         *string    `json:"reason"`
	CreatedAt      time.Time  `json:"created_at"`
}

// LedgerLine is any ledger entry (either bucket), as the admin ledger shows it.
type LedgerLine struct {
	StatementLine
	Bucket string `json:"bucket"`
}

// AvailableAfter returns the partner's available balance right after entry
// seq, read in tx; seq 0 (before the first entry) is 0. An unknown seq is
// ErrEntryNotFound.
func (s *Service) AvailableAfter(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, seq int64) (int64, error) {
	if seq == 0 {
		return 0, nil
	}
	if seq < 0 {
		return 0, fmt.Errorf("%w: seq %d", ErrEntryNotFound, seq)
	}
	v, err := store.New(tx).GetAvailableAfter(ctx, store.GetAvailableAfterParams{PartnerID: partnerID, Seq: seq})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: partner %s seq %d", ErrEntryNotFound, partnerID, seq)
	}
	if err != nil {
		return 0, fmt.Errorf("get available_after: %w", err)
	}
	return v, nil
}

// Statement returns the partner's available-bucket entries with
// fromSeq <= seq <= toSeq, in seq order. Pending entries never appear: a
// statement explains the available balance only.
func (s *Service) Statement(ctx context.Context, partnerID uuid.UUID, fromSeq, toSeq int64) ([]StatementLine, error) {
	rows, err := store.New(s.pool).ListStatementLines(ctx, store.ListStatementLinesParams{
		PartnerID: partnerID, FromSeq: fromSeq, ToSeq: toSeq,
	})
	if err != nil {
		return nil, fmt.Errorf("list statement lines: %w", err)
	}
	out := make([]StatementLine, len(rows))
	for i, r := range rows {
		out[i] = StatementLine{
			Seq: r.Seq, Kind: r.Kind, Amount: r.Amount, AvailableAfter: r.AvailableAfter,
			BookingID: r.BookingID, PayoutID: r.PayoutID, Reason: r.Reason, CreatedAt: r.CreatedAt,
		}
	}
	return out, nil
}

// LastSeq returns the partner's highest ledger seq; 0 before any entry.
func (s *Service) LastSeq(ctx context.Context, partnerID uuid.UUID) (int64, error) {
	n, err := store.New(s.pool).LastSeq(ctx, partnerID)
	if err != nil {
		return 0, fmt.Errorf("last seq: %w", err)
	}
	return n, nil
}

// entriesAfter returns up to limit entries (both buckets) with seq > afterSeq,
// in seq order.
func entriesAfter(ctx context.Context, q *store.Queries, partnerID uuid.UUID, afterSeq int64, limit int32) ([]LedgerLine, error) {
	rows, err := q.ListLedgerEntries(ctx, store.ListLedgerEntriesParams{
		PartnerID: partnerID, AfterSeq: afterSeq, Lim: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list ledger entries: %w", err)
	}
	out := make([]LedgerLine, len(rows))
	for i, r := range rows {
		out[i] = LedgerLine{
			StatementLine: StatementLine{
				Seq: r.Seq, Kind: r.Kind, Amount: r.Amount, AvailableAfter: r.AvailableAfter,
				BookingID: r.BookingID, PayoutID: r.PayoutID, Reason: r.Reason, CreatedAt: r.CreatedAt,
			},
			Bucket: r.Bucket,
		}
	}
	return out, nil
}
