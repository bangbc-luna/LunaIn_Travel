package identity

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/jobs"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
)

// testEnv wires a Service against a real Postgres and a running River client
// whose emails land in Mail. Shared by all identity tests.
type testEnv struct {
	Svc    *Service
	Pool   *pgxpool.Pool
	Clock  *clock.Fixed
	Mail   *mailer.Recorder
	Google *fakeGoogle

	// seenMail is how many emails lastCode has already consumed.
	seenMail int
}

var testStart = time.Date(2026, 11, 4, 9, 0, 0, 0, time.FixedZone("ICT", 7*3600))

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	pool := testutil.Postgres(t)
	rec := &mailer.Recorder{}
	workers := river.NewWorkers()
	river.AddWorker(workers, &mailer.SendEmailWorker{Mailer: rec})
	client, err := jobs.NewClient(pool, workers, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NoError(t, client.Start(context.Background()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(ctx)
	})

	clk := &clock.Fixed{T: testStart}
	google := &fakeGoogle{Tokens: map[string]GoogleIdentity{}}
	svc := NewService(Deps{
		Pool:       pool,
		Jobs:       client,
		Issuer:     auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk),
		Clock:      clk,
		CodePepper: []byte("test-pepper"),
		Google:     google,
		RefreshTTL: 30 * 24 * time.Hour,
	})
	return &testEnv{Svc: svc, Pool: pool, Clock: clk, Mail: rec, Google: google}
}

var codeRe = regexp.MustCompile(`\b\d{6}\b`)

// lastCode waits up to 5s for an email newer than the ones already seen by
// lastCode and returns the 6-digit code from the most recent email.
func lastCode(t *testing.T, env *testEnv) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs := env.Mail.Messages()
		if len(msgs) > env.seenMail {
			env.seenMail = len(msgs)
			code := codeRe.FindString(msgs[len(msgs)-1].Text)
			require.NotEmpty(t, code, "no code in email: %q", msgs[len(msgs)-1].Text)
			return code
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for email")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fakeGoogle verifies ID tokens from a fixed map.
type fakeGoogle struct {
	Tokens map[string]GoogleIdentity
}

var errFakeGoogle = errors.New("fake google: unknown token")

func (f *fakeGoogle) Verify(_ context.Context, idToken string) (GoogleIdentity, error) {
	id, ok := f.Tokens[idToken]
	if !ok {
		return GoogleIdentity{}, errFakeGoogle
	}
	return id, nil
}
