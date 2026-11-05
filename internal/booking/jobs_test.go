package booking_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

// serviceEnd is when the fixture booking's 08:00-17:00 slot ends.
var serviceEnd = time.Date(2026, 11, 7, 17, 0, 0, 0, clock.VN)

func (e *env) expireHolds(t *testing.T) int {
	t.Helper()
	n, err := e.Svc.ExpireHolds(context.Background())
	require.NoError(t, err)
	return n
}

func (e *env) markNoShows(t *testing.T) int {
	t.Helper()
	n, err := e.Svc.MarkNoShows(context.Background())
	require.NoError(t, err)
	return n
}

func (e *env) releaseDue(t *testing.T) int {
	t.Helper()
	n, err := e.Svc.ReleaseDue(context.Background())
	require.NoError(t, err)
	return n
}

func (e *env) fakeState(t *testing.T, oc int64) string {
	t.Helper()
	st, err := e.Fake.GetStatus(context.Background(), oc)
	require.NoError(t, err)
	return st.State
}

func (e *env) releaseDueAt(t *testing.T, id uuid.UUID) *time.Time {
	t.Helper()
	var due *time.Time
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT release_due_at FROM booking_settlements WHERE booking_id = $1`, id).Scan(&due))
	return due
}

func TestExpireHolds_AfterGraceOnly(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	hold := *v.HoldExpiresAt

	e.Clock.T = hold.Add(4 * time.Minute)
	require.Equal(t, 0, e.expireHolds(t))
	require.Equal(t, "pending_payment", e.bookingStatus(t, v.ID))
	require.Equal(t, "pending", e.paymentStatus(t, oc))
	require.Equal(t, "open", e.checkoutStatus(t, v.ID))

	e.Clock.T = hold.Add(6 * time.Minute)
	require.Equal(t, 1, e.expireHolds(t))
	require.Equal(t, "expired", e.bookingStatus(t, v.ID))
	require.Equal(t, "expired", e.paymentStatus(t, oc))
	require.Equal(t, "closed", e.checkoutStatus(t, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'pending_payment' AND to_status = 'expired'
		AND actor_kind = 'system' AND actor_user_id IS NULL`, v.ID))
	require.Equal(t, payment.StateCancelled, e.fakeState(t, oc), "the link is cancelled after commit")

	// A second run finds nothing.
	require.Equal(t, 0, e.expireHolds(t))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history WHERE to_status = 'expired'`))
	require.Empty(t, e.refunds(t))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))
}

func TestExpireHolds_SkipsAlreadyConfirmed(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, gross)

	e.Clock.T = v.HoldExpiresAt.Add(6 * time.Minute)
	require.Equal(t, 0, e.expireHolds(t))
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
	require.Equal(t, "paid", e.checkoutStatus(t, v.ID))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM booking_status_history WHERE to_status = 'expired'`))
	e.requireBalance(t, 342_000, 0)
}

// The real expiry job, then a late full transfer while still bookable: the
// booking is re-confirmed (Review Focus 2, as with the e.expire helper).
func TestExpireHolds_ThenLatePaymentReconfirms(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.Clock.T = v.HoldExpiresAt.Add(6 * time.Minute)
	require.Equal(t, 1, e.expireHolds(t))

	e.Clock.Advance(20 * time.Minute)
	e.pay(t, oc, gross)
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
	require.Equal(t, "paid", e.checkoutStatus(t, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'expired' AND to_status = 'confirmed'
		AND reason = 'thanh toán trễ, xác nhận lại'`, v.ID))
	require.Empty(t, e.refunds(t))
	e.requireBalance(t, 342_000, 0)
}

// A webhook racing the expiry job past release: both queue on the checkout
// lock (held by a blocker tx) in a forced order, then contend for it.
// Either way the booking ends confirmed exactly once (directly, or expired
// then re-confirmed), with one posting and no refund; nothing deadlocks.
func TestExpireHolds_RacesWebhook(t *testing.T) {
	for _, webhookFirst := range []bool{true, false, true, false} {
		e := newEnv(t)
		ctx := context.Background()
		v := e.create(t, "k")
		oc := e.orderCode(t, v.ID)
		body := e.Fake.MarkPaid(oc, gross)
		e.Clock.T = v.HoldExpiresAt.Add(6 * time.Minute)

		blocker, err := e.Pool.Begin(ctx)
		require.NoError(t, err)
		_, err = blocker.Exec(ctx, `SELECT 1 FROM checkouts c JOIN bookings b ON b.checkout_id = c.id
			WHERE b.id = $1 FOR UPDATE OF c`, v.ID)
		require.NoError(t, err)
		waiting := func(n int) {
			require.Eventually(t, func() bool {
				return e.count(t, `SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'`) == n
			}, 10*time.Second, 5*time.Millisecond)
		}

		var wg sync.WaitGroup
		var code int
		var expErr error
		hook := func() { code = e.webhook(t, body).Code }
		expire := func() { _, expErr = e.Svc.ExpireHolds(ctx) }
		first, second := expire, hook
		if webhookFirst {
			first, second = hook, expire
		}
		wg.Go(first)
		waiting(1)
		wg.Go(second)
		waiting(2)
		require.NoError(t, blocker.Rollback(ctx))
		wg.Wait()

		expired := e.count(t, `SELECT count(*) FROM booking_status_history WHERE to_status = 'expired'`)
		require.NoError(t, expErr)
		require.Equal(t, http.StatusOK, code)
		if webhookFirst {
			require.Equal(t, 0, expired, "confirmed first: the job skips it")
		} else {
			require.Equal(t, 1, expired, "expired first: the late payment re-confirms")
		}
		require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
		require.Equal(t, "paid", e.paymentStatus(t, oc))
		require.Equal(t, "paid", e.checkoutStatus(t, v.ID))
		require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history WHERE to_status = 'confirmed'`))
		require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_settlements`))
		require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE result IS NOT NULL`))
		require.Empty(t, e.refunds(t))
		e.requireBalance(t, 342_000, 0)
	}
}

func TestNoShow_ThenReleaseAfterDelay(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	e.pay(t, e.orderCode(t, v.ID), gross)

	// Not before the service ended.
	e.Clock.T = serviceEnd
	require.Equal(t, 0, e.markNoShows(t))
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))

	noShowAt := serviceEnd.Add(time.Minute)
	e.Clock.T = noShowAt
	require.Equal(t, 1, e.markNoShows(t))
	require.Equal(t, "no_show", e.bookingStatus(t, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM bookings WHERE id = $1 AND no_show_at = $2`, v.ID, noShowAt))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'confirmed' AND to_status = 'no_show' AND actor_kind = 'system'`, v.ID))
	due := e.releaseDueAt(t, v.ID)
	require.NotNil(t, due)
	require.True(t, due.Equal(noShowAt.Add(24*time.Hour)), "release_delay_days = 1: %v", due)
	require.Equal(t, 0, e.markNoShows(t))

	// Release only once due.
	e.Clock.T = noShowAt.Add(23 * time.Hour)
	require.Equal(t, 0, e.releaseDue(t))
	e.requireBalance(t, 342_000, 0)
	e.Clock.T = noShowAt.Add(24 * time.Hour)
	require.Equal(t, 1, e.releaseDue(t))
	e.requireBalance(t, 0, 342_000)
	require.Equal(t, 0, e.releaseDue(t))
}

func TestRelease_MovesToAvailable(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	e.pay(t, e.orderCode(t, v.ID), gross)
	e.Clock.T = time.Date(2026, 11, 7, 8, 30, 0, 0, clock.VN)
	_, err := e.Svc.CheckIn(context.Background(), e.P.ID, e.P.OwnerUserID, e.qrToken(t, v.ID))
	require.NoError(t, err)

	e.Clock.T = e.Clock.T.Add(24 * time.Hour)
	require.Equal(t, 1, e.releaseDue(t))
	e.requireBalance(t, 0, 342_000)
	require.NoError(t, e.Ledger.Verify(context.Background(), e.P.ID))
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM ledger_entries WHERE booking_id = $1 AND kind = 'release'`, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_settlements
		WHERE booking_id = $1 AND released_at IS NOT NULL AND net_pending = 0`, v.ID))
	require.Equal(t, "completed", e.bookingStatus(t, v.ID))
}

// The release is re-checked under the booking lock: a settlement that is not
// due at now (e.g. moved between the unlocked list and the lock) is skipped.
func TestReleaseIfDue_SkipsNotDue(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	e.pay(t, e.orderCode(t, v.ID), gross)
	noShowAt := serviceEnd.Add(time.Minute)
	e.Clock.T = noShowAt
	require.Equal(t, 1, e.markNoShows(t))

	release := func() bool {
		var released bool
		require.NoError(t, db.InTx(context.Background(), e.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), `SELECT 1 FROM bookings WHERE id = $1 FOR UPDATE`, v.ID); err != nil {
				return err
			}
			var err error
			released, err = e.Ledger.ReleaseIfDue(context.Background(), tx, v.ID, e.Clock.Now())
			return err
		}))
		return released
	}
	require.False(t, release(), "due in 24h")
	e.requireBalance(t, 342_000, 0)
	e.Clock.T = noShowAt.Add(24 * time.Hour)
	require.True(t, release())
	e.requireBalance(t, 0, 342_000)
	require.False(t, release(), "already released")
	e.requireBalance(t, 0, 342_000)
}

// Hold expiry never overwrites a checkout that is not open (e.g. paid).
func TestExpireHolds_ClosesOnlyOpenCheckout(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	e.exec(t, `UPDATE checkouts SET status = 'paid' WHERE id = (SELECT checkout_id FROM bookings WHERE id = $1)`, v.ID)
	e.Clock.T = v.HoldExpiresAt.Add(6 * time.Minute)
	require.Equal(t, 1, e.expireHolds(t))
	require.Equal(t, "expired", e.bookingStatus(t, v.ID))
	require.Equal(t, "paid", e.checkoutStatus(t, v.ID))
}

// Workers are thin wrappers over the processing functions.
var (
	_ = booking.ExpireHoldsWorker{}
	_ = booking.NoShowWorker{}
	_ = booking.ReleaseWorker{}
)
