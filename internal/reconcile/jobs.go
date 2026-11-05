package reconcile

import (
	"context"

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

func (w *DailyWorker) Work(ctx context.Context, _ *river.Job[DailyArgs]) error {
	_, err := w.Service.Run(ctx, nil)
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
