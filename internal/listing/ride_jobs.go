package listing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

// GenerateTripsArgs is the hourly trip generation job.
type GenerateTripsArgs struct{}

func (GenerateTripsArgs) Kind() string { return "listing_generate_trips" }

// GenerateTripsWorker runs Service.GenerateTrips. Service is set before the
// job client starts.
type GenerateTripsWorker struct {
	river.WorkerDefaults[GenerateTripsArgs]
	Service *Service
}

func (w *GenerateTripsWorker) Work(ctx context.Context, _ *river.Job[GenerateTripsArgs]) error {
	_, err := w.Service.GenerateTrips(ctx)
	return err
}

// PeriodicJobs: trip generation every hour and once on start. A run is never
// retried (MaxAttempts 1): the next hour does the same work.
func PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(time.Hour),
			func() (river.JobArgs, *river.InsertOpts) {
				return GenerateTripsArgs{}, &river.InsertOpts{MaxAttempts: 1}
			},
			&river.PeriodicJobOpts{RunOnStart: true}),
	}
}

// GenerateTrips extends every active schedule's trips to the 60-day horizon
// (see generateForSchedule) and returns how many trips it inserted or
// reopened. Each schedule runs in its own tx holding its row FOR UPDATE (so
// it serializes with a partner edit, which locks the listing then the
// schedule); a schedule that fails is logged and the others go on.
// Idempotent: trips are unique per (listing_id, departure_at).
func (s *Service) GenerateTrips(ctx context.Context) (int, error) {
	now := s.clock.Now()
	today := vnDate(now)
	horizon := today
	horizon.Time = today.Time.AddDate(0, 0, tripHorizonDays-1)
	ids, err := store.New(s.pool).ListSchedulesToGenerate(ctx, store.ListSchedulesToGenerateParams{Today: today, Horizon: horizon})
	if err != nil {
		return 0, fmt.Errorf("list schedules to generate: %w", err)
	}
	total := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := s.generateOne(ctx, id, now)
		if err != nil {
			s.log.Error("listing: generate trips failed", "schedule_id", id, "err", err)
			continue
		}
		total += n
	}
	return total, nil
}

func (s *Service) generateOne(ctx context.Context, id uuid.UUID, now time.Time) (int, error) {
	n := 0
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		sc, err := store.New(tx).LockRideScheduleByID(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock ride schedule: %w", err)
		}
		n, err = s.generateForSchedule(ctx, tx, sc, now)
		return err
	})
	return n, err
}
