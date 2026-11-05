package booking_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	webhookPath = "/v1/payments/payos/webhook"
	simulatePat = "POST /v1/dev/payments/{order_code}/simulate-paid"
	// gross 380.000 at 1000 bps: commission 38.000, partner 342.000.
	gross      = int64(380_000)
	commission = int64(38_000)
)

// webhook posts a raw PayOS webhook body through the real mux.
func (e *env) webhook(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.Handler.ServeHTTP(rec, req)
	return rec
}

// pay delivers a signed "paid" webhook for orderCode and requires a 200.
func (e *env) pay(t *testing.T, orderCode, amount int64) {
	t.Helper()
	rec := e.webhook(t, e.Fake.MarkPaid(orderCode, amount))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// orderCode is the order code of the booking's pending payment.
func (e *env) orderCode(t *testing.T, bookingID uuid.UUID) int64 {
	t.Helper()
	var oc int64
	require.NoError(t, e.Pool.QueryRow(context.Background(), `SELECT p.provider_order_code
		FROM payments p JOIN bookings b ON b.checkout_id = p.checkout_id
		WHERE b.id = $1 AND p.status = 'pending'`, bookingID).Scan(&oc))
	return oc
}

func (e *env) str(t *testing.T, query string, args ...any) string {
	t.Helper()
	var s string
	require.NoError(t, e.Pool.QueryRow(context.Background(), query, args...).Scan(&s))
	return s
}

func (e *env) bookingStatus(t *testing.T, id uuid.UUID) string {
	t.Helper()
	return e.str(t, `SELECT status FROM bookings WHERE id = $1`, id)
}

func (e *env) paymentStatus(t *testing.T, orderCode int64) string {
	t.Helper()
	return e.str(t, `SELECT status FROM payments WHERE provider_order_code = $1`, orderCode)
}

func (e *env) checkoutStatus(t *testing.T, bookingID uuid.UUID) string {
	t.Helper()
	return e.str(t, `SELECT c.status FROM checkouts c JOIN bookings b ON b.checkout_id = c.id WHERE b.id = $1`, bookingID)
}

func (e *env) requireBalance(t *testing.T, pending, available int64) {
	t.Helper()
	b, err := e.Ledger.Balance(context.Background(), e.P.ID)
	require.NoError(t, err)
	require.Equal(t, ledger.Balance{Pending: pending, Available: available}, b)
	require.NoError(t, e.Ledger.Verify(context.Background(), e.P.ID))
}

func (e *env) emails(t *testing.T) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'`)
}

type refundRow struct {
	Amount    int64
	Reason    string
	Status    string
	BookingID *uuid.UUID
	Reference *string
	BankBin   *string
	Account   *string
	Holder    *string
}

func (e *env) refunds(t *testing.T) []refundRow {
	t.Helper()
	rows, err := e.Pool.Query(context.Background(), `SELECT amount, reason, status, booking_id, provider_reference,
		bank_bin, account_number, account_holder FROM refunds ORDER BY created_at, id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []refundRow
	for rows.Next() {
		var r refundRow
		require.NoError(t, rows.Scan(&r.Amount, &r.Reason, &r.Status, &r.BookingID, &r.Reference, &r.BankBin, &r.Account, &r.Holder))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func ref(oc int64) string { return "FAKE-REF-" + strconv.FormatInt(oc, 10) }

// expire puts a booking in the state the hold-expiry job leaves it in.
func (e *env) expire(t *testing.T, id uuid.UUID) {
	t.Helper()
	e.exec(t, `UPDATE bookings SET status = 'expired' WHERE id = $1`, id)
	e.exec(t, `UPDATE payments SET status = 'expired' WHERE status = 'pending'
		AND checkout_id = (SELECT checkout_id FROM bookings WHERE id = $1)`, id)
	e.exec(t, `UPDATE checkouts SET status = 'closed' WHERE id = (SELECT checkout_id FROM bookings WHERE id = $1)`, id)
	e.Clock.Advance(30 * time.Minute)
}

func TestPaid_Confirms(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, gross)

	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, "paid", e.checkoutStatus(t, v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
	require.Equal(t, ref(oc), e.str(t, `SELECT provider_reference FROM payments WHERE provider_order_code = $1`, oc))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM bookings WHERE id = $1 AND confirmed_at = $2`, v.ID, testNow))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'pending_payment' AND to_status = 'confirmed' AND actor_kind = 'system'`, v.ID))
	e.requireBalance(t, 342_000, 0)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM platform_commission_entries
		WHERE booking_id = $1 AND kind = 'commission' AND amount = $2`, v.ID, commission))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events
		WHERE dedupe_key = $1 AND result = 'confirmed' AND processed_at IS NOT NULL AND signature_valid`,
		"payos:"+strconv.FormatInt(oc, 10)+":"+ref(oc)))
	require.Empty(t, e.refunds(t))
	require.Equal(t, 1, e.emails(t))

	mine, err := e.Svc.GetMine(context.Background(), e.Customer, v.ID)
	require.NoError(t, err)
	require.Equal(t, "confirmed", mine.Status)
	require.NotNil(t, mine.QRToken)
	require.Nil(t, mine.Payment)
}

// Review Focus 1: the same webhook twice confirms and posts once.
func TestWebhook_DuplicateDelivery(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	body := e.Fake.MarkPaid(oc, gross)
	for range 2 {
		rec := e.webhook(t, body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history WHERE to_status = 'confirmed'`))
	e.requireBalance(t, 342_000, 0)
	require.Equal(t, 1, e.emails(t))
}

// Review Focus 1: concurrent deliveries confirm and post once.
func TestWebhook_ConcurrentDelivery(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	body := e.Fake.MarkPaid(oc, gross)

	const n = 5
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body))
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
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_settlements`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history WHERE to_status = 'confirmed'`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events`))
	e.requireBalance(t, 342_000, 0)
}

func TestWebhook_BadSignature400(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	var w map[string]any
	require.NoError(t, json.Unmarshal(e.Fake.MarkPaid(oc, gross), &w))
	w["signature"] = "00" + w["signature"].(string)[2:]
	body, err := json.Marshal(w)
	require.NoError(t, err)

	requireCode(t, e.webhook(t, body), http.StatusBadRequest, "invalid_signature")
	requireCode(t, e.webhook(t, []byte(`not json`)), http.StatusBadRequest, "invalid_signature")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM payment_events`))
	require.Equal(t, "pending_payment", e.bookingStatus(t, v.ID))
}

// PayOS sends a test webhook when the URL is registered.
func TestWebhook_UnknownOrder200(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	e.pay(t, 999_999_999, gross)
	require.Equal(t, "pending_payment", e.bookingStatus(t, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events
		WHERE payment_id IS NULL AND result = 'unknown_order'`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))
}

// Ruling 9: the provider's status lags behind its webhook. The first
// delivery stores nothing and asks for a retry (503); the retry confirms.
func TestWebhook_ProviderSaysNotPaid(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	body := e.Fake.MarkPaid(oc, gross)
	e.Fake.LagNextStatus()

	requireCode(t, e.webhook(t, body), http.StatusServiceUnavailable, "payment_not_confirmed")
	require.Equal(t, "pending_payment", e.bookingStatus(t, v.ID))
	require.Equal(t, "pending", e.paymentStatus(t, oc))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events
		WHERE dedupe_key = $1 AND result = 'awaiting_status'`, "payos:"+strconv.FormatInt(oc, 10)+":"+ref(oc)+":awaiting"),
		"only the tracking row: no dedupe row may block the retry")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))

	rec := e.webhook(t, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE result = 'confirmed'`))
	e.requireBalance(t, 342_000, 0)
}

func TestPaid_Underpaid(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, 300_000)
	st, err := e.Fake.GetStatus(context.Background(), oc)
	require.NoError(t, err)
	require.Equal(t, payment.Status{State: payment.StateUnderpaid, AmountPaid: 300_000}, st,
		"the provider itself reports UNDERPAID")

	require.Equal(t, "pending_payment", e.bookingStatus(t, v.ID))
	require.Equal(t, "needs_review", e.paymentStatus(t, oc))
	r := ref(oc)
	require.Equal(t, []refundRow{{Amount: 300_000, Reason: "payment_mismatch", Status: "awaiting_bank_info", Reference: &r}},
		e.refunds(t))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM booking_settlements`))
	require.Equal(t, 1, e.emails(t))
}

func TestPaid_Overpaid(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, 400_000)

	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, "needs_review", e.paymentStatus(t, oc))
	require.Equal(t, "paid", e.checkoutStatus(t, v.ID))
	r := ref(oc)
	require.Equal(t, []refundRow{{Amount: 20_000, Reason: "payment_mismatch", Status: "awaiting_bank_info", Reference: &r}},
		e.refunds(t))
	e.requireBalance(t, 342_000, 0)
}

func TestPaid_DuplicatePaymentRefunded(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, gross)

	// A second transfer on the same order code (another bank reference).
	rec := e.webhook(t, e.Fake.MarkPaidRef(oc, gross, "SECOND-REF"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
	second := "SECOND-REF"
	require.Equal(t, []refundRow{{Amount: gross, Reason: "duplicate_payment", Status: "awaiting_bank_info", Reference: &second}},
		e.refunds(t))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries`))
	e.requireBalance(t, 342_000, 0)
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM payment_events`))
}

// Review Focus 2: paid after the hold expired, still bookable: confirmed.
func TestLatePaid_ReconfirmsWhenStillBookable(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.expire(t, v.ID)
	e.pay(t, oc, gross)

	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
	require.Equal(t, "paid", e.checkoutStatus(t, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'expired' AND to_status = 'confirmed'
		AND actor_kind = 'system' AND reason = 'thanh toán trễ, xác nhận lại'`, v.ID))
	require.Empty(t, e.refunds(t))
	e.requireBalance(t, 342_000, 0)
}

// requireLateRefund: the booking stays expired, the money goes back in full
// and the partner's ledger is untouched.
func (e *env) requireLateRefund(t *testing.T, v booking.BookingView, oc int64) {
	t.Helper()
	require.Equal(t, "expired", e.bookingStatus(t, v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
	id := v.ID
	require.Equal(t, []refundRow{{Amount: gross, Reason: "late_payment", Status: "awaiting_bank_info", BookingID: &id}},
		e.refunds(t))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM booking_settlements`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM platform_commission_entries`))
	require.Equal(t, 1, e.emails(t))
}

// Review Focus 2: the listing was hidden meanwhile.
func TestLatePaid_RefundsWhenListingHidden(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.expire(t, v.ID)
	_, err := e.Listing.Hide(context.Background(), e.Admin, e.L.ID, "Vi phạm nội dung")
	require.NoError(t, err)
	e.pay(t, oc, gross)
	e.requireLateRefund(t, v, oc)
}

// Review Focus 2: the partner was suspended meanwhile.
func TestLatePaid_RefundsWhenPartnerSuspended(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.expire(t, v.ID)
	require.NoError(t, e.Partner.Suspend(context.Background(), e.Admin, e.P.ID, "Vi phạm hợp đồng"))
	e.pay(t, oc, gross)
	e.requireLateRefund(t, v, oc)
}

// Review Focus 2: the slot already started when the money arrived.
func TestLatePaid_RefundsWhenSlotPassed(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.expire(t, v.ID)
	e.Clock.T = time.Date(2026, 11, 7, 8, 30, 0, 0, clock.VN)
	e.pay(t, oc, gross)
	e.requireLateRefund(t, v, oc)
}

// The slot no longer runs on the visit date (closed meanwhile).
func TestLatePaid_RefundsWhenSlotClosed(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.expire(t, v.ID)
	e.exec(t, `UPDATE attraction_time_slots SET active = false WHERE id = $1`, e.Slot.ID)
	e.pay(t, oc, gross)
	e.requireLateRefund(t, v, oc)
}

func TestRefundPrefilledFromSavedAccount(t *testing.T) {
	e := newEnv(t)
	e.exec(t, `INSERT INTO customer_refund_accounts (user_id, bank_bin, account_number, account_holder)
		VALUES ($1, '970436', '0123456789', 'NGUYEN VAN AN')`, e.Customer)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.pay(t, oc, 300_000)

	r, bin, acc, holder := ref(oc), "970436", "0123456789", "NGUYEN VAN AN"
	require.Equal(t, []refundRow{{
		Amount: 300_000, Reason: "payment_mismatch", Status: "pending", Reference: &r,
		BankBin: &bin, Account: &acc, Holder: &holder,
	}}, e.refunds(t))
}

// Ruling 8: a lost CreateLink response is recovered right away: the order
// code is cancelled at the provider and replaced by a fresh one.
func TestCreate_LostCreateResponseRecoveredInline(t *testing.T) {
	e := newEnv(t)
	e.Fake.LoseNextCreateResponse()
	v := e.create(t, "k")
	require.NotNil(t, v.Payment)
	require.NotNil(t, v.Payment.CheckoutURL)
	oc := e.orderCode(t, v.ID)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments WHERE status = 'cancelled' AND provider_order_code < $1`, oc))
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM payments`))
}

// Ruling 8: a CreateLink whose response was lost does not wedge the payment;
// money later arriving on an abandoned order code is refunded.
func TestPay_LostCreateResponseRotatesOrderCode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Both the first link and its inline replacement lose their response.
	e.Fake.LoseNextCreateResponse()
	e.Fake.LoseNextCreateResponse()
	v := e.create(t, "k")
	require.NotNil(t, v.Payment)
	require.Nil(t, v.Payment.CheckoutURL)
	var firstOC int64
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT min(provider_order_code) FROM payments`).Scan(&firstOC))
	lostOC := e.orderCode(t, v.ID)
	st, err := e.Fake.GetStatus(ctx, lostOC)
	require.NoError(t, err)
	require.Equal(t, payment.StatePending, st.State, "the link exists at the provider")

	tok := e.token(t, e.Customer, auth.RoleCustomer)
	rec := e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/pay", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decode[booking.BookingView](t, rec)
	require.NotNil(t, got.Payment.CheckoutURL)
	newOC := e.orderCode(t, v.ID)
	require.NotEqual(t, lostOC, newOC)
	require.Equal(t, "cancelled", e.paymentStatus(t, lostOC))
	st, err = e.Fake.GetStatus(ctx, lostOC)
	require.NoError(t, err)
	require.Equal(t, payment.StateCancelled, st.State, "the lost link was cancelled at the provider")
	var oldExp, newExp time.Time
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT
		(SELECT expires_at FROM payments WHERE provider_order_code = $1),
		(SELECT expires_at FROM payments WHERE provider_order_code = $2)`, lostOC, newOC).Scan(&oldExp, &newExp))
	require.True(t, oldExp.Equal(newExp))
	require.Equal(t, 3, e.count(t, `SELECT count(*) FROM payments`))

	e.pay(t, newOC, gross)
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))

	// The customer still paid an old QR: refunded, never confirmed twice.
	e.pay(t, lostOC, gross)
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, "needs_review", e.paymentStatus(t, lostOC))
	lostRef := ref(lostOC)
	require.Equal(t, []refundRow{{Amount: gross, Reason: "duplicate_payment", Status: "awaiting_bank_info", Reference: &lostRef}},
		e.refunds(t))

	// And the very first QR too.
	e.pay(t, firstOC, gross)
	require.Len(t, e.refunds(t), 2)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history WHERE to_status = 'confirmed'`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries`))
	e.requireBalance(t, 342_000, 0)
}

// Ruling 8: the lost link was already paid: no new link, the webhook confirms.
func TestPay_LostCreateResponseAlreadyPaid(t *testing.T) {
	e := newEnv(t)
	e.Fake.LoseNextCreateResponse()
	e.Fake.LoseNextCreateResponse()
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	body := e.Fake.MarkPaid(oc, gross) // paid, webhook not delivered yet

	_, err := e.Svc.Pay(context.Background(), e.Customer, v.ID)
	requireErr(t, err, http.StatusConflict, "payment_processing")
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM payments`))
	require.Equal(t, "pending", e.paymentStatus(t, oc))

	rec := e.webhook(t, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
}

// Task 5 review: replaying the creation retries a missing payment link.
func TestCreate_ReplayActivatesMissingLink(t *testing.T) {
	e := newEnv(t)
	e.Fake.FailCreateLink(errors.New("payos down"))
	v := e.create(t, "k")
	require.Nil(t, v.Payment.CheckoutURL)
	e.Fake.FailCreateLink(nil)
	again := e.create(t, "k")
	require.Equal(t, v.ID, again.ID)
	require.NotNil(t, again.Payment)
	require.NotNil(t, again.Payment.CheckoutURL)
}

// Task 5 review: only the customer sees the payment link.
func TestPartnerAndAdminViews_NoPaymentLink(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := e.create(t, "k")
	require.NotNil(t, v.Payment)
	pv, err := e.Svc.GetForPartner(ctx, e.P.ID, v.ID)
	require.NoError(t, err)
	require.Nil(t, pv.Payment)
	av, err := e.Svc.AdminGet(ctx, v.ID)
	require.NoError(t, err)
	require.Nil(t, av.Payment)
}

func TestDevSimulateRoute_OnlyWhenFake(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)

	patterns := func(p payment.Provider, appEnv string) []string {
		rr := httpx.NewRecordingRouter(http.NewServeMux())
		payment.NewService(e.Pool, e.Clock, p, nil).Routes(rr,
			payment.RouteDeps{Handler: e.Svc, ClientIP: httpx.ClientIPFunc(0), AppEnv: appEnv})
		return rr.Patterns()
	}
	payos := payment.NewPayOS(payment.PayOSConfig{ClientID: "c", APIKey: "k", ChecksumKey: "s"}, nil)
	for _, env := range []string{"dev", "test"} {
		require.NotContains(t, patterns(payos, env), simulatePat, env)
		require.Contains(t, patterns(payos, env), "POST "+webhookPath, env)
		require.Contains(t, patterns(payment.NewFake(), env), simulatePat, env)
	}

	// The simulated payment goes through the real webhook flow.
	path := "/v1/dev/payments/" + strconv.FormatInt(oc, 10) + "/simulate-paid"
	requireCode(t, e.do(t, "POST", path, "", `{"amount": 1, "x": 1}`), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "POST", "/v1/dev/payments/123/simulate-paid", "", `{}`), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "POST", "/v1/dev/payments/abc/simulate-paid", "", `{}`), http.StatusNotFound, "not_found")
	rec := e.do(t, "POST", path, "", `{}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "confirmed", e.bookingStatus(t, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events WHERE result = 'confirmed'`))
	e.requireBalance(t, 342_000, 0)

	// An explicit amount: a second booking underpaid.
	v2 := e.create(t, "k2")
	oc2 := e.orderCode(t, v2.ID)
	rec = e.do(t, "POST", "/v1/dev/payments/"+strconv.FormatInt(oc2, 10)+"/simulate-paid", "", `{"amount": 1000}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "needs_review", e.paymentStatus(t, oc2))
}

func TestDevSimulateRoute_NeverInProd(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)

	mux := http.NewServeMux()
	rr := httpx.NewRecordingRouter(mux)
	e.Payments.Routes(rr, payment.RouteDeps{Handler: e.Svc, ClientIP: httpx.ClientIPFunc(0), AppEnv: "prod"})
	require.NotContains(t, rr.Patterns(), simulatePat)
	require.Contains(t, rr.Patterns(), "POST "+webhookPath)
	mux.HandleFunc("/", httpx.NotFound)

	req := httptest.NewRequest(http.MethodPost, "/v1/dev/payments/"+strconv.FormatInt(oc, 10)+"/simulate-paid",
		bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "pending_payment", e.bookingStatus(t, v.ID))
}
