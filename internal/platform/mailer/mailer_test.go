package mailer_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
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

func TestEnqueueTx_MaxAttempts(t *testing.T) {
	pool := testutil.Postgres(t)
	ctx := context.Background()
	workers := river.NewWorkers()
	river.AddWorker(workers, &mailer.SendEmailWorker{Mailer: &mailer.Recorder{}})
	client, err := jobs.NewClient(pool, workers, slog.Default())
	require.NoError(t, err)

	require.NoError(t, db.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := mailer.EnqueueTx(ctx, tx, client, mailer.Message{To: "a@b.c", Subject: "otp", Text: "t", MaxAttempts: 3}); err != nil {
			return err
		}
		return mailer.EnqueueTx(ctx, tx, client, mailer.Message{To: "a@b.c", Subject: "default", Text: "t"})
	}))

	var otp, def int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT max_attempts FROM river_job WHERE kind = 'send_email' AND args->>'subject' = 'otp'`).Scan(&otp))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT max_attempts FROM river_job WHERE kind = 'send_email' AND args->>'subject' = 'default'`).Scan(&def))
	require.Equal(t, 3, otp)
	require.Equal(t, mailer.DefaultMaxAttempts, def)
}

func TestSMTPMailer_StalledServerIsBoundedByContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(10 * time.Second) // never writes the greeting
	}()

	addr := ln.Addr().(*net.TCPAddr)
	m := mailer.SMTPMailer{Host: "127.0.0.1", Port: addr.Port, From: "LunaIn <no-reply@lunain.vn>"}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = m.Send(ctx, mailer.Message{To: "an@gmail.com", Subject: "s", Text: "t"})
	require.Error(t, err)
	require.Less(t, time.Since(start), 2*time.Second)
	var ne net.Error
	require.True(t, errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()), "got %v", err)
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
