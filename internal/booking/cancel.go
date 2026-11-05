package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
	"github.com/bangbc-luna/lunain-travel/internal/platform/money"
)

// Admin cancellation modes (CancelByAdmin).
const (
	// CancelModeForceMajeure: an incident the customer did not cause; 100%
	// refund and the commission is waived (the partner ends at 0).
	CancelModeForceMajeure = "force_majeure"
	// CancelModePolicy: the cancellation policy applies as if the customer
	// cancelled; LunaIn keeps its commission.
	CancelModePolicy = "policy"

	cancelKindCustomer          = "customer"
	cancelKindAdminForceMajeure = "admin_force_majeure"
	cancelKindAdminPolicy       = "admin_policy"

	actorAdmin = "admin"

	auditActionCancel = "booking.cancel"
	auditKindBooking  = "booking"

	maxCancelReason = 500
)

var (
	// ErrInvalidBookingState: only a confirmed booking can be cancelled.
	ErrInvalidBookingState = httpx.NewError(http.StatusConflict, "invalid_booking_state", "Chỉ hủy được đơn đặt chỗ đã xác nhận")
	// ErrInvalidCancel: a bad admin cancellation request (mode or reason).
	ErrInvalidCancel = httpx.NewError(http.StatusUnprocessableEntity, "invalid_cancel", "Yêu cầu hủy không hợp lệ")
)

// BankAccount is the customer's bank account a refund is sent to.
type BankAccount = payment.BankAccount

// errServiceEnded: the (current, possibly rescheduled) service already
// ended; the no-show path owns the booking (Ruling 21).
var errServiceEnded = httpx.NewError(ErrInvalidBookingState.Status, ErrInvalidBookingState.Code,
	"Đơn đặt chỗ đã qua thời gian sử dụng dịch vụ, không thể hủy")

// cancelBlock is the cancellation predicate shared by the commands and
// BookingView.can_cancel (nil: allowed). A confirmed booking (customer or
// Super Admin) can be cancelled until its current service_end_at; the refund
// then depends on the time left before original_service_start_at (0% once
// it passed, e.g. after the original start but before a rescheduled visit).
func cancelBlock(b store.Booking, now time.Time) error {
	switch {
	case b.Status != StatusConfirmed:
		return ErrInvalidBookingState
	case !now.Before(b.ServiceEndAt):
		return errServiceEnded
	}
	return nil
}

func canCancel(b store.Booking, now time.Time) bool { return cancelBlock(b, now) == nil }

// cancellation describes one cancel command.
type cancellation struct {
	kind         string
	refundReason string
	actorKind    string
	actor        uuid.UUID
	reason       *string // history and cancel_reason (admin only)
	forceMajeure bool
	bank         *BankAccount // customer only: refund account, saved as default
}

// CancelByCustomer cancels the customer's own confirmed booking. Another
// customer's booking is ErrBookingNotFound; any other status is
// ErrInvalidBookingState. The refund follows the policy snapshot against
// original_service_start_at; with bank the refund is pending (and the account
// saved as the customer's default), without it is awaiting_bank_info unless
// an account was saved before.
func (s *Service) CancelByCustomer(ctx context.Context, customerID, bookingID uuid.UUID, bank *BankAccount) (BookingView, error) {
	err := s.cancel(ctx, bookingID, &customerID, cancellation{
		kind: cancelKindCustomer, refundReason: payment.RefundCustomerCancel,
		actorKind: actorCustomer, actor: customerID, bank: bank,
	})
	if err != nil {
		return BookingView{}, err
	}
	return s.GetMine(ctx, customerID, bookingID)
}

// CancelByAdmin cancels a confirmed booking as Super Admin. mode is
// force_majeure (100% refund and commission waiver) or policy (like a
// customer cancellation); reason (1..500 characters) is stored on the
// booking, its history and the audit log (booking.cancel, same tx).
func (s *Service) CancelByAdmin(ctx context.Context, actor, bookingID uuid.UUID, mode string, reason string) (BookingView, error) {
	c := cancellation{refundReason: payment.RefundAdminCancel, actorKind: actorAdmin, actor: actor}
	switch mode {
	case CancelModeForceMajeure:
		c.kind, c.forceMajeure = cancelKindAdminForceMajeure, true
	case CancelModePolicy:
		c.kind = cancelKindAdminPolicy
	default:
		return BookingView{}, httpx.NewError(ErrInvalidCancel.Status, ErrInvalidCancel.Code,
			"mode phải là force_majeure hoặc policy")
	}
	reason = strings.TrimSpace(reason)
	if n := utf8.RuneCountInString(reason); n < 1 || n > maxCancelReason || strings.ContainsFunc(reason, unicode.IsControl) {
		return BookingView{}, httpx.NewError(ErrInvalidCancel.Status, ErrInvalidCancel.Code,
			fmt.Sprintf("Lý do hủy phải có từ 1 đến %d ký tự", maxCancelReason))
	}
	c.reason = &reason
	if err := s.cancel(ctx, bookingID, nil, c); err != nil {
		return BookingView{}, err
	}
	row, err := s.detail(ctx, bookingID)
	if err != nil {
		return BookingView{}, err
	}
	return s.load(ctx, row, forAdmin)
}

// cancel runs one cancellation in a tx. owner, when set, must own the
// booking (else ErrBookingNotFound).
//
// Lock order (DB §1.1): the customer's refund account (a users child) is
// saved first, then the checkout and the booking are locked FOR UPDATE and
// the state re-checked; the refund row (FK to the payment) comes before the
// ledger postings, which lock partner_balances last. In one tx: booking
// cancelled + history, refund (if > 0) and ledger.PostRefund, the
// commission waiver (force majeure), ledger.Release at once (the booking's
// pending returns to 0), the audit row (admin) and the customer email.
func (s *Service) cancel(ctx context.Context, bookingID uuid.UUID, owner *uuid.UUID, c cancellation) error {
	b, err := store.New(s.pool).GetBooking(ctx, bookingID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != nil && b.CustomerID != *owner) {
		return ErrBookingNotFound
	}
	if err != nil {
		return fmt.Errorf("get booking: %w", err)
	}
	// Ownership (404) first, then the bank account (422), then the state (409).
	if c.bank != nil {
		acc, err := c.bank.Normalize()
		if err != nil {
			return err
		}
		c.bank = &acc
	}
	if err := cancelBlock(b, s.clock.Now()); err != nil {
		return err
	}
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if c.bank != nil {
			if _, err := s.payments.SaveRefundAccountTx(ctx, tx, b.CustomerID, *c.bank); err != nil {
				return err
			}
		}
		co, err := q.LockCheckout(ctx, b.CheckoutID)
		if err != nil {
			return fmt.Errorf("lock checkout: %w", err)
		}
		b, err := q.LockBooking(ctx, bookingID)
		if err != nil {
			return fmt.Errorf("lock booking: %w", err)
		}
		now := s.clock.Now()
		if err := cancelBlock(b, now); err != nil {
			return err
		}
		refund := b.GrossAmount
		if !c.forceMajeure {
			if refund, err = policyRefund(b, now); err != nil {
				return err
			}
		}

		if err := q.CancelBooking(ctx, store.CancelBookingParams{
			Now: now, CancelKind: c.kind, CancelReason: c.reason, CancelledBy: &c.actor, ID: b.ID,
		}); err != nil {
			return fmt.Errorf("cancel booking: %w", err)
		}
		if err := insertHistory(ctx, q, b, StatusCancelled, c.actorKind, &c.actor, c.reason, now); err != nil {
			return err
		}
		var r *payment.Refund
		if refund > 0 {
			// Refunded against the payment that confirmed the checkout.
			if co.PaidPaymentID == nil {
				return fmt.Errorf("booking: confirmed booking %s has no paid checkout", b.ID)
			}
			id := b.ID
			created, err := s.payments.CreateRefund(ctx, tx, payment.RefundInput{
				PaymentID: *co.PaidPaymentID, CheckoutID: co.ID, CustomerID: co.CustomerID,
				BookingID: &id, Amount: refund, Reason: c.refundReason,
			})
			if err != nil {
				return err
			}
			r = &created
		}
		if err := s.ledger.PostRefund(ctx, tx, b.ID, b.PartnerID, refund); err != nil {
			return fmt.Errorf("post refund: %w", err)
		}
		if c.forceMajeure {
			if err := s.ledger.PostCommissionWaiver(ctx, tx, b.ID, b.PartnerID, b.ServiceType, b.CommissionAmount); err != nil {
				return fmt.Errorf("post commission waiver: %w", err)
			}
		}
		if err := s.ledger.Release(ctx, tx, b.ID); err != nil {
			return fmt.Errorf("release: %w", err)
		}
		if c.actorKind == actorAdmin {
			id := b.ID
			if err := audit.Record(ctx, tx, now, audit.Entry{
				ActorUserID: c.actor, Action: auditActionCancel, TargetKind: auditKindBooking, TargetID: &id,
				Before: map[string]any{"status": b.Status},
				After:  map[string]any{"status": StatusCancelled, "cancel_kind": c.kind, "refund_amount": refund},
				Reason: *c.reason,
			}); err != nil {
				return err
			}
		}
		return s.enqueueEmail(ctx, tx, cancelEmail(b, r))
	})
}

// policyRefund is MulBps(gross, RefundBps(snapshot, original_service_start_at
// − now)): a rescheduled booking is refunded against its original start
// (Review Focus 3).
func policyRefund(b store.Booking, now time.Time) (int64, error) {
	var tiers []listing.CancellationTier
	if err := json.Unmarshal(b.CancellationPolicySnapshot, &tiers); err != nil {
		return 0, fmt.Errorf("decode policy snapshot of booking %s: %w", b.ID, err)
	}
	return money.MulBps(b.GrossAmount, listing.RefundBps(tiers, b.OriginalServiceStartAt.Sub(now))), nil
}

// cancelEmail tells the customer the booking was cancelled and what is
// refunded (r nil: nothing).
func cancelEmail(b store.Booking, r *payment.Refund) mailer.Message {
	text := "Xin chào " + b.ContactName + ",\n\n" +
		"Đơn đặt chỗ " + b.Code + " (sử dụng lúc " + b.ServiceStartAt.In(clock.VN).Format("15:04 ngày 02/01/2006") +
		") đã được hủy.\n"
	switch {
	case r == nil:
		text += "Theo chính sách hủy của đơn, khoản thanh toán không được hoàn lại.\n"
	case r.Status == payment.RefundAwaitingBankInfo:
		text += "Số tiền sẽ được hoàn lại: " + formatVND(r.Amount) + ".\n" +
			"Vui lòng cập nhật tài khoản ngân hàng nhận hoàn tiền trong tài khoản LunaIn của bạn.\n"
	default:
		text += "Số tiền sẽ được hoàn lại: " + formatVND(r.Amount) + ".\n" +
			"Tiền sẽ được chuyển vào tài khoản ngân hàng bạn đã cung cấp.\n"
	}
	return mailer.Message{To: b.ContactEmail, Subject: "Đơn đặt chỗ " + b.Code + " đã được hủy", Text: text}
}
