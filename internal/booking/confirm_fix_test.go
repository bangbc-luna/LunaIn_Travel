package booking_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/payment"
)

// replacedLink creates a booking whose first payment link lost its response
// and was replaced inline: it returns the booking, the cancelled order code
// and the pending replacement.
func (e *env) replacedLink(t *testing.T) (id string, oldOC, newOC int64) {
	t.Helper()
	e.Fake.LoseNextCreateResponse()
	v := e.create(t, "k")
	require.NotNil(t, v.Payment.CheckoutURL)
	newOC = e.orderCode(t, v.ID)
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT provider_order_code FROM payments WHERE status = 'cancelled'`).Scan(&oldOC))
	return v.ID.String(), oldOC, newOC
}

// Ruling 10: the customer pays the replaced QR while the checkout is still
// payable: accepted, the replacement is cancelled (also at the provider).
func TestPaid_OnReplacedLinkAccepted(t *testing.T) {
	e := newEnv(t)
	id, oldOC, newOC := e.replacedLink(t)
	e.pay(t, oldOC, gross)

	require.Equal(t, "confirmed", e.str(t, `SELECT status FROM bookings WHERE id = $1`, id))
	require.Equal(t, "paid", e.paymentStatus(t, oldOC))
	require.Equal(t, "cancelled", e.paymentStatus(t, newOC))
	st, err := e.Fake.GetStatus(context.Background(), newOC)
	require.NoError(t, err)
	require.Equal(t, payment.StateCancelled, st.State, "the replacement link was cancelled after commit")
	require.Empty(t, e.refunds(t))
	e.requireBalance(t, 342_000, 0)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE result = 'accepted_replaced_link+confirmed'`))

	// Money on the replacement afterwards is a duplicate.
	e.pay(t, newOC, gross)
	r := ref(newOC)
	require.Equal(t, []refundRow{{Amount: gross, Reason: "duplicate_payment", Status: "awaiting_bank_info", Reference: &r}},
		e.refunds(t))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM river_job WHERE args::text LIKE '%đã được thanh toán trước đó%'`))
}

// Ruling 10: the replaced QR is paid but the booking can no longer be
// served: refunded as a duplicate, nothing confirmed.
func TestPaid_OnReplacedLinkNotBookableRefunded(t *testing.T) {
	e := newEnv(t)
	id, oldOC, newOC := e.replacedLink(t)
	e.Clock.Advance(30 * time.Minute) // hold past release, expiry job not run
	_, err := e.Listing.Hide(context.Background(), e.Admin, e.L.ID, "Vi phạm nội dung")
	require.NoError(t, err)
	e.pay(t, oldOC, gross)

	require.Equal(t, "pending_payment", e.str(t, `SELECT status FROM bookings WHERE id = $1`, id))
	require.Equal(t, "needs_review", e.paymentStatus(t, oldOC))
	require.Equal(t, "pending", e.paymentStatus(t, newOC))
	r := ref(oldOC)
	require.Equal(t, []refundRow{{Amount: gross, Reason: "duplicate_payment", Status: "awaiting_bank_info", Reference: &r}},
		e.refunds(t))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))
	// Ruling 12: the order was never paid: neutral email.
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM river_job WHERE args::text LIKE '%liên hệ hỗ trợ để hoàn tất đơn%'`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM river_job WHERE args::text LIKE '%đã được thanh toán trước đó%'`))
}

// Ruling 11: pending_payment whose hold passed release (expiry job lagging)
// is checked like an expired booking.
func TestLatePending_NotBookableExpiresAndRefunds(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.Clock.Advance(30 * time.Minute)
	_, err := e.Listing.Hide(context.Background(), e.Admin, e.L.ID, "Vi phạm nội dung")
	require.NoError(t, err)
	e.pay(t, oc, gross)

	require.Equal(t, "expired", e.bookingStatus(t, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'pending_payment' AND to_status = 'expired' AND actor_kind = 'system'`, v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
	require.Equal(t, "closed", e.checkoutStatus(t, v.ID))
	id := v.ID
	require.Equal(t, []refundRow{{Amount: gross, Reason: "late_payment", Status: "awaiting_bank_info", BookingID: &id}},
		e.refunds(t))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM booking_settlements`))
}

// Ruling 11: the same lag but still bookable: confirmed normally.
func TestLatePending_StillBookableConfirms(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.Clock.Advance(30 * time.Minute)
	e.pay(t, oc, gross)
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	e.requireBalance(t, 342_000, 0)
}

// Rulings 12/14: a second, still too small transfer after an underpayment
// is another mismatch (the needs_review payment does not block it); its
// email never claims the order was paid before.
func TestUnderpaidThenCorrectiveTransfer(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, 300_000)
	rec := e.webhook(t, e.Fake.MarkPaidRef(oc, 80_000, "REF-2"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, "pending_payment", e.bookingStatus(t, v.ID))
	rows := e.refunds(t)
	require.Len(t, rows, 2)
	require.Equal(t, int64(80_000), rows[1].Amount)
	require.Equal(t, "payment_mismatch", rows[1].Reason)
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM river_job WHERE args::text LIKE '%đã được thanh toán trước đó%'`))
}

// Ruling 12: two different transfers on one order code at the same time:
// one confirms, the other is refunded as a duplicate.
func TestWebhook_ConcurrentDifferentReferences(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	bodies := [][]byte{e.Fake.MarkPaidRef(oc, gross, "R1"), e.Fake.MarkPaidRef(oc, gross, "R2")}
	var wg sync.WaitGroup
	codes := make(chan int, len(bodies))
	for _, b := range bodies {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(b))
			rec := httptest.NewRecorder()
			e.Handler.ServeHTTP(rec, req)
			codes <- rec.Code
		})
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		require.Equal(t, http.StatusOK, c)
	}
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	rows := e.refunds(t)
	require.Len(t, rows, 1)
	require.Equal(t, gross, rows[0].Amount)
	require.Equal(t, "duplicate_payment", rows[0].Reason)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history WHERE to_status = 'confirmed'`))
	e.requireBalance(t, 342_000, 0)
}

// Ruling 12: overpaid AND too late: the booking's gross back as a late
// refund, the excess as a mismatch, no ledger entry.
func TestLatePaid_OverpaidAndIneligible(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.expire(t, v.ID)
	_, err := e.Listing.Hide(context.Background(), e.Admin, e.L.ID, "Vi phạm nội dung")
	require.NoError(t, err)
	e.pay(t, oc, 400_000)

	require.Equal(t, "expired", e.bookingStatus(t, v.ID))
	require.Equal(t, "needs_review", e.paymentStatus(t, oc))
	id, r := v.ID, ref(oc)
	require.Equal(t, []refundRow{
		{Amount: gross, Reason: "late_payment", Status: "awaiting_bank_info", BookingID: &id},
		{Amount: 20_000, Reason: "payment_mismatch", Status: "awaiting_bank_info", Reference: &r},
	}, e.refunds(t))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM platform_commission_entries`))
}

// Ruling 12: a provider error other than "not found" never rotates the
// order code; not found does.
func TestCreateLinkFailure_RotatesOnlyOnNotFound(t *testing.T) {
	e := newEnv(t)
	e.Fake.FailCreateLink(errors.New("payos down"))
	e.Fake.FailNextStatus(&payment.ProviderError{Code: "20", Desc: "Lỗi hệ thống"})
	v := e.create(t, "a")
	require.Nil(t, v.Payment.CheckoutURL)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments`))

	e.Fake.FailNextStatus(errors.New("connection reset"))
	_, err := e.Svc.Pay(context.Background(), e.Customer, v.ID)
	requireErr(t, err, http.StatusBadGateway, "payment_provider_unavailable")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments`))

	// Not found at the provider (the fake never recorded the link): rotated.
	_, err = e.Svc.Pay(context.Background(), e.Customer, v.ID)
	requireErr(t, err, http.StatusBadGateway, "payment_provider_unavailable")
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM payments`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments WHERE status = 'cancelled'`))
}
