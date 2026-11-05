package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/payment/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// Review states of a needs_review payment (Payment.ReviewState).
const (
	ReviewNew      = "new"
	ReviewSeen     = "seen"
	ReviewResolved = "resolved"

	maxReviewNoteRunes = 1000
)

// API errors of the payment review use cases.
var (
	ErrPaymentNotInReview = httpx.NewError(http.StatusConflict, "payment_not_in_review",
		"Giao dịch này không ở trạng thái cần xem xét hoặc đã được xử lý xong")
	ErrReviewNoteInvalid = httpx.NewError(http.StatusUnprocessableEntity, "invalid_review_note",
		"Ghi chú tối đa 1000 ký tự")
)

var paymentStatuses = []string{StatusPending, StatusPaid, StatusExpired, StatusCancelled, StatusNeedsReview}

// PaymentFilter pages the Super Admin payment list (newest first).
// OpenReview keeps only needs_review payments not resolved yet.
type PaymentFilter struct {
	Status     *string
	OpenReview bool
	CheckoutID *uuid.UUID
	Limit      int
	Cursor     *uuid.UUID
}

// EventSummary is one payment_events row of a payment.
type EventSummary struct {
	DedupeKey string `json:"dedupe_key"`
	Result    string `json:"result"`
	// Amount of the transfer processed (NULL for older or unprocessed rows).
	Amount     *int64    `json:"amount"`
	ReceivedAt time.Time `json:"received_at"`
}

// PaymentDetail is a payment as the Super Admin reviews it: its webhook
// events and its refunds.
type PaymentDetail struct {
	Payment
	Events           []EventSummary `json:"events"`
	Refunds          []Refund       `json:"refunds"`
	ReviewNote       *string        `json:"review_note"`
	ReviewResolvedAt *time.Time     `json:"review_resolved_at"`
}

// adminPayment fills ReviewState (only on needs_review payments).
func adminPayment(r store.Payment) Payment {
	p := fromRow(r)
	if p.Status == StatusNeedsReview {
		switch {
		case p.ReviewResolvedAt != nil:
			p.ReviewState = ReviewResolved
		case p.ReviewSeenAt != nil:
			p.ReviewState = ReviewSeen
		default:
			p.ReviewState = ReviewNew
		}
	}
	return p
}

// AdminListPayments pages every payment, newest first, and returns the
// cursor of the next page. An unknown status is httpx.ErrInvalidRequest.
func (s *Service) AdminListPayments(ctx context.Context, f PaymentFilter) ([]Payment, *uuid.UUID, error) {
	if f.Status != nil && !slices.Contains(paymentStatuses, *f.Status) {
		return nil, nil, httpx.ErrInvalidRequest
	}
	limit := pageLimit(f.Limit)
	rows, err := store.New(s.pool).ListAdminPayments(ctx, store.ListAdminPaymentsParams{
		Status: f.Status, OpenReview: f.OpenReview, CheckoutID: f.CheckoutID, Cursor: f.Cursor, Lim: int32(limit + 1),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list payments: %w", err)
	}
	var next *uuid.UUID
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1].ID
		next = &last
	}
	out := make([]Payment, len(rows))
	for i, r := range rows {
		out[i] = adminPayment(r)
	}
	return out, next, nil
}

// AdminGetPayment returns any payment with its events and refunds, read in
// one snapshot; unknown is ErrPaymentNotFound.
func (s *Service) AdminGetPayment(ctx context.Context, id uuid.UUID) (PaymentDetail, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return PaymentDetail{}, fmt.Errorf("begin payment detail tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	row, err := q.GetPayment(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentDetail{}, ErrPaymentNotFound
	}
	if err != nil {
		return PaymentDetail{}, fmt.Errorf("get payment: %w", err)
	}
	evs, err := q.ListPaymentEvents(ctx, &id)
	if err != nil {
		return PaymentDetail{}, fmt.Errorf("list payment events: %w", err)
	}
	refs, err := q.ListPaymentRefunds(ctx, id)
	if err != nil {
		return PaymentDetail{}, fmt.Errorf("list payment refunds: %w", err)
	}
	d := PaymentDetail{
		Payment: adminPayment(row), Events: make([]EventSummary, len(evs)), Refunds: make([]Refund, len(refs)),
		ReviewNote: row.ReviewNote, ReviewResolvedAt: row.ReviewResolvedAt,
	}
	for i, ev := range evs {
		d.Events[i] = EventSummary{DedupeKey: ev.DedupeKey, Amount: ev.Amount, ReceivedAt: ev.ReceivedAt}
		if ev.Result != nil {
			d.Events[i].Result = *ev.Result
		}
	}
	for i, r := range refs {
		d.Refunds[i] = refundFromRow(r)
	}
	return d, nil
}

// lockForReview locks, in tx, the payment's checkout then the payment (DB
// §1.1; bookings are not needed: nothing of them changes). Unknown is
// ErrPaymentNotFound.
func lockForReview(ctx context.Context, q *store.Queries, id uuid.UUID) (store.Payment, error) {
	cur, err := q.GetPayment(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return cur, ErrPaymentNotFound
	}
	if err != nil {
		return cur, fmt.Errorf("get payment: %w", err)
	}
	if _, err := q.LockCheckoutRow(ctx, cur.CheckoutID); err != nil {
		return cur, fmt.Errorf("lock checkout: %w", err)
	}
	if cur, err = q.LockPayment(ctx, id); err != nil {
		return cur, fmt.Errorf("lock payment: %w", err)
	}
	return cur, nil
}

// MarkReviewSeen records that a Super Admin opened a needs_review payment
// (the FE calls it when the detail opens). The first call sets
// review_seen_at/by and audits payment.review_seen; later calls change
// nothing (idempotent), a resolved review included. A payment that is not
// needs_review is ErrPaymentNotInReview.
func (s *Service) MarkReviewSeen(ctx context.Context, actor, id uuid.UUID) (Payment, error) {
	now := s.clock.Now()
	var out Payment
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := lockForReview(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Status != StatusNeedsReview {
			return ErrPaymentNotInReview
		}
		if cur.ReviewSeenAt != nil {
			out = adminPayment(cur)
			return nil
		}
		row, err := q.SetReviewSeen(ctx, store.SetReviewSeenParams{Now: now, Actor: actor, ID: id})
		if err != nil {
			return fmt.Errorf("set review seen: %w", err)
		}
		out = adminPayment(row)
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor, Action: "payment.review_seen", TargetKind: "payment", TargetID: &id,
			After: map[string]any{"status": out.Status, "checkout_id": out.CheckoutID, "order_code": out.OrderCode},
		})
	})
	return out, err
}

// ResolveReview is the "Hoàn thành" button: the Super Admin confirms a
// needs_review payment was handled, with an optional note (trimmed, at most
// 1000 runes, else ErrReviewNoteInvalid; blank is no note). It sets
// review_resolved_at/by and the note (and review_seen_at/by when still
// empty) and audits payment.review_resolve. It changes neither
// payments.status nor the checkout: every refund of the over-, under- or
// duplicate payment was already created by HandlePaid. Not needs_review, or
// already resolved, is ErrPaymentNotInReview.
func (s *Service) ResolveReview(ctx context.Context, actor, id uuid.UUID, note *string) (Payment, error) {
	if note != nil {
		n := strings.TrimSpace(*note)
		switch {
		case n == "":
			note = nil
		case utf8.RuneCountInString(n) > maxReviewNoteRunes || !utf8.ValidString(n) || strings.ContainsRune(n, 0):
			return Payment{}, ErrReviewNoteInvalid
		default:
			note = &n
		}
	}
	now := s.clock.Now()
	var out Payment
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := lockForReview(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Status != StatusNeedsReview || cur.ReviewResolvedAt != nil {
			return ErrPaymentNotInReview
		}
		row, err := q.ResolveReview(ctx, store.ResolveReviewParams{Now: now, Actor: actor, Note: note, ID: id})
		if err != nil {
			return fmt.Errorf("resolve review: %w", err)
		}
		out = adminPayment(row)
		e := audit.Entry{
			ActorUserID: actor, Action: "payment.review_resolve", TargetKind: "payment", TargetID: &id,
			Before: map[string]any{"review_seen_at": cur.ReviewSeenAt},
			After:  map[string]any{"status": out.Status, "checkout_id": out.CheckoutID, "order_code": out.OrderCode},
		}
		if note != nil {
			e.Reason = *note
		}
		return audit.Record(ctx, tx, now, e)
	})
	return out, err
}
