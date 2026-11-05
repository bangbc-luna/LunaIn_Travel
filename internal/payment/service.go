package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bangbc-luna/lunain-travel/internal/payment/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// Payment statuses (payments.status).
const (
	StatusPending   = "pending"
	StatusPaid      = "paid"
	StatusExpired   = "expired"
	StatusCancelled = "cancelled"
	// StatusNeedsReview: money arrived but did not match (spec §8).
	StatusNeedsReview = "needs_review"
)

var (
	// ErrPaymentNotFound: no pending payment for the checkout / unknown id.
	ErrPaymentNotFound = httpx.NewError(http.StatusNotFound, "not_found", "Không tìm thấy thanh toán")
	// ErrPaymentNotPending: the payment is no longer waiting for money.
	ErrPaymentNotPending = httpx.NewError(http.StatusConflict, "payment_not_pending", "Thanh toán này không còn ở trạng thái chờ")
	// ErrPaymentExpired: the payment deadline passed; no new link is created.
	ErrPaymentExpired = httpx.NewError(http.StatusConflict, "payment_expired", "Đã hết thời hạn thanh toán")
	// ErrProviderUnavailable wraps a failed CreateLink call.
	ErrProviderUnavailable = httpx.NewError(http.StatusBadGateway, "payment_provider_unavailable", "Cổng thanh toán đang gián đoạn, vui lòng thử lại sau ít phút")
	// ErrPaymentProcessing: the provider already reports the link paid; the
	// webhook (or a retry of it) confirms the booking, no new link is made.
	ErrPaymentProcessing = httpx.NewError(http.StatusConflict, "payment_processing", "Thanh toán của bạn đã được ghi nhận và đang chờ xác nhận, vui lòng đợi trong giây lát")
)

// Payment is the customer-facing state of one payment.
type Payment struct {
	ID          uuid.UUID `json:"id"`
	CheckoutID  uuid.UUID `json:"-"`
	OrderCode   int64     `json:"order_code"`
	Amount      int64     `json:"amount"`
	Status      string    `json:"status"`
	CheckoutURL *string   `json:"checkout_url"`
	QRCode      *string   `json:"qr_code"`
	ExpiresAt   time.Time `json:"expires_at"`
	// ProviderReference is the bank reference of the first transfer received.
	ProviderReference *string    `json:"-"`
	PaidAt            *time.Time `json:"-"`
}

func fromRow(r store.Payment) Payment {
	return Payment{
		ID: r.ID, CheckoutID: r.CheckoutID, OrderCode: r.ProviderOrderCode, Amount: r.Amount,
		Status: r.Status, CheckoutURL: r.CheckoutUrl, QRCode: r.QrCode, ExpiresAt: r.ExpiresAt,
		ProviderReference: r.ProviderReference, PaidAt: r.PaidAt,
	}
}

// Service orchestrates payments. The provider (PayOS) is only ever called
// outside a database transaction.
type Service struct {
	pool     *pgxpool.Pool
	clock    clock.Clock
	provider Provider
	log      *slog.Logger
}

// NewService builds a Service. log may be nil (slog.Default).
func NewService(pool *pgxpool.Pool, c clock.Clock, provider Provider, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, clock: c, provider: provider, log: log}
}

// ReservePayment inserts a pending payment of amount for checkoutID inside
// the caller's tx (which already holds the checkout/booking locks: payments
// come last in the lock order). The PayOS order code comes from the
// payment_order_code_seq column default. No provider call is made here:
// call ActivateLink after the tx committed.
func (s *Service) ReservePayment(ctx context.Context, tx pgx.Tx, checkoutID uuid.UUID, amount int64, expiresAt time.Time) (Payment, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Payment{}, fmt.Errorf("payment id: %w", err)
	}
	r, err := store.New(tx).InsertPendingPayment(ctx, store.InsertPendingPaymentParams{
		ID: id, CheckoutID: checkoutID, Amount: amount, ExpiresAt: expiresAt, Now: s.clock.Now(),
	})
	if err != nil {
		return Payment{}, fmt.Errorf("insert payment: %w", err)
	}
	return fromRow(r), nil
}

// ActivateLink creates the provider payment link of a pending payment and
// stores its checkout URL and QR code. It must not run inside a DB tx. It is
// idempotent: a payment that already has a URL is returned as is.
//
// A failed CreateLink may still have created the link (its response was
// lost), and the provider never accepts an order code twice, so the order
// code is checked with GetStatus (Ruling 8):
//   - PAID: ErrPaymentProcessing; the webhook confirms the booking.
//   - PENDING: the link is cancelled at the provider first.
//   - CANCELLED, EXPIRED or unknown to the provider: nothing to cancel.
//
// Then, in one short tx (checkout -> bookings -> payment), the payment is
// marked cancelled and a fresh pending payment with a new order code and the
// same expires_at replaces it; after commit its link is created. Money that
// later arrives on the cancelled order code is refunded by the webhook.
// Any provider failure on the way is ErrProviderUnavailable; the caller may
// retry.
func (s *Service) ActivateLink(ctx context.Context, paymentID uuid.UUID) (Payment, error) {
	q := store.New(s.pool)
	row, err := q.GetPaymentWithCheckoutCode(ctx, paymentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrPaymentNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("get payment: %w", err)
	}
	p := fromRow(row.Payment)
	if p.Status != StatusPending {
		return p, ErrPaymentNotPending
	}
	if p.CheckoutURL != nil {
		return p, nil
	}
	if !s.clock.Now().Before(p.ExpiresAt) {
		return p, ErrPaymentExpired
	}
	desc, err := DescriptionForCheckout(row.CheckoutCode)
	if err != nil {
		return p, err
	}
	link, err := s.provider.CreateLink(ctx, LinkRequest{
		OrderCode: p.OrderCode, Amount: p.Amount, Description: desc, ExpiresAt: p.ExpiresAt,
	})
	if err == nil {
		return s.storeLink(ctx, p, link)
	}
	s.log.Warn("payment: create link failed", "payment_id", p.ID, "order_code", p.OrderCode, "err", err)
	// A concurrent ActivateLink may have stored the link meanwhile.
	if cur, gerr := q.GetPayment(ctx, paymentID); gerr == nil && cur.CheckoutUrl != nil {
		return fromRow(cur), nil
	}
	if errors.Is(err, ErrInvalidDescription) || errors.Is(err, ErrDescriptionTooLong) {
		return p, err
	}
	fresh, err := s.replaceLostLink(ctx, p, err)
	if err != nil {
		return p, err
	}
	if fresh.CheckoutURL != nil {
		return fresh, nil
	}
	link, err = s.provider.CreateLink(ctx, LinkRequest{
		OrderCode: fresh.OrderCode, Amount: fresh.Amount, Description: desc, ExpiresAt: fresh.ExpiresAt,
	})
	if err != nil {
		s.log.Warn("payment: create link failed", "payment_id", fresh.ID, "order_code", fresh.OrderCode, "err", err)
		return fresh, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	return s.storeLink(ctx, fresh, link)
}

// replaceLostLink settles the provider-side state of p's order code after a
// failed CreateLink and replaces p by a fresh pending payment (see
// ActivateLink). It returns the fresh payment, or the current pending one if
// a concurrent call already replaced p.
func (s *Service) replaceLostLink(ctx context.Context, p Payment, createErr error) (Payment, error) {
	unavailable := func(err error) error { return fmt.Errorf("%w: %v", ErrProviderUnavailable, err) }
	st, err := s.provider.GetStatus(ctx, p.OrderCode)
	var pe *ProviderError
	switch {
	case err == nil && st.State == StatePaid:
		return p, ErrPaymentProcessing
	case err == nil && st.State == StatePending:
		if err := s.provider.CancelLink(ctx, p.OrderCode); err != nil {
			s.log.Warn("payment: cancel lost link failed", "payment_id", p.ID, "order_code", p.OrderCode, "err", err)
			return p, unavailable(err)
		}
	case err == nil:
		// CANCELLED, EXPIRED: the order code can no longer be paid.
	case errors.As(err, &pe):
		// Unknown to the provider: the link was never created.
	default:
		// The provider is unreachable: we cannot tell whether the link exists.
		return p, unavailable(createErr)
	}

	var fresh Payment
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.LockCheckoutRow(ctx, p.CheckoutID); err != nil {
			return fmt.Errorf("lock checkout: %w", err)
		}
		if _, err := q.LockCheckoutBookingRows(ctx, p.CheckoutID); err != nil {
			return fmt.Errorf("lock bookings: %w", err)
		}
		cur, err := q.LockPayment(ctx, p.ID)
		if err != nil {
			return fmt.Errorf("lock payment: %w", err)
		}
		if cur.Status != StatusPending {
			// Replaced by a concurrent call, paid or expired meanwhile.
			next, err := q.GetPendingPaymentByCheckout(ctx, p.CheckoutID)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrPaymentNotPending
			}
			if err != nil {
				return fmt.Errorf("get pending payment: %w", err)
			}
			fresh = fromRow(next)
			return nil
		}
		if err := q.CancelPayment(ctx, store.CancelPaymentParams{Now: s.clock.Now(), ID: cur.ID}); err != nil {
			return fmt.Errorf("cancel payment: %w", err)
		}
		fresh, err = s.ReservePayment(ctx, tx, cur.CheckoutID, cur.Amount, cur.ExpiresAt)
		return err
	})
	if err != nil {
		return p, err
	}
	s.log.Info("payment: order code replaced after a failed link creation",
		"old_order_code", p.OrderCode, "new_order_code", fresh.OrderCode)
	return fresh, nil
}

// storeLink saves a created link on p (only while it is pending without
// one) and returns the stored row.
func (s *Service) storeLink(ctx context.Context, p Payment, link Link) (Payment, error) {
	q := store.New(s.pool)
	var qr *string
	if link.QRCode != "" {
		qr = &link.QRCode
	}
	if _, err := q.SetPaymentLink(ctx, store.SetPaymentLinkParams{
		CheckoutUrl: &link.CheckoutURL, QrCode: qr, Now: s.clock.Now(), ID: p.ID,
	}); err != nil {
		return p, fmt.Errorf("store payment link: %w", err)
	}
	cur, err := q.GetPayment(ctx, p.ID)
	if err != nil {
		return p, fmt.Errorf("get payment: %w", err)
	}
	return fromRow(cur), nil
}

// PendingForCheckout returns the pending payment of a checkout, or nil.
func (s *Service) PendingForCheckout(ctx context.Context, checkoutID uuid.UUID) (*Payment, error) {
	r, err := store.New(s.pool).GetPendingPaymentByCheckout(ctx, checkoutID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pending payment: %w", err)
	}
	p := fromRow(r)
	return &p, nil
}

// StartPayment makes sure the pending payment of a checkout has a payment
// link (creating it at the provider if needed, see ActivateLink) and returns
// it. Outside any DB tx. No pending payment is ErrPaymentNotFound.
func (s *Service) StartPayment(ctx context.Context, checkoutID uuid.UUID) (Payment, error) {
	p, err := s.PendingForCheckout(ctx, checkoutID)
	if err != nil {
		return Payment{}, err
	}
	if p == nil {
		return Payment{}, ErrPaymentNotFound
	}
	return s.ActivateLink(ctx, p.ID)
}

// GetByOrderCode returns the payment of a provider order code, whatever its
// status (cancelled rows included); unknown is ErrPaymentNotFound.
func (s *Service) GetByOrderCode(ctx context.Context, orderCode int64) (Payment, error) {
	r, err := store.New(s.pool).GetPaymentByOrderCode(ctx, orderCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrPaymentNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("get payment by order code: %w", err)
	}
	return fromRow(r), nil
}

// LockPayment locks a payment FOR UPDATE in the caller's tx; the caller
// already holds its checkout and bookings (payments come after them).
func (s *Service) LockPayment(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Payment, error) {
	r, err := store.New(tx).LockPayment(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrPaymentNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("lock payment: %w", err)
	}
	return fromRow(r), nil
}

// RecordReceived stores the first transfer received on a locked payment:
// status (paid or needs_review), its bank reference, when it was paid and
// the raw webhook data.
func (s *Service) RecordReceived(ctx context.Context, tx pgx.Tx, id uuid.UUID, status, reference string, paidAt time.Time, raw []byte) error {
	if status != StatusPaid && status != StatusNeedsReview {
		return fmt.Errorf("payment: cannot record money as %q", status)
	}
	if err := store.New(tx).RecordPaymentReceived(ctx, store.RecordPaymentReceivedParams{
		Status: status, PaidAt: paidAt, ProviderReference: reference, Raw: raw, Now: s.clock.Now(), ID: id,
	}); err != nil {
		return fmt.Errorf("record payment received: %w", err)
	}
	return nil
}
