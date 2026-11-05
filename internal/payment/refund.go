package payment

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/payment/store"
)

// Refund reasons (refunds.reason).
const (
	RefundCustomerCancel   = "customer_cancel"
	RefundAdminCancel      = "admin_cancel"
	RefundLatePayment      = "late_payment"
	RefundPaymentMismatch  = "payment_mismatch"
	RefundDuplicatePayment = "duplicate_payment"
)

// Refund statuses (refunds.status).
const (
	RefundAwaitingBankInfo = "awaiting_bank_info"
	RefundPending          = "pending"
	RefundCompleted        = "completed"
)

// RefundInput describes a refund to create. A booking refund sets BookingID;
// a payment-level refund (over-, under- or duplicate payment) leaves it nil
// and sets ProviderReference, the bank reference of the transfer it returns.
type RefundInput struct {
	PaymentID, CheckoutID uuid.UUID
	// CustomerID owns the checkout; their saved refund account is used.
	CustomerID        uuid.UUID
	BookingID         *uuid.UUID
	ProviderReference *string
	Amount            int64
	Reason            string
}

// Refund is a created refund.
type Refund struct {
	ID                uuid.UUID  `json:"id"`
	PaymentID         uuid.UUID  `json:"payment_id"`
	BookingID         *uuid.UUID `json:"booking_id"`
	Amount            int64      `json:"amount"`
	Reason            string     `json:"reason"`
	Status            string     `json:"status"`
	ProviderReference *string    `json:"-"`
}

// CreateRefund inserts a refund inside the caller's tx (which holds the
// checkout, its bookings and the payment). It starts awaiting_bank_info, or
// pending with the bank account prefilled when the customer saved one.
func (s *Service) CreateRefund(ctx context.Context, tx pgx.Tx, in RefundInput) (Refund, error) {
	if in.Amount <= 0 {
		return Refund{}, fmt.Errorf("payment: refund amount %d", in.Amount)
	}
	if in.BookingID == nil && (in.ProviderReference == nil || *in.ProviderReference == "") {
		return Refund{}, errors.New("payment: a payment-level refund needs the transfer reference")
	}
	q := store.New(tx)
	id, err := uuid.NewV7()
	if err != nil {
		return Refund{}, fmt.Errorf("refund id: %w", err)
	}
	p := store.InsertRefundParams{
		ID: id, PaymentID: in.PaymentID, CheckoutID: in.CheckoutID, BookingID: in.BookingID,
		ProviderReference: in.ProviderReference, Amount: in.Amount, Reason: in.Reason,
		Status: RefundAwaitingBankInfo, Now: s.clock.Now(),
	}
	acc, err := q.GetRefundAccount(ctx, in.CustomerID)
	switch {
	case err == nil:
		p.Status = RefundPending
		p.BankBin, p.AccountNumber, p.AccountHolder = &acc.BankBin, &acc.AccountNumber, &acc.AccountHolder
	case !errors.Is(err, pgx.ErrNoRows):
		return Refund{}, fmt.Errorf("get refund account: %w", err)
	}
	r, err := q.InsertRefund(ctx, p)
	if err != nil {
		return Refund{}, fmt.Errorf("insert refund: %w", err)
	}
	return Refund{
		ID: r.ID, PaymentID: r.PaymentID, BookingID: r.BookingID, Amount: r.Amount,
		Reason: r.Reason, Status: r.Status, ProviderReference: r.ProviderReference,
	}, nil
}
