package booking

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
)

// SetJobHooks overrides the job batch size (0 keeps the default) and sets the
// per-item hook run inside each item's transaction (nil clears it).
func (s *Service) SetJobHooks(batch int32, before func(id uuid.UUID) error) {
	s.batchSize = batch
	s.beforeItem = before
}

// SetBeforeRentalLock sets the hook HandlePaid runs just before it locks a
// rental checkout's vehicle models (nil clears it).
func (s *Service) SetBeforeRentalLock(f func()) {
	s.beforeRentalLock = f
}

// PeakRentalHolds exposes peakRentalHolds (with except) to the tests.
func PeakRentalHolds(ctx context.Context, tx pgx.Tx, modelIDs []uuid.UUID, from time.Time, to *time.Time, now time.Time, except uuid.UUID) (map[uuid.UUID]int, error) {
	return peakRentalHolds(ctx, store.New(tx), modelIDs, from, to, now, except)
}
