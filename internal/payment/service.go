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
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// Payment statuses (payments.status).
const (
	StatusPending = "pending"
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
}

func fromRow(r store.Payment) Payment {
	return Payment{
		ID: r.ID, CheckoutID: r.CheckoutID, OrderCode: r.ProviderOrderCode, Amount: r.Amount,
		Status: r.Status, CheckoutURL: r.CheckoutUrl, QRCode: r.QrCode, ExpiresAt: r.ExpiresAt,
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
// idempotent: a payment that already has a URL is returned as is. On a
// provider failure the row stays pending without URL and the error matches
// ErrProviderUnavailable; the caller may retry.
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
	if err != nil {
		s.log.Warn("payment: create link failed", "payment_id", p.ID, "order_code", p.OrderCode, "err", err)
		// A concurrent ActivateLink may have stored the link meanwhile.
		if cur, gerr := q.GetPayment(ctx, paymentID); gerr == nil && cur.CheckoutUrl != nil {
			return fromRow(cur), nil
		}
		return p, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	var qr *string
	if link.QRCode != "" {
		qr = &link.QRCode
	}
	if _, err := q.SetPaymentLink(ctx, store.SetPaymentLinkParams{
		CheckoutUrl: &link.CheckoutURL, QrCode: qr, Now: s.clock.Now(), ID: p.ID,
	}); err != nil {
		return p, fmt.Errorf("store payment link: %w", err)
	}
	cur, err := q.GetPayment(ctx, paymentID)
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
// link (creating it at the provider if needed) and returns it. Outside any
// DB tx. No pending payment is ErrPaymentNotFound.
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
