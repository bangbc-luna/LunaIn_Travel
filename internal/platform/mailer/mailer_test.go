package mailer_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/jobs"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
)

func TestEnqueueTx_RolledBackTxSendsNothing(t *testing.T) {
	pool := testutil.Postgres(t)
	ctx := context.Background()
	workers := river.NewWorkers()
	river.AddWorker(workers, &mailer.SendEmailWorker{Mailer: &mailer.Recorder{}})
	client, err := jobs.NewClient(pool, workers, slog.Default())
	require.NoError(t, err)

	sentinel := errors.New("rollback")
	err = db.InTx(ctx, pool, func(tx pgx.Tx) error {
		require.NoError(t, mailer.EnqueueTx(ctx, tx, client, mailer.Message{To: "a@b.c", Subject: "s", Text: "t"}))
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'send_email'`).Scan(&n))
	require.Equal(t, 0, n)
}

func TestEnqueueTx_CommittedJobIsSent(t *testing.T) {
	pool := testutil.Postgres(t)
	ctx := context.Background()
	rec := &mailer.Recorder{}
	workers := river.NewWorkers()
	river.AddWorker(workers, &mailer.SendEmailWorker{Mailer: rec})
	client, err := jobs.NewClient(pool, workers, slog.Default())
	require.NoError(t, err)

	events, cancel := client.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = client.Stop(stopCtx)
	})

	msg := mailer.Message{To: "an@gmail.com", Subject: "Xin chào", Text: "Mã: 123456"}
	require.NoError(t, db.InTx(ctx, pool, func(tx pgx.Tx) error {
		return mailer.EnqueueTx(ctx, tx, client, msg)
	}))

	select {
	case <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for job completion")
	}
	require.Equal(t, []mailer.Message{msg}, rec.Messages())
}

func TestBuildMessage(t *testing.T) {
	raw := string(mailer.BuildMessage("LunaIn <no-reply@lunain.vn>", mailer.Message{
		To: "an@gmail.com", Subject: "Xác minh email", Text: "Mã: 123456",
	}))
	require.Contains(t, raw, "From: LunaIn <no-reply@lunain.vn>\r\n")
	require.Contains(t, raw, "To: an@gmail.com\r\n")
	require.Contains(t, raw, "Subject: =?UTF-8?q?")
	require.Contains(t, raw, "Content-Type: text/plain; charset=UTF-8\r\n")
	require.True(t, strings.HasSuffix(raw, "\r\n\r\nMã: 123456"))
}
