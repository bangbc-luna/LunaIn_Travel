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

// DefaultMaxAttempts is the delivery attempts of a Message with MaxAttempts 0.
const DefaultMaxAttempts = 8

func (SendEmailArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: DefaultMaxAttempts}
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
// m.MaxAttempts (default DefaultMaxAttempts) bounds delivery retries.
func EnqueueTx(ctx context.Context, tx pgx.Tx, client *river.Client[pgx.Tx], m Message) error {
	n := m.MaxAttempts
	if n <= 0 {
		n = DefaultMaxAttempts
	}
	_, err := client.InsertTx(ctx, tx, SendEmailArgs{Message: m}, &river.InsertOpts{MaxAttempts: n})
	return err
}
