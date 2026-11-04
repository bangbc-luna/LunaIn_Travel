package listing

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	maxTiers       = 10
	maxHoursBefore = 8760
	maxRefundBps   = 10000

	actionCancellationPolicy = "cancellation_policy"
)

// ErrInvalidCancellationPolicy is the sentinel of invalid refund tiers.
var ErrInvalidCancellationPolicy = httpx.NewError(http.StatusUnprocessableEntity, "invalid_cancellation_policy", "Chính sách hủy không hợp lệ")

func invalidPolicy(msg string) error {
	return &fieldError{
		app:      httpx.NewError(ErrInvalidCancellationPolicy.Status, ErrInvalidCancellationPolicy.Code, msg),
		sentinel: ErrInvalidCancellationPolicy,
	}
}

// CancellationTier says that a cancellation at least HoursBefore hours before
// the start refunds RefundBps basis points (10000 = 100%).
type CancellationTier struct {
	HoursBefore int `json:"hours_before"`
	RefundBps   int `json:"refund_bps"`
}

// ValidateTiers checks a policy and returns it sorted by HoursBefore
// descending. The refund must not increase as the start gets closer. An empty
// policy is valid and means no refund.
func ValidateTiers(t []CancellationTier) ([]CancellationTier, error) {
	if len(t) > maxTiers {
		return nil, invalidPolicy(fmt.Sprintf("Tối đa %d mốc hoàn tiền", maxTiers))
	}
	out := append([]CancellationTier{}, t...)
	for _, x := range out {
		if x.HoursBefore < 0 || x.HoursBefore > maxHoursBefore {
			return nil, invalidPolicy(fmt.Sprintf("hours_before phải nằm trong khoảng 0..%d", maxHoursBefore))
		}
		if x.RefundBps < 0 || x.RefundBps > maxRefundBps {
			return nil, invalidPolicy(fmt.Sprintf("refund_bps phải nằm trong khoảng 0..%d", maxRefundBps))
		}
	}
	slices.SortFunc(out, func(a, b CancellationTier) int { return b.HoursBefore - a.HoursBefore })
	for i := 1; i < len(out); i++ {
		if out[i].HoursBefore == out[i-1].HoursBefore {
			return nil, invalidPolicy(fmt.Sprintf("Mốc %d giờ bị trùng", out[i].HoursBefore))
		}
		if out[i].RefundBps > out[i-1].RefundBps {
			return nil, invalidPolicy("Càng sát giờ bắt đầu thì tỷ lệ hoàn tiền không được tăng")
		}
	}
	return out, nil
}

// RefundBps returns the refund in basis points for a cancellation untilStart
// before the start: the tier with the largest HoursBefore such that
// untilStart >= HoursBefore hours. No matching tier, or a start that already
// passed (untilStart < 0), refunds 0. Pure: booking passes its stored snapshot.
func RefundBps(tiers []CancellationTier, untilStart time.Duration) int {
	if untilStart < 0 {
		return 0
	}
	best, bps := -1, 0
	for _, t := range tiers {
		if untilStart >= time.Duration(t.HoursBefore)*time.Hour && t.HoursBefore > best {
			best, bps = t.HoursBefore, t.RefundBps
		}
	}
	return bps
}

// CancellationPolicy returns the listing's tiers, HoursBefore descending.
func (s *Service) CancellationPolicy(ctx context.Context, tx pgx.Tx, listingID uuid.UUID) ([]CancellationTier, error) {
	return listTiers(ctx, tx, listingID)
}

func listTiers(ctx context.Context, q store.DBTX, listingID uuid.UUID) ([]CancellationTier, error) {
	rows, err := store.New(q).ListCancellationTiers(ctx, listingID)
	if err != nil {
		return nil, fmt.Errorf("list cancellation tiers: %w", err)
	}
	out := make([]CancellationTier, len(rows))
	for i, r := range rows {
		out[i] = CancellationTier{HoursBefore: int(r.HoursBefore), RefundBps: int(r.RefundBps)}
	}
	return out, nil
}

// SetCancellationPolicy replaces all tiers of the partner's listing and
// returns them sorted by HoursBefore descending.
func (s *Service) SetCancellationPolicy(ctx context.Context, partnerID, actor, id uuid.UUID, tiers []CancellationTier) ([]CancellationTier, error) {
	valid, err := ValidateTiers(tiers)
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.requireActivePartner(ctx, tx, partnerID); err != nil {
			return err
		}
		if _, err := s.lockOwned(ctx, tx, partnerID, id); err != nil {
			return err
		}
		old, err := listTiers(ctx, tx, id)
		if err != nil {
			return err
		}
		q := store.New(tx)
		if err := q.DeleteCancellationTiers(ctx, id); err != nil {
			return fmt.Errorf("delete cancellation tiers: %w", err)
		}
		for _, t := range valid {
			rowID, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("cancellation tier id: %w", err)
			}
			if err := q.InsertCancellationTier(ctx, store.InsertCancellationTierParams{
				ID: rowID, ListingID: id, HoursBefore: int32(t.HoursBefore), RefundBps: int32(t.RefundBps), Now: now,
			}); err != nil {
				return fmt.Errorf("insert cancellation tier: %w", err)
			}
		}
		return s.logChange(ctx, tx, id, actor, actionCancellationPolicy, map[string]change{
			"tiers": {old, valid},
		}, now)
	})
	if err != nil {
		return nil, err
	}
	return valid, nil
}
