package payout

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

func countWhere(t *testing.T, e *env, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func TestRequest_HoldsAndSnapshots(t *testing.T) {
	e := newEnv(t)
	p := e.fundedPartner(t, "alpha")

	v, err := e.svc.Request(context.Background(), p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	require.Equal(t, StatusRequested, v.Status)
	require.Equal(t, p.ID, v.PartnerID)
	require.Equal(t, int64(300_000), v.Amount)
	require.True(t, v.RequestedAt.Equal(testNow))
	require.Equal(t, BankSnapshot{
		BankBIN: p.bank.BankBIN, BankName: p.bank.BankName,
		AccountNumber: p.bank.AccountNumber, AccountHolder: p.bank.AccountHolder,
	}, v.Bank)
	e.requireAvailable(t, p, 600_000)

	var holdSeq int64
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT seq FROM ledger_entries WHERE payout_id = $1 AND kind = 'payout_hold'`, v.ID).Scan(&holdSeq))
	require.Equal(t, int64(1), v.StatementSeqFrom)
	require.Equal(t, holdSeq, v.StatementSeqTo)
	require.Equal(t, int64(0), v.OpeningAvailable)
	require.Equal(t, int64(600_000), v.ClosingAvailable)
	require.Len(t, v.Statement, 2)
	require.Equal(t, "release", v.Statement[0].Kind)
	require.Equal(t, "payout_hold", v.Statement[1].Kind)
	require.Equal(t, int64(-300_000), v.Statement[1].Amount)
	requireBalanced(t, v)

	// The stored row matches what Request returned.
	got, err := e.svc.GetMine(context.Background(), p.ID, v.ID)
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
	require.Equal(t, v.Payout.Bank, got.Bank)
	require.Equal(t, v.StatementSeqFrom, got.StatementSeqFrom)
	require.Equal(t, v.StatementSeqTo, got.StatementSeqTo)
	require.Equal(t, v.OpeningAvailable, got.OpeningAvailable)
	require.Equal(t, v.ClosingAvailable, got.ClosingAvailable)
	require.Equal(t, v.Statement, got.Statement)

	var requestedBy, bankAccountID uuid.UUID
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT requested_by, bank_account_id FROM payouts WHERE id = $1`, v.ID).Scan(&requestedBy, &bankAccountID))
	require.Equal(t, p.Owner, requestedBy)
	require.Equal(t, p.bank.ID, bankAccountID)
}

func TestRequest_SecondStatementStartsAfterPrevious(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v1, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	e.reject(t, v1)
	e.fund(t, p, 100_000)
	e.requireAvailable(t, p, 1_000_000)

	v2, err := e.svc.Request(ctx, p.ID, p.Owner, 200_000)
	require.NoError(t, err)
	require.Equal(t, v1.StatementSeqTo+1, v2.StatementSeqFrom)
	require.Equal(t, v1.ClosingAvailable, v2.OpeningAvailable)
	require.Equal(t, int64(800_000), v2.ClosingAvailable)
	requireBalanced(t, v2)

	kinds := map[string]int64{}
	for _, l := range v2.Statement {
		kinds[l.Kind] += l.Amount
	}
	require.Equal(t, map[string]int64{"payout_reversal": 300_000, "release": 100_000, "payout_hold": -200_000}, kinds)
	require.Len(t, v2.Statement, 3)
	e.requireAvailable(t, p, 800_000)

	// The first statement is unchanged by what came after it.
	g1, err := e.svc.GetMine(ctx, p.ID, v1.ID)
	require.NoError(t, err)
	require.Equal(t, StatusRejected, g1.Status)
	require.Len(t, g1.Statement, 2)
	requireBalanced(t, g1)
}

func TestRequest_InvalidAmount422(t *testing.T) {
	e := newEnv(t)
	p := e.fundedPartner(t, "alpha")
	for _, amount := range []int64{0, -1} {
		_, err := e.svc.Request(context.Background(), p.ID, p.Owner, amount)
		requireCode(t, err, http.StatusUnprocessableEntity, "invalid_amount")
	}
	require.Equal(t, 0, countWhere(t, e, `SELECT count(*) FROM payouts`))
}

func TestRequest_BelowMinimum422(t *testing.T) {
	e := newEnv(t)
	p := e.fundedPartner(t, "alpha")
	_, err := e.svc.Request(context.Background(), p.ID, p.Owner, 99_999)
	requireCode(t, err, http.StatusUnprocessableEntity, "payout_below_minimum")
	require.Contains(t, err.Error(), "100.000")
	require.Equal(t, 0, countWhere(t, e, `SELECT count(*) FROM payouts`))
	e.requireAvailable(t, p, 900_000)

	// Exactly the minimum is fine.
	_, err = e.svc.Request(context.Background(), p.ID, p.Owner, 100_000)
	require.NoError(t, err)
}

func TestRequest_ExceedsAvailable422(t *testing.T) {
	e := newEnv(t)
	p := e.fundedPartner(t, "alpha")
	_, err := e.svc.Request(context.Background(), p.ID, p.Owner, 900_001)
	requireCode(t, err, http.StatusUnprocessableEntity, "insufficient_available")
	require.Equal(t, 0, countWhere(t, e, `SELECT count(*) FROM payouts`))
	e.requireAvailable(t, p, 900_000)

	// All of it is fine.
	v, err := e.svc.Request(context.Background(), p.ID, p.Owner, 900_000)
	require.NoError(t, err)
	require.Equal(t, int64(0), v.ClosingAvailable)
	e.requireAvailable(t, p, 0)
}

func TestRequest_NegativeAvailable422(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	e.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.ledger.PostAdjustment(ctx, tx, ledger.Adjustment{
			PartnerID: p.ID, Amount: -1_000_000, Reason: "thu hồi", CreatedBy: e.admin,
		})
		return err
	})
	e.requireAvailable(t, p, -100_000)
	_, err := e.svc.Request(ctx, p.ID, p.Owner, 100_000)
	requireCode(t, err, http.StatusUnprocessableEntity, "insufficient_available")
	require.Equal(t, 0, countWhere(t, e, `SELECT count(*) FROM payouts`))

	// Zero available is refused too.
	q := e.newPartner(t, "beta", true)
	_, err = e.svc.Request(ctx, q.ID, q.Owner, 100_000)
	requireCode(t, err, http.StatusUnprocessableEntity, "insufficient_available")
	require.Equal(t, 0, countWhere(t, e, `SELECT count(*) FROM payouts`))
}

func TestRequest_NoBankAccount422(t *testing.T) {
	e := newEnv(t)
	p := e.newPartner(t, "alpha", false)
	e.fund(t, p, 900_000)
	_, err := e.svc.Request(context.Background(), p.ID, p.Owner, 300_000)
	requireCode(t, err, http.StatusUnprocessableEntity, "bank_account_required")
	require.Equal(t, 0, countWhere(t, e, `SELECT count(*) FROM payouts`))
	e.requireAvailable(t, p, 900_000)
}

func TestRequest_OneOpen409(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	_, err = e.svc.Request(ctx, p.ID, p.Owner, 100_000)
	requireCode(t, err, http.StatusConflict, "payout_already_open")

	// An approved payout is still open.
	e.mustExec(t, `UPDATE payouts SET status = 'approved', approved_at = now(), approved_by = $2 WHERE id = $1`,
		v.ID, e.admin)
	_, err = e.svc.Request(ctx, p.ID, p.Owner, 100_000)
	requireCode(t, err, http.StatusConflict, "payout_already_open")
	require.Equal(t, 1, countWhere(t, e, `SELECT count(*) FROM payouts`))
	e.requireAvailable(t, p, 600_000)

	// Another partner is not blocked.
	q := e.fundedPartner(t, "beta")
	_, err = e.svc.Request(ctx, q.ID, q.Owner, 100_000)
	require.NoError(t, err)
}

func TestRequest_Suspended409(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	require.NoError(t, e.partners.Suspend(ctx, e.admin, p.ID, "vi phạm"))
	_, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	requireCode(t, err, http.StatusConflict, "partner_suspended")
	require.Equal(t, 0, countWhere(t, e, `SELECT count(*) FROM payouts`))
	e.requireAvailable(t, p, 900_000)
}

// Review Focus 1: two concurrent requests of one partner make exactly one
// payout and one hold.
func TestRequest_Concurrent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")

	const n = 2
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = e.svc.Request(ctx, p.ID, p.Owner, 500_000)
		}()
	}
	close(start)
	wg.Wait()

	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		status, code := appCode(t, err)
		require.Contains(t, []string{"payout_already_open", "insufficient_available"}, code, "%v", err)
		require.Contains(t, []int{http.StatusConflict, http.StatusUnprocessableEntity}, status)
	}
	require.Equal(t, 1, ok, "errors: %v", errs)
	require.Equal(t, 1, countWhere(t, e, `SELECT count(*) FROM payouts WHERE partner_id = $1`, p.ID))
	require.Equal(t, 1, countWhere(t, e,
		`SELECT count(*) FROM ledger_entries WHERE partner_id = $1 AND kind = 'payout_hold'`, p.ID))
	e.requireAvailable(t, p, 400_000)
}

// waitForLockWaiter waits until some backend is blocked on a lock.
func waitForLockWaiter(t *testing.T, e *env) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if countWhere(t, e, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no backend is waiting on a lock")
}

// Review Focus 2: a release that commits right after a payout_hold (higher
// seq) belongs to the next statement, never to two and never to none.
func TestRequest_ReleaseRacesStatementBoundary(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	b2 := e.confirmBooking(t, p, 200_000)

	reached, proceed := make(chan struct{}), make(chan struct{})
	e.svc.beforeCommit = func() {
		close(reached)
		<-proceed
	}
	type result struct {
		v   PayoutView
		err error
	}
	resA := make(chan result, 1)
	go func() {
		v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
		resA <- result{v, err}
	}()
	select {
	case <-reached:
	case r := <-resA:
		t.Fatalf("Request returned before the hook: %v", r.err)
	}

	errB := make(chan error, 1)
	go func() {
		errB <- db.InTx(ctx, e.pool, func(tx pgx.Tx) error { return e.releaseTx(ctx, tx, b2) })
	}()
	// B blocks on partner_balances, held by A.
	waitForLockWaiter(t, e)
	select {
	case err := <-errB:
		t.Fatalf("release finished while the payout tx held partner_balances: %v", err)
	default:
	}
	close(proceed)
	a := <-resA
	require.NoError(t, a.err)
	require.NoError(t, <-errB)
	e.svc.beforeCommit = nil
	v1 := a.v
	e.requireAvailable(t, p, 800_000)

	var releaseSeq int64
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT seq FROM ledger_entries
		WHERE booking_id = $1 AND kind = 'release' AND bucket = 'available'`, b2.BookingID).Scan(&releaseSeq))
	require.Greater(t, releaseSeq, v1.StatementSeqTo)

	g1, err := e.svc.GetMine(ctx, p.ID, v1.ID)
	require.NoError(t, err)
	requireBalanced(t, g1)
	for _, l := range g1.Statement {
		require.NotEqual(t, releaseSeq, l.Seq)
	}

	pv, err := e.svc.Preview(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, v1.StatementSeqTo+1, pv.StatementFromSeq)
	require.Equal(t, v1.ClosingAvailable, pv.OpeningAvailable)
	require.Len(t, pv.Statement, 1)
	require.Equal(t, releaseSeq, pv.Statement[0].Seq)
	require.Equal(t, int64(200_000), pv.Statement[0].Amount)
	require.Equal(t, int64(800_000), pv.Available)

	e.reject(t, v1)
	v2, err := e.svc.Request(ctx, p.ID, p.Owner, 100_000)
	require.NoError(t, err)
	requireBalanced(t, v2)
	require.Equal(t, v1.StatementSeqTo+1, v2.StatementSeqFrom)
	require.Equal(t, v1.ClosingAvailable, v2.OpeningAvailable)
	seen := 0
	for _, l := range v2.Statement {
		if l.Seq == releaseSeq {
			seen++
		}
	}
	require.Equal(t, 1, seen)
	e.requireAvailable(t, p, 1_000_000)
}

func TestPreview_CanRequestRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	p := e.fundedPartner(t, "alpha")
	pv, err := e.svc.Preview(ctx, p.ID)
	require.NoError(t, err)
	require.True(t, pv.CanRequest)
	require.Equal(t, int64(900_000), pv.Available)
	require.Equal(t, int64(100_000), pv.MinPayoutAmount)
	require.Nil(t, pv.OpenPayoutID)
	require.Equal(t, int64(1), pv.StatementFromSeq)
	require.Equal(t, int64(0), pv.OpeningAvailable)
	require.Len(t, pv.Statement, 1)
	require.Equal(t, int64(900_000), pv.Statement[0].Amount)

	// An open payout blocks a new one; the next statement starts after it.
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	pv, err = e.svc.Preview(ctx, p.ID)
	require.NoError(t, err)
	require.False(t, pv.CanRequest)
	require.NotNil(t, pv.OpenPayoutID)
	require.Equal(t, v.ID, *pv.OpenPayoutID)
	require.Equal(t, v.StatementSeqTo+1, pv.StatementFromSeq)
	require.Equal(t, int64(600_000), pv.OpeningAvailable)
	require.Equal(t, int64(600_000), pv.Available)
	require.Empty(t, pv.Statement)

	// No bank account.
	nb := e.newPartner(t, "nobank", false)
	e.fund(t, nb, 900_000)
	pv, err = e.svc.Preview(ctx, nb.ID)
	require.NoError(t, err)
	require.False(t, pv.CanRequest)
	require.Equal(t, int64(900_000), pv.Available)

	// Suspended.
	s := e.fundedPartner(t, "suspended")
	require.NoError(t, e.partners.Suspend(ctx, e.admin, s.ID, "vi phạm"))
	pv, err = e.svc.Preview(ctx, s.ID)
	require.NoError(t, err)
	require.False(t, pv.CanRequest)

	// Below the minimum.
	low := e.newPartner(t, "low", true)
	e.fund(t, low, 99_999)
	pv, err = e.svc.Preview(ctx, low.ID)
	require.NoError(t, err)
	require.False(t, pv.CanRequest)
	require.Equal(t, int64(99_999), pv.Available)

	// With a zero minimum, nothing available still cannot be requested.
	zero := int64(0)
	_, err = e.settings.Update(ctx, e.admin, settings.Patch{MinPayoutAmount: &zero})
	require.NoError(t, err)
	empty := e.newPartner(t, "empty", true)
	pv, err = e.svc.Preview(ctx, empty.ID)
	require.NoError(t, err)
	require.False(t, pv.CanRequest)
	require.Equal(t, int64(0), pv.Available)
	require.Equal(t, int64(0), pv.StatementFromSeq-1)
	pv, err = e.svc.Preview(ctx, low.ID)
	require.NoError(t, err)
	require.True(t, pv.CanRequest)
}
