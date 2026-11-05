package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/payment/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/validate"
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

// Refund is one refund to a customer.
type Refund struct {
	ID                uuid.UUID  `json:"id"`
	PaymentID         uuid.UUID  `json:"payment_id"`
	BookingID         *uuid.UUID `json:"booking_id"`
	Amount            int64      `json:"amount"`
	Reason            string     `json:"reason"`
	Status            string     `json:"status"`
	BankBIN           *string    `json:"bank_bin"`
	AccountNumber     *string    `json:"account_number"`
	AccountHolder     *string    `json:"account_holder"`
	TransferReference *string    `json:"transfer_reference"`
	CompletedAt       *time.Time `json:"completed_at"`
	CreatedAt         time.Time  `json:"created_at"`
	ProviderReference *string    `json:"-"`
}

func refundFromRow(r store.Refund) Refund {
	return Refund{
		ID: r.ID, PaymentID: r.PaymentID, BookingID: r.BookingID, Amount: r.Amount,
		Reason: r.Reason, Status: r.Status, BankBIN: r.BankBin, AccountNumber: r.AccountNumber,
		AccountHolder: r.AccountHolder, TransferReference: r.TransferReference,
		CompletedAt: r.CompletedAt, CreatedAt: r.CreatedAt, ProviderReference: r.ProviderReference,
	}
}

var (
	// ErrInvalidBankAccount: a refund bank account breaks the bank account
	// rules (BIN 6 digits, account 6–19 digits, holder name).
	ErrInvalidBankAccount = httpx.NewError(http.StatusUnprocessableEntity, "invalid_bank_account", "Tài khoản ngân hàng không hợp lệ")
	// ErrRefundNotFound: unknown refund, or one of another customer.
	ErrRefundNotFound = httpx.NewError(http.StatusNotFound, "not_found", "Không tìm thấy khoản hoàn tiền")
	// ErrRefundNotAwaitingBankInfo: the refund already has its bank account.
	ErrRefundNotAwaitingBankInfo = httpx.NewError(http.StatusConflict, "refund_not_awaiting_bank_info", "Khoản hoàn tiền này đã có tài khoản nhận")
	// ErrRefundAccountNotFound: the customer saved no refund account.
	ErrRefundAccountNotFound = httpx.NewError(http.StatusNotFound, "not_found", "Bạn chưa lưu tài khoản nhận hoàn tiền")
)

// maxListedRefunds bounds GET /v1/refunds (newest first).
const maxListedRefunds = 200

// BankAccount is a customer's bank account for refunds.
type BankAccount struct {
	BankBIN       string `json:"bank_bin"`
	AccountNumber string `json:"account_number"`
	AccountHolder string `json:"account_holder"`
}

// Normalize applies the partner bank account rules (validate.NormalizeBankAccount);
// a bad account is ErrInvalidBankAccount naming the field.
func (b BankAccount) Normalize() (BankAccount, error) {
	acc, msg := validate.NormalizeBankAccount(b.BankBIN, b.AccountNumber, b.AccountHolder)
	if msg != "" {
		return BankAccount{}, httpx.NewError(ErrInvalidBankAccount.Status, ErrInvalidBankAccount.Code, msg)
	}
	return BankAccount{BankBIN: acc.BIN, AccountNumber: acc.Number, AccountHolder: acc.Holder}, nil
}

// SaveRefundAccountTx normalizes acc and saves it, inside tx, as the
// customer's default refund account (CreateRefund then prefills it).
func (s *Service) SaveRefundAccountTx(ctx context.Context, tx pgx.Tx, customerID uuid.UUID, acc BankAccount) (BankAccount, error) {
	acc, err := acc.Normalize()
	if err != nil {
		return BankAccount{}, err
	}
	r, err := store.New(tx).UpsertRefundAccount(ctx, store.UpsertRefundAccountParams{
		UserID: customerID, BankBin: acc.BankBIN, AccountNumber: acc.AccountNumber,
		AccountHolder: acc.AccountHolder, Now: s.clock.Now(),
	})
	if err != nil {
		return BankAccount{}, fmt.Errorf("upsert refund account: %w", err)
	}
	return BankAccount{BankBIN: r.BankBin, AccountNumber: r.AccountNumber, AccountHolder: r.AccountHolder}, nil
}

// SaveRefundAccount is SaveRefundAccountTx in its own tx
// (PUT /v1/customer/refund-account).
func (s *Service) SaveRefundAccount(ctx context.Context, customerID uuid.UUID, acc BankAccount) (BankAccount, error) {
	var out BankAccount
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		out, err = s.SaveRefundAccountTx(ctx, tx, customerID, acc)
		return err
	})
	return out, err
}

// RefundAccount returns the customer's saved refund account;
// none is ErrRefundAccountNotFound.
func (s *Service) RefundAccount(ctx context.Context, customerID uuid.UUID) (BankAccount, error) {
	r, err := store.New(s.pool).GetRefundAccount(ctx, customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return BankAccount{}, ErrRefundAccountNotFound
	}
	if err != nil {
		return BankAccount{}, fmt.Errorf("get refund account: %w", err)
	}
	return BankAccount{BankBIN: r.BankBin, AccountNumber: r.AccountNumber, AccountHolder: r.AccountHolder}, nil
}

// SetRefundBankAccount gives the bank account of one of the customer's own
// refunds (owned through its checkout) that is awaiting_bank_info; it becomes
// pending. Another customer's refund is ErrRefundNotFound, a refund that
// already has its account ErrRefundNotAwaitingBankInfo.
func (s *Service) SetRefundBankAccount(ctx context.Context, customerID, refundID uuid.UUID, bank BankAccount) error {
	acc, err := bank.Normalize()
	if err != nil {
		return err
	}
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		r, err := q.LockCustomerRefund(ctx, store.LockCustomerRefundParams{ID: refundID, CustomerID: customerID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRefundNotFound
		}
		if err != nil {
			return fmt.Errorf("lock refund: %w", err)
		}
		if r.Status != RefundAwaitingBankInfo {
			return ErrRefundNotAwaitingBankInfo
		}
		if err := q.SetRefundBankInfo(ctx, store.SetRefundBankInfoParams{
			BankBin: &acc.BankBIN, AccountNumber: &acc.AccountNumber, AccountHolder: &acc.AccountHolder,
			Now: s.clock.Now(), ID: r.ID,
		}); err != nil {
			return fmt.Errorf("set refund bank info: %w", err)
		}
		return nil
	})
}

// ListMine lists the customer's refunds (through their checkouts), newest
// first.
func (s *Service) ListMine(ctx context.Context, customerID uuid.UUID) ([]Refund, error) {
	rows, err := store.New(s.pool).ListCustomerRefunds(ctx, store.ListCustomerRefundsParams{
		CustomerID: customerID, Lim: maxListedRefunds,
	})
	if err != nil {
		return nil, fmt.Errorf("list refunds: %w", err)
	}
	out := make([]Refund, len(rows))
	for i, r := range rows {
		out[i] = refundFromRow(r)
	}
	return out, nil
}

// CapturedPayment returns, inside tx, the payment whose money confirmed the
// checkout (the paid one, else the latest needs_review one that received
// money): booking refunds are made against it.
func (s *Service) CapturedPayment(ctx context.Context, tx pgx.Tx, checkoutID uuid.UUID) (Payment, error) {
	r, err := store.New(tx).GetCapturedPayment(ctx, checkoutID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, fmt.Errorf("payment: checkout %s has no captured payment", checkoutID)
	}
	if err != nil {
		return Payment{}, fmt.Errorf("get captured payment: %w", err)
	}
	return fromRow(r), nil
}

// CheckoutHasBookingRefund reports whether any booking of the checkout was
// already refunded (a booking is refunded at most once).
func (s *Service) CheckoutHasBookingRefund(ctx context.Context, tx pgx.Tx, checkoutID uuid.UUID) (bool, error) {
	ok, err := store.New(tx).CheckoutHasBookingRefund(ctx, checkoutID)
	if err != nil {
		return false, fmt.Errorf("checkout booking refunds: %w", err)
	}
	return ok, nil
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
	return refundFromRow(r), nil
}
