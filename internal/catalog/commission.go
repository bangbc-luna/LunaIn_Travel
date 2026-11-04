package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/catalog/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

const (
	maxRateBps = 10000
	// backdateTolerance absorbs client/server clock skew; anything older is a
	// backdated rate, which would silently change the meaning of past orders.
	backdateTolerance = time.Minute
)

// CommissionRate is one row of the append-only rate history.
type CommissionRate struct {
	ID            uuid.UUID `json:"id"`
	ServiceType   string    `json:"service_type"`
	RateBps       int       `json:"rate_bps"`
	EffectiveFrom time.Time `json:"effective_from"`
	CreatedBy     uuid.UUID `json:"created_by"`
}

func fromRateRow(r store.ListCommissionRatesRow) CommissionRate {
	return CommissionRate{ID: r.ID, ServiceType: r.ServiceType, RateBps: int(r.RateBps), EffectiveFrom: r.EffectiveFrom, CreatedBy: r.CreatedBy}
}

// CreateCommissionRate appends a rate effective from effectiveFrom (default
// now). Backdating beyond one minute and rates outside 0..10000 are rejected.
// A duplicate (service_type, effective_from) is ErrCommissionRateExists, also
// under concurrent creation (ON CONFLICT DO NOTHING).
func (s *Service) CreateCommissionRate(ctx context.Context, actor uuid.UUID, serviceType string, rateBps int, effectiveFrom *time.Time) (CommissionRate, error) {
	now := s.clock.Now()
	from := now
	if effectiveFrom != nil {
		from = *effectiveFrom
	}
	if rateBps < 0 || rateBps > maxRateBps || from.Before(now.Add(-backdateTolerance)) {
		return CommissionRate{}, ErrInvalidCommission
	}
	id, err := uuid.NewV7()
	if err != nil {
		return CommissionRate{}, fmt.Errorf("commission rate id: %w", err)
	}
	cr := CommissionRate{ID: id, ServiceType: serviceType, RateBps: rateBps, EffectiveFrom: from.UTC(), CreatedBy: actor}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.ServiceType(ctx, tx, serviceType); err != nil {
			return err
		}
		n, err := store.New(tx).InsertCommissionRate(ctx, store.InsertCommissionRateParams{
			ID: id, ServiceType: serviceType, RateBps: int32(rateBps), EffectiveFrom: cr.EffectiveFrom, CreatedBy: actor, Now: now,
		})
		if err != nil {
			return fmt.Errorf("insert commission rate: %w", err)
		}
		if n == 0 {
			return ErrCommissionRateExists
		}
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor,
			Action:      "commission_rate.create",
			TargetKind:  "commission_rate",
			TargetID:    &id,
			After:       cr,
		})
	})
	if err != nil {
		return CommissionRate{}, err
	}
	return cr, nil
}

// CurrentCommissionRate returns the rate in force at `at`: the row with the
// greatest effective_from <= at. None is ErrNoCommissionRate.
func (s *Service) CurrentCommissionRate(ctx context.Context, tx pgx.Tx, serviceType string, at time.Time) (int, error) {
	bps, err := store.New(tx).CurrentCommissionRate(ctx, store.CurrentCommissionRateParams{ServiceType: serviceType, At: at})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoCommissionRate
	}
	if err != nil {
		return 0, fmt.Errorf("current commission rate: %w", err)
	}
	return int(bps), nil
}

// CommissionHistory lists a service type's rates, newest effective_from first.
// An unknown service type is ErrInvalidServiceType.
func (s *Service) CommissionHistory(ctx context.Context, serviceType string) ([]CommissionRate, error) {
	var out []CommissionRate
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.ServiceType(ctx, tx, serviceType); err != nil {
			return err
		}
		rows, err := store.New(tx).ListCommissionRates(ctx, serviceType)
		if err != nil {
			return fmt.Errorf("list commission rates: %w", err)
		}
		out = make([]CommissionRate, 0, len(rows))
		for _, r := range rows {
			out = append(out, fromRateRow(r))
		}
		return nil
	})
	return out, err
}
