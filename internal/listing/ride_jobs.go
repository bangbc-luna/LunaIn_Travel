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
// reopened. It also sweeps the trips a schedule no longer covers (inactive,
// or after valid_to) that a schedule edit kept because they had seats held
// and whose seats have since been released (SweepScheduleTrips). Each
// schedule runs in its own tx holding its row FOR UPDATE (so it serializes
// with a partner edit, which locks the listing then the schedule); a schedule
// that fails is logged and the others go on, and the failures are returned
// joined at the end so the job run shows as failed. Idempotent: trips are
// unique per (listing_id, departure_at).
func (s *Service) GenerateTrips(ctx context.Context) (int, error) {
	now := s.clock.Now()
	today := vnDate(now)
	horizon := today
	horizon.Time = today.Time.AddDate(0, 0, tripHorizonDays-1)
	ids, err := store.New(s.pool).ListSchedulesToGenerate(ctx, store.ListSchedulesToGenerateParams{Today: today, Horizon: horizon, Now: now})
	if err != nil {
		return 0, fmt.Errorf("list schedules to generate: %w", err)
	}
	total := 0
	var failed []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return total, errors.Join(append(failed, err)...)
		}
		n, err := s.generateOne(ctx, id, now)
		if err != nil {
			s.log.Error("listing: generate trips failed", "schedule_id", id, "err", err)
			failed = append(failed, fmt.Errorf("schedule %s: %w", id, err))
			continue
		}
		total += n
	}
	return total, errors.Join(failed...)
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
		if err := s.sweepSchedule(ctx, tx, sc.ID, now); err != nil {
			return err
		}
		n, err = s.generateForSchedule(ctx, tx, sc, now)
		return err
	})
	return n, err
}

// sweepSchedule handles the unbooked future trips that schedule id (locked by
// the caller) no longer covers. A trip that another active schedule of the
// listing covers is handed over to it, with its capacity and price (review
// I2: a partner replaced the schedule while the trip held seats). That
// schedule is locked FOR UPDATE first; the partner path never holds two
// schedules, and the overlap rule keeps a covering schedule from needing id
// back. The remaining trips are cancelled (cancel_source schedule).
func (s *Service) sweepSchedule(ctx context.Context, tx pgx.Tx, id uuid.UUID, now time.Time) error {
	q := store.New(tx)
	covers, err := q.ListCoveringSchedules(ctx, store.ListCoveringSchedulesParams{ScheduleID: id, Now: now})
	if err != nil {
		return fmt.Errorf("list covering schedules: %w", err)
	}
	for _, cover := range covers {
		if _, err := q.LockRideScheduleByID(ctx, cover); err != nil {
			return fmt.Errorf("lock covering schedule: %w", err)
		}
		moved, err := q.ReassignCoveredTrips(ctx, store.ReassignCoveredTripsParams{ScheduleID: id, CoverID: cover, Now: now})
		if err != nil {
			return fmt.Errorf("reassign covered trips: %w", err)
		}
		if moved > 0 {
			s.log.Info("listing: trips handed over to the covering schedule", "schedule_id", id, "cover_id", cover, "trips", moved)
		}
	}
	swept, err := q.SweepScheduleTrips(ctx, store.SweepScheduleTripsParams{ScheduleID: id, Reason: scheduleCancelMsg, Now: now})
	if err != nil {
		return fmt.Errorf("sweep schedule trips: %w", err)
	}
	if swept > 0 {
		s.log.Info("listing: cancelled trips outside their schedule", "schedule_id", id, "trips", swept)
	}
	return nil
}
