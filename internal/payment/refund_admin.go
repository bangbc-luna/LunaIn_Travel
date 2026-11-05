package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/payment/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100

	maxTransferReferenceRunes = 100
	refundProofPurpose        = "refund_proof"

	// Bank info reminders (user decision): every 3 days at most, at most 5
	// times, at most remindBatch refunds per run.
	remindEvery = 72 * time.Hour
	// remindSlack: the daily job does not start at the same second every
	// day; a run up to this much earlier than remindEvery after the
	// previous reminder still sends the next one (instead of on day 4).
	remindSlack  = time.Hour
	maxReminders = 5
	remindBatch  = 200
)

// API errors of the Super Admin refund use cases.
var (
	ErrTransferReferenceInvalid = httpx.NewError(http.StatusUnprocessableEntity, "invalid_transfer_reference",
		"Vui lòng nhập mã giao dịch chuyển khoản (tối đa 100 ký tự)")
	ErrRefundAwaitingBankInfo = httpx.NewError(http.StatusConflict, "refund_awaiting_bank_info",
		"Khoản hoàn tiền này chưa có tài khoản nhận của khách")
	ErrRefundAlreadyCompleted = httpx.NewError(http.StatusConflict, "refund_already_completed",
		"Khoản hoàn tiền này đã được đánh dấu chuyển khoản")
)

var (
	refundStatuses = []string{RefundAwaitingBankInfo, RefundPending, RefundCompleted}
	refundReasons  = []string{
		RefundCustomerCancel, RefundAdminCancel, RefundLatePayment, RefundPaymentMismatch, RefundDuplicatePayment,
	}
)

// RefundFilter pages the Super Admin refund list (newest first); every
// filter is optional.
type RefundFilter struct {
	Status, Reason       *string
	BookingID, PaymentID *uuid.UUID
	Limit                int
	Cursor               *uuid.UUID
}

// CompleteRefundInput is what the Super Admin records after transferring a
// refund by hand.
type CompleteRefundInput struct {
	TransferReference string
	// ProofFileID is an optional refund_proof file uploaded by the admin.
	ProofFileID *uuid.UUID
}

// validText trims s and checks it has 1..max runes of valid UTF-8 without
// NUL (Postgres text cannot store it).
func validText(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	return s, n >= 1 && n <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// pageLimit applies the default and maximum page size.
func pageLimit(n int) int {
	if n <= 0 {
		return defaultPageSize
	}
	return min(n, maxPageSize)
}

// AdminListRefunds pages every refund, newest first, and returns the cursor
// of the next page (nil on the last). An unknown status or reason is
// httpx.ErrInvalidRequest.
func (s *Service) AdminListRefunds(ctx context.Context, f RefundFilter) ([]Refund, *uuid.UUID, error) {
	if (f.Status != nil && !slices.Contains(refundStatuses, *f.Status)) ||
		(f.Reason != nil && !slices.Contains(refundReasons, *f.Reason)) {
		return nil, nil, httpx.ErrInvalidRequest
	}
	limit := pageLimit(f.Limit)
	rows, err := store.New(s.pool).ListAdminRefunds(ctx, store.ListAdminRefundsParams{
		Status: f.Status, Reason: f.Reason, BookingID: f.BookingID, PaymentID: f.PaymentID,
		Cursor: f.Cursor, Lim: int32(limit + 1),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list refunds: %w", err)
	}
	var next *uuid.UUID
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1].ID
		next = &last
	}
	out := make([]Refund, len(rows))
	for i, r := range rows {
		out[i] = refundFromRow(r)
	}
	return out, next, nil
}

// AdminGetRefund returns any refund; unknown is ErrRefundNotFound.
func (s *Service) AdminGetRefund(ctx context.Context, id uuid.UUID) (Refund, error) {
	r, err := store.New(s.pool).GetRefund(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, ErrRefundNotFound
	}
	if err != nil {
		return Refund{}, fmt.Errorf("get refund: %w", err)
	}
	return refundFromRow(r), nil
}

// lockRefund takes only the refunds row FOR UPDATE; unknown is
// ErrRefundNotFound.
func lockRefund(ctx context.Context, q *store.Queries, id uuid.UUID) (store.Refund, error) {
	r, err := q.LockRefund(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrRefundNotFound
	}
	if err != nil {
		return r, fmt.Errorf("lock refund: %w", err)
	}
	return r, nil
}

// CompleteRefund records that a pending refund was transferred by hand: the
// trimmed transfer reference (1..100 runes, else
// ErrTransferReferenceInvalid) and an optional refund_proof file uploaded by
// the actor (else files.ErrInvalidFile). In one tx it locks only the refund
// (awaiting_bank_info is ErrRefundAwaitingBankInfo, completed
// ErrRefundAlreadyCompleted), completes it, audits refund.complete and emails
// the customer's account address. No ledger entry: the refund's money left
// the partner's balance when the refund was decided.
func (s *Service) CompleteRefund(ctx context.Context, actor, id uuid.UUID, in CompleteRefundInput) (Refund, error) {
	ref, ok := validText(in.TransferReference, maxTransferReferenceRunes)
	if !ok {
		return Refund{}, ErrTransferReferenceInvalid
	}
	now := s.clock.Now()
	var out Refund
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := lockRefund(ctx, q, id)
		if err != nil {
			return err
		}
		switch cur.Status {
		case RefundAwaitingBankInfo:
			return ErrRefundAwaitingBankInfo
		case RefundCompleted:
			return ErrRefundAlreadyCompleted
		}
		if in.ProofFileID != nil {
			if s.ops.Files == nil {
				return errors.New("payment: no file checker configured (SetOps)")
			}
			if err := s.ops.Files.Owned(ctx, tx, *in.ProofFileID, actor, refundProofPurpose); err != nil {
				return err
			}
		}
		row, err := q.CompleteRefund(ctx, store.CompleteRefundParams{
			TransferReference: ref, ProofFileID: in.ProofFileID, Actor: actor, Now: now, ID: id,
		})
		if err != nil {
			return fmt.Errorf("complete refund: %w", err)
		}
		out = refundFromRow(row)
		if err := audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor, Action: "refund.complete", TargetKind: "refund", TargetID: &id,
			Before: map[string]any{"status": cur.Status},
			After: map[string]any{
				"status": out.Status, "amount": out.Amount, "reason": out.Reason, "payment_id": out.PaymentID,
				"transfer_reference": ref, "proof_file_id": in.ProofFileID,
			},
		}); err != nil {
			return err
		}
		text := "Xin chào,\n\n" +
			"Khoản hoàn " + formatAmount(out.Amount) + "đ đã được chuyển, mã giao dịch " + ref + ".\n"
		if out.BankBIN != nil && out.AccountNumber != nil {
			text += "Tài khoản nhận: ngân hàng " + *out.BankBIN + " - số tài khoản " + *out.AccountNumber + ".\n"
		}
		return s.emailCustomer(ctx, tx, row.CheckoutID, mailer.Message{
			Subject: "LunaIn đã chuyển khoản hoàn tiền " + formatAmount(out.Amount) + "đ", Text: text,
		})
	})
	return out, err
}

// RemindAwaitingBankInfo emails the customer of every refund still
// awaiting_bank_info, created at least 3 days ago and last reminded at least
// 3 days ago (or never), reminded fewer than 5 times — "3 days" tolerating a
// job start up to remindSlack early; at most 200 refunds
// per run (oldest first). Each refund is handled in its own tx: locked, the
// conditions checked again, the email queued, its reminder count and time
// updated. A failing refund is logged and skipped (the next run retries).
// It returns how many reminders were queued.
func (s *Service) RemindAwaitingBankInfo(ctx context.Context) (int, error) {
	now := s.clock.Now()
	cutoff := now.Add(-remindEvery + remindSlack)
	ids, err := store.New(s.pool).ListRefundsToRemind(ctx, store.ListRefundsToRemindParams{
		Cutoff: cutoff, MaxCount: maxReminders, Lim: remindBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list refunds to remind: %w", err)
	}
	n := 0
	for _, id := range ids {
		sent := false
		err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			q := store.New(tx)
			r, err := lockRefund(ctx, q, id)
			if err != nil {
				return err
			}
			if r.Status != RefundAwaitingBankInfo || r.CreatedAt.After(cutoff) ||
				(r.BankInfoRemindedAt != nil && r.BankInfoRemindedAt.After(cutoff)) ||
				r.BankInfoReminderCount >= maxReminders {
				return nil // changed since the unlocked read
			}
			if err := s.emailCustomer(ctx, tx, r.CheckoutID, mailer.Message{
				Subject: "Nhắc nhở: cập nhật tài khoản nhận hoàn tiền",
				Text: "Xin chào,\n\n" +
					"LunaIn đang giữ khoản hoàn " + formatAmount(r.Amount) + "đ cho bạn nhưng chưa có tài khoản ngân hàng nhận tiền.\n" +
					"Vui lòng cập nhật tài khoản ngân hàng nhận hoàn tiền trong tài khoản LunaIn của bạn.\n",
			}); err != nil {
				return err
			}
			if err := q.MarkRefundReminded(ctx, store.MarkRefundRemindedParams{Now: now, ID: id}); err != nil {
				return fmt.Errorf("mark refund reminded: %w", err)
			}
			sent = true
			return nil
		})
		if err != nil {
			s.log.Error("payment: refund bank info reminder failed", "refund_id", id, "err", err)
			continue
		}
		if sent {
			n++
		}
	}
	return n, nil
}

// emailCustomer queues m, in tx, to the account email of the checkout's
// customer. Without a job client (SetOps not called) it is only logged.
func (s *Service) emailCustomer(ctx context.Context, tx pgx.Tx, checkoutID uuid.UUID, m mailer.Message) error {
	if s.ops.Jobs == nil || s.ops.Users == nil {
		s.log.Info("payment: email not queued (no job client)", "checkout_id", checkoutID, "subject", m.Subject)
		return nil
	}
	customerID, err := store.New(tx).GetCheckoutCustomer(ctx, checkoutID)
	if err != nil {
		return fmt.Errorf("get checkout customer: %w", err)
	}
	if m.To, err = s.ops.Users.EmailOf(ctx, tx, customerID); err != nil {
		return err
	}
	if err := mailer.EnqueueTx(ctx, tx, s.ops.Jobs, m); err != nil {
		return fmt.Errorf("enqueue email: %w", err)
	}
	return nil
}

// formatAmount writes 380000 as "380.000".
func formatAmount(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(ch)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
