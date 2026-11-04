package mailer

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// SendEmailArgs is the River job payload for sending one email.
type SendEmailArgs struct {
	Message
}

func (SendEmailArgs) Kind() string { return "send_email" }

func (SendEmailArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 8}
}

// SendEmailWorker delivers queued emails through Mailer.
type SendEmailWorker struct {
	river.WorkerDefaults[SendEmailArgs]
	Mailer Mailer
}

func (w *SendEmailWorker) Work(ctx context.Context, job *river.Job[SendEmailArgs]) error {
	return w.Mailer.Send(ctx, job.Args.Message)
}

// EnqueueTx enqueues an email inside the caller's business transaction, so it
// is sent only if that transaction commits. All business emails go through here.
func EnqueueTx(ctx context.Context, tx pgx.Tx, client *river.Client[pgx.Tx], m Message) error {
	_, err := client.InsertTx(ctx, tx, SendEmailArgs{Message: m}, nil)
	return err
}
