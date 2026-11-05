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

// Results stored in payment_events.result by the webhook itself; the
// PaymentHandler returns the others.
const (
	ResultUnknownOrder = "unknown_order"
	ResultNotPaid      = "not_paid"
)

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
	// ErrInvalidAmount: a simulated payment amount must be positive.
	ErrInvalidAmount = httpx.NewError(http.StatusUnprocessableEntity, "invalid_amount", "Số tiền phải lớn hơn 0")
)

// PaidEvent is money confirmed by the provider on one payment.
type PaidEvent struct {
	PaymentID, CheckoutID uuid.UUID
	OrderCode             int64
	AmountPaid            int64
	// Reference is the bank reference of the transfer.
	Reference string
	PaidAt    time.Time
	// Raw is the verified webhook "data" object.
	Raw json.RawMessage
}

// PaymentHandler applies a paid event (booking.Service implements it). It
// runs inside the webhook's tx, which already recorded the event's
// payment_events row (the dedupe key): any error rolls both back, and the
// provider retries the webhook. It returns the result stored on the event.
type PaymentHandler interface {
	HandlePaid(ctx context.Context, tx pgx.Tx, p PaidEvent) (result string, err error)
}

// DedupeKey identifies one incoming transfer on one order code.
func DedupeKey(orderCode int64, reference string) string {
	return "payos:" + strconv.FormatInt(orderCode, 10) + ":" + reference
}

// ProcessWebhook handles a PayOS payment webhook body:
//
//  1. VerifyWebhook: a bad signature is ErrInvalidWebhook, nothing is stored.
//  2. A dedupe_key already in payment_events: done (a repeated delivery).
//  3. An order code we do not know (PayOS tests the URL with one): the event
//     is stored as unknown_order and that is all.
//  4. Provider.GetStatus re-checks the order OUTSIDE any tx; not PAID is
//     stored as not_paid.
//  5. In one tx: insert the payment_events row (a concurrent delivery of the
//     same transfer inserts nothing and stops there), h.HandlePaid, store
//     its result.
//
// Any other error is transient: nothing of step 5 is kept and the caller
// answers 500 so that the provider retries.
func (s *Service) ProcessWebhook(ctx context.Context, body []byte, h PaymentHandler) error {
	wd, err := s.provider.VerifyWebhook(body)
	if err != nil {
		s.log.Warn("payment: webhook rejected", "err", err)
		return ErrInvalidWebhook
	}
	key := DedupeKey(wd.OrderCode, wd.Reference)
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
	if st.State != StatePaid {
		s.log.Warn("payment: webhook says paid, provider does not",
			"order_code", wd.OrderCode, "state", st.State, "reference", wd.Reference)
		return s.storeEvent(ctx, q, key, &p.ID, body, ResultNotPaid, now)
	}

	paidAt := wd.TransactionAt
	if paidAt.IsZero() {
		paidAt = now
	}
	ev := PaidEvent{
		PaymentID: p.ID, CheckoutID: p.CheckoutID, OrderCode: wd.OrderCode, AmountPaid: wd.Amount,
		Reference: wd.Reference, PaidAt: paidAt, Raw: wd.Raw,
	}
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
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
		result, err := h.HandlePaid(ctx, tx, ev)
		if err != nil {
			return err
		}
		if err := tq.FinishPaymentEvent(ctx, store.FinishPaymentEventParams{Now: s.clock.Now(), Result: result, DedupeKey: key}); err != nil {
			return fmt.Errorf("finish payment event: %w", err)
		}
		return nil
	})
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
