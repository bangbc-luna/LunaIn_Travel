package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/ledger/store"
)

const (
	kindPayoutHold     = "payout_hold"
	kindPayoutReversal = "payout_reversal"
	kindAdjustment     = "adjustment"

	// MaxAdjustment bounds |Adjustment.Amount| (VND).
	MaxAdjustment int64 = 1_000_000_000_000
	// MaxAdjustmentReason is the longest adjustment reason, in runes.
	MaxAdjustmentReason = 500
)

// PostPayoutHold takes amount out of the partner's available balance for a
// payout: payout_hold −amount in available. amount must be > 0 (else
// ErrInvalidAmount); the balance, read under the partner_balances lock, must
// be > 0 (else ErrNothingAvailable) and ≥ amount (else ErrExceedsAvailable).
// Neither check writes anything, so the tx stays usable.
//
// The caller MUST hold the payouts row (locked before partner_balances, DB
// §1.1). A second hold of the same payout is ErrAlreadyPosted (tx aborted).
func (s *Service) PostPayoutHold(ctx context.Context, tx pgx.Tx, partnerID, payoutID uuid.UUID, amount int64) (Posted, error) {
	if amount <= 0 {
		return Posted{}, fmt.Errorf("%w: payout hold %d", ErrInvalidAmount, amount)
	}
	q := store.New(tx)
	now := s.clock.Now()
	bal, err := lockBalance(ctx, q, partnerID, now)
	if err != nil {
		return Posted{}, err
	}
	switch {
	case bal.Available <= 0:
		return Posted{}, fmt.Errorf("%w: available %d", ErrNothingAvailable, bal.Available)
	case amount > bal.Available:
		return Posted{}, fmt.Errorf("%w: payout %d, available %d", ErrExceedsAvailable, amount, bal.Available)
	}
	return s.postOne(ctx, q, partnerID, now, entry{
		kind: kindPayoutHold, bucket: bucketAvailable, amount: -amount, payoutID: &payoutID,
	})
}

// PostPayoutReversal gives a rejected payout's amount back: payout_reversal
// +amount in available, whatever the balance is now (it may be negative).
// amount must be > 0 (else ErrInvalidAmount). The caller MUST hold the
// payouts row. A second reversal of the same payout is ErrAlreadyPosted (tx
// aborted).
func (s *Service) PostPayoutReversal(ctx context.Context, tx pgx.Tx, partnerID, payoutID uuid.UUID, amount int64) (Posted, error) {
	if amount <= 0 {
		return Posted{}, fmt.Errorf("%w: payout reversal %d", ErrInvalidAmount, amount)
	}
	return s.postOne(ctx, store.New(tx), partnerID, s.clock.Now(), entry{
		kind: kindPayoutReversal, bucket: bucketAvailable, amount: amount, payoutID: &payoutID,
	})
}

// Adjustment is a manual Super Admin correction of a partner's available
// balance.
type Adjustment struct {
	PartnerID uuid.UUID
	// BookingID optionally ties the adjustment to one of the partner's bookings.
	BookingID *uuid.UUID
	// Amount is signed, non-zero and at most MaxAdjustment in absolute value.
	Amount int64
	// Reason is required: 1..MaxAdjustmentReason runes after trimming.
	Reason    string
	CreatedBy uuid.UUID
}

// PostAdjustment records a: adjustment Amount in available with the trimmed
// reason and its author. Invalid input is ErrInvalidAmount / ErrInvalidReason
// (nothing written). A booking that is unknown or another partner's is
// ErrBookingNotOfPartner (tx aborted).
func (s *Service) PostAdjustment(ctx context.Context, tx pgx.Tx, a Adjustment) (Posted, error) {
	if a.Amount == 0 || a.Amount > MaxAdjustment || a.Amount < -MaxAdjustment {
		return Posted{}, fmt.Errorf("%w: adjustment %d", ErrInvalidAmount, a.Amount)
	}
	reason := strings.TrimSpace(a.Reason)
	if n := utf8.RuneCountInString(reason); n == 0 || n > MaxAdjustmentReason || !utf8.ValidString(reason) {
		return Posted{}, ErrInvalidReason
	}
	if a.CreatedBy == uuid.Nil {
		return Posted{}, errors.New("ledger: adjustment without author")
	}
	createdBy := a.CreatedBy
	return s.postOne(ctx, store.New(tx), a.PartnerID, s.clock.Now(), entry{
		kind: kindAdjustment, bucket: bucketAvailable, amount: a.Amount,
		bookingID: a.BookingID, createdBy: &createdBy, reason: &reason,
	})
}

// postOne posts a single non-zero entry.
func (s *Service) postOne(ctx context.Context, q *store.Queries, partnerID uuid.UUID, now time.Time, e entry) (Posted, error) {
	posted, err := s.post(ctx, q, partnerID, now, e)
	if err != nil {
		return Posted{}, err
	}
	if len(posted) != 1 {
		return Posted{}, fmt.Errorf("ledger: %s posted %d entries", e.kind, len(posted))
	}
	return posted[0], nil
}
