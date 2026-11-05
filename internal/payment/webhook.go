package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/payment/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// ResultUnknownOrder is stored in payment_events.result for an order code
// we do not know; the PaymentHandler returns the other results.
const ResultUnknownOrder = "unknown_order"

const (
	// WebhookPath is where PayOS posts payment webhooks.
	WebhookPath = "/v1/payments/payos/webhook"
	// webhookPerMinute is the per-IP rate limit of the webhook.
	webhookPerMinute = 600
	maxWebhookBytes  = 1 << 20
)

var (
	// ErrInvalidWebhook: the webhook body is malformed or its signature is
	// wrong. Nothing is stored.
	ErrInvalidWebhook = httpx.NewError(http.StatusBadRequest, "invalid_signature", "Webhook không hợp lệ hoặc sai chữ ký")
	// ErrPaymentNotConfirmed: the provider does not (yet) report any money
	// on the order. Nothing is stored; the provider retries the webhook.
	ErrPaymentNotConfirmed = httpx.NewError(http.StatusServiceUnavailable, "payment_not_confirmed", "Cổng thanh toán chưa xác nhận giao dịch, vui lòng gửi lại sau")
	// ErrInvalidAmount: a simulated payment amount must be positive.
	ErrInvalidAmount = httpx.NewError(http.StatusUnprocessableEntity, "invalid_amount", "Số tiền phải lớn hơn 0")
)

// PaidEvent is money confirmed by the provider on one payment.
type PaidEvent struct {
	PaymentID, CheckoutID uuid.UUID
	OrderCode             int64
	// AmountPaid is the money this transfer brings: the signed webhook
	// amount, capped by the provider's amountPaid (which is cumulative over
	// transfers and is the authority on an underpayment).
	AmountPaid int64
	// Reference identifies the transfer: the bank reference, or a fallback
	// when the provider sent none (see TransferReference).
	Reference string
	PaidAt    time.Time
	// Raw is the verified webhook "data" object.
	Raw json.RawMessage
}

// PaidOutcome is what a PaymentHandler did with a PaidEvent.
type PaidOutcome struct {
	// Result is stored in payment_events.result.
	Result string
	// CancelOrderCodes are payment links to cancel at the provider once
	// the tx committed (best effort).
	CancelOrderCodes []int64
}

// PaymentHandler applies a paid event (booking.Service implements it). It
// runs inside the webhook's tx, which already recorded the event's
// payment_events row (the dedupe key): any error rolls both back, and the
// provider retries the webhook.
type PaymentHandler interface {
	HandlePaid(ctx context.Context, tx pgx.Tx, p PaidEvent) (PaidOutcome, error)
}

// DedupeKey identifies one incoming transfer on one order code.
func DedupeKey(orderCode int64, reference string) string {
	return "payos:" + strconv.FormatInt(orderCode, 10) + ":" + reference
}

// TransferReference is the reference identifying a webhook's transfer: the
// bank reference; without one "link:" + paymentLinkId; without that either
// "event:" + orderCode + ":" + the raw transactionDateTime.
func TransferReference(wd WebhookData) string {
	switch {
	case wd.Reference != "":
		return wd.Reference
	case wd.PaymentLinkID != "":
		return "link:" + wd.PaymentLinkID
	default:
		return "event:" + strconv.FormatInt(wd.OrderCode, 10) + ":" + wd.TransactionDateTime
	}
}

// moneyReceived reports how much of this transfer the provider confirms:
// state PAID or UNDERPAID, or any amountPaid, counts as money (Ruling 9).
// The webhook amount is capped by amountPaid when the provider reports one.
func moneyReceived(st Status, webhookAmount int64) (int64, bool) {
	if st.State != StatePaid && st.State != StateUnderpaid && st.AmountPaid <= 0 {
		return 0, false
	}
	amount := webhookAmount
	if st.AmountPaid > 0 && (amount <= 0 || st.AmountPaid < amount) {
		amount = st.AmountPaid
	}
	return amount, amount > 0
}

// ProcessWebhook handles a PayOS payment webhook body:
//
//  1. VerifyWebhook: a bad signature is ErrInvalidWebhook, nothing is stored.
//  2. A dedupe_key already in payment_events: done (a repeated delivery).
//  3. An order code we do not know (PayOS tests the URL with one): the event
//     is stored as unknown_order and that is all.
//  4. Provider.GetStatus re-checks the order OUTSIDE any tx. Money-first
//     (Ruling 9): PAID, UNDERPAID or amountPaid > 0 is money; otherwise
//     (PENDING, PROCESSING, unknown with nothing paid) nothing is stored and
//     the answer is ErrPaymentNotConfirmed (503), so the provider retries
//     and a later delivery is processed normally.
//  5. In one tx: insert the payment_events row (a concurrent delivery of the
//     same transfer inserts nothing and stops there), h.HandlePaid, store
//     its result. After commit, links the handler replaced are cancelled at
//     the provider (best effort).
//
// Any other error is transient: nothing of step 5 is kept and the caller
// answers 500 so that the provider retries.
func (s *Service) ProcessWebhook(ctx context.Context, body []byte, h PaymentHandler) error {
	wd, err := s.provider.VerifyWebhook(body)
	if err != nil {
		s.log.Warn("payment: webhook rejected", "err", err)
		return ErrInvalidWebhook
	}
	reference := TransferReference(wd)
	key := DedupeKey(wd.OrderCode, reference)
	q := store.New(s.pool)
	seen, err := q.PaymentEventExists(ctx, key)
	if err != nil {
		return fmt.Errorf("payment event exists: %w", err)
	}
	if seen {
		return nil
	}

	now := s.clock.Now()
	p, err := s.GetByOrderCode(ctx, wd.OrderCode)
	if errors.Is(err, ErrPaymentNotFound) {
		s.log.Info("payment: webhook for an unknown order code", "order_code", wd.OrderCode)
		return s.storeEvent(ctx, q, key, nil, body, ResultUnknownOrder, now)
	}
	if err != nil {
		return err
	}

	st, err := s.provider.GetStatus(ctx, wd.OrderCode)
	if err != nil {
		return fmt.Errorf("payment: confirm order %d with the provider: %w", wd.OrderCode, err)
	}
	amount, ok := moneyReceived(st, wd.Amount)
	if !ok {
		s.log.Warn("payment: webhook says paid, provider reports no money yet; asking for a retry",
			"order_code", wd.OrderCode, "state", st.State, "reference", reference)
		return ErrPaymentNotConfirmed
	}

	paidAt := wd.TransactionAt
	if paidAt.IsZero() {
		paidAt = now
	}
	ev := PaidEvent{
		PaymentID: p.ID, CheckoutID: p.CheckoutID, OrderCode: wd.OrderCode, AmountPaid: amount,
		Reference: reference, PaidAt: paidAt, Raw: wd.Raw,
	}
	var out PaidOutcome
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		tq := store.New(tx)
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		n, err := tq.InsertPaymentEvent(ctx, store.InsertPaymentEventParams{
			ID: id, PaymentID: &p.ID, DedupeKey: key, Payload: body, Now: now,
		})
		if err != nil {
			return fmt.Errorf("insert payment event: %w", err)
		}
		if n == 0 {
			return nil // a concurrent delivery of the same transfer committed first
		}
		out, err = h.HandlePaid(ctx, tx, ev)
		if err != nil {
			return err
		}
		if err := tq.FinishPaymentEvent(ctx, store.FinishPaymentEventParams{Now: s.clock.Now(), Result: out.Result, DedupeKey: key}); err != nil {
			return fmt.Errorf("finish payment event: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, oc := range out.CancelOrderCodes {
		s.CancelLinkBestEffort(context.WithoutCancel(ctx), oc)
	}
	return nil
}

// storeEvent records an event that needs no further processing.
func (s *Service) storeEvent(ctx context.Context, q *store.Queries, key string, paymentID *uuid.UUID, body []byte, result string, now time.Time) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if _, err := q.InsertPaymentEvent(ctx, store.InsertPaymentEventParams{
		ID: id, PaymentID: paymentID, DedupeKey: key, Payload: body, Now: now, ProcessedAt: &now, Result: &result,
	}); err != nil {
		return fmt.Errorf("insert payment event: %w", err)
	}
	return nil
}

// RouteDeps configures Routes.
type RouteDeps struct {
	Handler  PaymentHandler
	ClientIP func(*http.Request) string
	// AppEnv is dev, test or prod.
	AppEnv string
}

// Routes registers the public PayOS webhook (rate limited per IP) and, only
// when the provider is the in-memory Fake and AppEnv is not prod, the dev
// route POST /v1/dev/payments/{order_code}/simulate-paid, which signs a
// webhook with the Fake and feeds it to the very same webhook handler.
func (s *Service) Routes(mux httpx.Router, d RouteDeps) {
	webhook := s.webhookHandler(d.Handler)
	mux.Handle("POST "+WebhookPath, httpx.RateLimit(webhookPerMinute, webhookPerMinute, d.ClientIP)(webhook))
	if fake, ok := s.provider.(*Fake); ok && d.AppEnv != "prod" {
		mux.Handle("POST /v1/dev/payments/{order_code}/simulate-paid", s.simulatePaid(fake, webhook))
	}
}

func (s *Service) webhookHandler(h PaymentHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes+1))
		if err != nil || len(body) > maxWebhookBytes {
			httpx.WriteError(w, r, ErrInvalidWebhook)
			return
		}
		if err := s.ProcessWebhook(r.Context(), body, h); err != nil {
			var ae *httpx.AppError
			if !errors.As(err, &ae) {
				s.log.Error("payment: webhook failed, the provider will retry", "err", err)
			}
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
	})
}

// simulatePaid marks an order paid at the Fake (body {"amount"?}, default the
// payment amount) and posts the signed webhook to the webhook handler.
func (s *Service) simulatePaid(fake *Fake, webhook http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oc, err := strconv.ParseInt(r.PathValue("order_code"), 10, 64)
		if err != nil {
			httpx.WriteError(w, r, ErrPaymentNotFound)
			return
		}
		var in struct {
			Amount *int64 `json:"amount"`
		}
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, err := s.GetByOrderCode(r.Context(), oc)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		amount := p.Amount
		if in.Amount != nil {
			amount = *in.Amount
		}
		if amount <= 0 {
			httpx.WriteError(w, r, ErrInvalidAmount)
			return
		}
		body := fake.MarkPaid(oc, amount)
		req := r.Clone(r.Context())
		req.Method, req.URL.Path = http.MethodPost, WebhookPath
		req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		webhook.ServeHTTP(w, req)
	})
}
