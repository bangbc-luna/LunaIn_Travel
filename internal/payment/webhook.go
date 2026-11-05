package payment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// ResultUnknownOrder: an order code we do not know.
	ResultUnknownOrder = "unknown_order"
	// ResultAwaitingStatus: the tracking row (dedupe key + ":awaiting") of a
	// transfer the provider's status did not show yet.
	ResultAwaitingStatus = "awaiting_status"
	// ResultNoAmount: a signed webhook without a positive amount.
	ResultNoAmount = "no_amount"
)

// StatusLagLimit is how long a transfer whose signed webhook the provider's
// status does not reflect (PENDING/PROCESSING, nothing paid) is retried
// before it is processed anyway, flagged needs_review.
const StatusLagLimit = 30 * time.Minute

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
	// AmountPaid is the money this transfer brings: always the signed
	// per-transfer webhook amount (Ruling 13), never the provider's
	// cumulative amountPaid.
	AmountPaid int64
	// NeedsReview: the provider's status did not confirm the money; the
	// payment must end up needs_review whatever else happens.
	NeedsReview bool
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
// bank reference, or without one "raw:" + lowercase hex sha256 of the signed
// "data" JSON exactly as received (Ruling 16): stable across redeliveries of
// one transfer, distinct for different transfers on the same link.
func TransferReference(wd WebhookData) string {
	if wd.Reference != "" {
		return wd.Reference
	}
	sum := sha256.Sum256(wd.Raw)
	return "raw:" + hex.EncodeToString(sum[:])
}

// statusShowsMoney: state PAID or UNDERPAID, or any amount paid.
func statusShowsMoney(st Status) bool {
	return st.State == StatePaid || st.State == StateUnderpaid || st.AmountPaid > 0
}

// ProcessWebhook handles a PayOS payment webhook body:
//
//  1. VerifyWebhook: a bad signature is ErrInvalidWebhook, nothing is stored.
//     A valid signature with code "00" and amount > 0 is proof of a transfer
//     of exactly data.amount (Ruling 13).
//  2. A dedupe_key already in payment_events: done (a repeated delivery).
//  3. An order code we do not know (PayOS tests the URL with one): the event
//     is stored as unknown_order and that is all.
//  4. Provider.GetStatus, OUTSIDE any tx, only detects a lagging status:
//     - it shows money (PAID, UNDERPAID or amountPaid > 0): processed;
//     - PENDING or PROCESSING with nothing paid: the first sighting stores a
//     tracking row (dedupe key + ":awaiting") and answers
//     ErrPaymentNotConfirmed (503) so the provider retries; once that row
//     is StatusLagLimit old, a delivery is processed, flagged needs_review;
//     - any other state with nothing paid (CANCELLED, EXPIRED, FAILED,
//     unknown): processed at once, flagged needs_review. Money is never
//     dropped.
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
	if wd.Amount <= 0 {
		s.log.Warn("payment: signed webhook without amount", "order_code", wd.OrderCode, "reference", reference)
		return s.storeEvent(ctx, q, key, &p.ID, body, ResultNoAmount, now)
	}

	st, err := s.provider.GetStatus(ctx, wd.OrderCode)
	if err != nil {
		return fmt.Errorf("payment: confirm order %d with the provider: %w", wd.OrderCode, err)
	}
	needsReview := false
	if !statusShowsMoney(st) || st.AmountPaid < wd.Amount {
		s.log.Warn("payment: provider status disagrees with the signed webhook",
			"order_code", wd.OrderCode, "reference", reference, "webhook_amount", wd.Amount,
			"status_state", st.State, "status_amount_paid", st.AmountPaid)
	}
	if !statusShowsMoney(st) {
		if st.State == StatePending || st.State == StateProcessing {
			waited, err := s.awaitStatus(ctx, q, key, p.ID, body, now)
			if err != nil {
				return err
			}
			if !waited {
				return ErrPaymentNotConfirmed
			}
		}
		needsReview = true
	}

	paidAt := wd.TransactionAt
	if paidAt.IsZero() {
		paidAt = now
	}
	ev := PaidEvent{
		PaymentID: p.ID, CheckoutID: p.CheckoutID, OrderCode: wd.OrderCode, AmountPaid: wd.Amount,
		NeedsReview: needsReview, Reference: reference, PaidAt: paidAt, Raw: wd.Raw,
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

// awaitStatus tracks a transfer the provider's status does not show yet. The
// first sighting stores the tracking row (its own short statement) and
// reports false; a later one reports true once the row is StatusLagLimit old.
func (s *Service) awaitStatus(ctx context.Context, q *store.Queries, key string, paymentID uuid.UUID, body []byte, now time.Time) (bool, error) {
	trackKey := key + ":awaiting"
	since, err := q.GetPaymentEventReceivedAt(ctx, trackKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, s.storeEvent(ctx, q, trackKey, &paymentID, body, ResultAwaitingStatus, now)
	}
	if err != nil {
		return false, fmt.Errorf("get awaiting event: %w", err)
	}
	return now.Sub(since) >= StatusLagLimit, nil
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
