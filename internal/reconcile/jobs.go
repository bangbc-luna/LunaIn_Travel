package reconcile

import (
	"context"
	"errors"
	"time"

	"github.com/riverqueue/river"

	"github.com/bangbc-luna/lunain-travel/internal/payment"
)

// DailyArgs is the daily reconciliation job.
type DailyArgs struct{}

func (DailyArgs) Kind() string { return "reconcile_daily" }

// RemindRefundsArgs is the daily refund bank info reminder job.
type RemindRefundsArgs struct{}

func (RemindRefundsArgs) Kind() string { return "payment_remind_refunds" }

// DailyWorker runs Service.Run as the scheduled job. Service is set before
// the job client starts.
type DailyWorker struct {
	river.WorkerDefaults[DailyArgs]
	Service *Service
}

// DailyTimeout replaces River's 1 minute default: a run makes up to 500
// provider calls. When it expires, the run still stores what it found.
const DailyTimeout = 15 * time.Minute

// Timeout is the job's time budget (DailyTimeout).
func (w *DailyWorker) Timeout(*river.Job[DailyArgs]) time.Duration { return DailyTimeout }

// Work runs the reconciliation; while another run holds the lock it skips
// (logged): that run does the same work.
func (w *DailyWorker) Work(ctx context.Context, _ *river.Job[DailyArgs]) error {
	_, err := w.Service.Run(ctx, nil)
	if errors.Is(err, ErrRunning) {
		w.Service.d.Log.Info("reconcile: daily run skipped, another run is in progress")
		return nil
	}
	return err
}

// RemindRefundsWorker runs payment.Service.RemindAwaitingBankInfo.
type RemindRefundsWorker struct {
	river.WorkerDefaults[RemindRefundsArgs]
	Payments *payment.Service
}

func (w *RemindRefundsWorker) Work(ctx context.Context, _ *river.Job[RemindRefundsArgs]) error {
	_, err := w.Payments.RemindAwaitingBankInfo(ctx)
	return err
}

// PeriodicJobs: the reconciliation at 02:30 and the refund reminders at
// 09:00, Vietnam time, not run on start. A run is never retried (MaxAttempts
// 1): a manual run (POST /v1/admin/reconciliations/run) or the next day
// redoes the work.
func PeriodicJobs() []*river.PeriodicJob {
	opts := &river.InsertOpts{MaxAttempts: 1}
	job := func(s river.PeriodicSchedule, args river.JobArgs) *river.PeriodicJob {
		return river.NewPeriodicJob(s, func() (river.JobArgs, *river.InsertOpts) { return args, opts },
			&river.PeriodicJobOpts{RunOnStart: false})
	}
	return []*river.PeriodicJob{
		job(DailyAt(2, 30, vnZone), DailyArgs{}),
		job(DailyAt(9, 0, vnZone), RemindRefundsArgs{}),
	}
}
