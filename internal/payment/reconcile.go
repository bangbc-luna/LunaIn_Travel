package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bangbc-luna/lunain-travel/internal/payment/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// Mismatch kinds of a reconciliation.
const (
	// MismatchStaleAwaitingProcessed: a transfer left awaiting_status was
	// processed by the reconciliation through ProcessWebhook (Detail: its
	// result).
	MismatchStaleAwaitingProcessed = "stale_awaiting_processed"
	// MismatchRecoveredTransfer is reserved for recording a transfer seen
	// only in the provider's status; ReconcilePayments never does it (see
	// there) and reports MismatchMissingTransfer instead.
	MismatchRecoveredTransfer = "recovered_transfer"
	// MismatchMissingTransfer: the provider reports more money paid on the
	// link than was recorded.
	MismatchMissingTransfer = "missing_transfer"
	// MismatchRecordedExceedsProvider: more money was recorded than the
	// provider reports, StatusLagLimit after the last transfer.
	MismatchRecordedExceedsProvider = "recorded_exceeds_provider"
	// MismatchProviderError: the provider could not be asked, replaying a
	// stale transfer failed, or the run stopped early (context timeout or
	// cancellation; Detail "timeout/cancelled after n/m ...").
	MismatchProviderError = "provider_error"
	// MismatchWindowTruncated: more payments were due for a check than the
	// per-run cap; Detail gives checked/total.
	MismatchWindowTruncated = "window_truncated"
	// MismatchNeedsReviewOpen: a needs_review payment not resolved yet.
	MismatchNeedsReviewOpen = "needs_review_open"
	// MismatchBalanceDrift: ledger.VerifyAll found a drift.
	MismatchBalanceDrift = "balance_drift"
)

// Reconciliation results (payment_reconciliations.status).
const (
	ReconciliationOK       = "ok"
	ReconciliationMismatch = "mismatch"
)

const (
	// reconcileWindow: payments created this recently are checked with the
	// provider every run.
	reconcileWindow = 72 * time.Hour
	// defaultReconcileBatch bounds the stale transfers replayed and the
	// payments checked per run (Service.reconcileBatch).
	defaultReconcileBatch = 500
	// statusCallTimeout bounds each provider call of a reconciliation.
	statusCallTimeout  = 15 * time.Second
	maxReconciliations = 100
)

// ErrReconciliationNotFound: no reconciliation ran on that date.
var ErrReconciliationNotFound = httpx.NewError(http.StatusNotFound, "not_found", "Không tìm thấy kết quả đối soát")

// Mismatch is one problem a reconciliation found.
type Mismatch struct {
	Kind           string     `json:"kind"`
	PaymentID      *uuid.UUID `json:"payment_id"`
	OrderCode      *int64     `json:"order_code"`
	PartnerID      *uuid.UUID `json:"partner_id"`
	BookingID      *uuid.UUID `json:"booking_id"`
	Reference      *string    `json:"reference"`
	ProviderAmount *int64     `json:"provider_amount"`
	RecordedAmount *int64     `json:"recorded_amount"`
	Detail         string     `json:"detail"`
}

// Reconciliation is the stored result of the reconciliation of one Vietnam
// calendar day (the latest run of that day).
type Reconciliation struct {
	ID              uuid.UUID  `json:"id"`
	RunDate         string     `json:"run_date"`
	Status          string     `json:"status"`
	CheckedPayments int        `json:"checked_payments"`
	Mismatches      []Mismatch `json:"mismatches"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      time.Time  `json:"finished_at"`
	RunBy           *uuid.UUID `json:"run_by"`
}

func reconciliationFromRow(r store.PaymentReconciliation) (Reconciliation, error) {
	out := Reconciliation{
		ID: r.ID, RunDate: r.RunDate.Time.Format(time.DateOnly), Status: r.Status,
		CheckedPayments: int(r.CheckedPayments), StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, RunBy: r.RunBy,
		Mismatches: []Mismatch{},
	}
	if err := json.Unmarshal(r.Mismatches, &out.Mismatches); err != nil {
		return Reconciliation{}, fmt.Errorf("decode reconciliation mismatches: %w", err)
	}
	return out, nil
}

// pgDay is the calendar day of d (its own location) as a pgtype.Date.
func pgDay(d time.Time) pgtype.Date {
	y, m, day := d.Date()
	return pgtype.Date{Time: time.Date(y, m, day, 0, 0, 0, 0, time.UTC), Valid: true}
}

// ReconcilePayments compares what was recorded with what the provider
// reports. It runs outside any transaction and asks the provider about one
// payment at a time; it never writes ledger entries or bookings itself.
//
//	(a) Every awaiting_status tracking row received at least StatusLagLimit
//	    before now whose transfer has no main event yet (at most the batch
//	    cap) is replayed through ProcessWebhook with its stored payload: the
//	    normal webhook path, which now processes it flagged needs_review (or
//	    normally if the provider's status caught up).
//	    stale_awaiting_processed (Detail: the result), provider_error when
//	    ProcessWebhook fails.
//	(b) Every payment created in the last reconcileWindow, plus every
//	    unresolved needs_review payment — the unresolved needs_review ones
//	    first, then oldest first, at most the batch cap (window_truncated
//	    with checked/total when there are more): GetStatus. An order code the
//	    provider does not know is skipped when no link was ever stored for
//	    it; any other error is provider_error. recorded = sum of the
//	    payment's event amounts, provider = Status.AmountPaid:
//	    - provider > recorded: missing_transfer, once the gap is
//	      StatusLagLimit old (the newest unrecorded listed transfer, else the
//	      later of the payment's creation and its last recorded transfer):
//	      before that the webhook, or its retries, may still come. The money
//	      is NOT recorded from the status: the PayOS docs ("Lấy thông tin
//	      link thanh toán") document no fields for transactions[], so nothing
//	      establishes that a listed reference is the webhook's
//	      data.reference; recording it under another dedupe key would count
//	      the later webhook as a second transfer and refund the customer
//	      twice. Admins act on the alert.
//	    - provider < recorded, the last recorded transfer at least
//	      StatusLagLimit old: recorded_exceeds_provider.
//	(c) Every unresolved needs_review payment of (b): needs_review_open.
//
// When ctx ends (job timeout, cancelled request) the provider is no longer
// called: what was found so far is returned, plus a provider_error
// "timeout/cancelled after n/m ...", with checked = the payments actually
// checked. err is only returned when a listing query fails for another
// reason; a problem with one item is a Mismatch.
func (s *Service) ReconcilePayments(ctx context.Context, now time.Time, h PaymentHandler) (int, []Mismatch, error) {
	ms := []Mismatch{}
	q := store.New(s.pool)
	stopped := func(what string, done, total int) Mismatch {
		return Mismatch{Kind: MismatchProviderError, Detail: fmt.Sprintf(
			"đối soát dừng giữa chừng (timeout/cancelled after %d/%d %s): %v", done, total, what, context.Cause(ctx))}
	}

	stale, err := q.ListStaleAwaitingEvents(ctx, store.ListStaleAwaitingEventsParams{
		Cutoff: now.Add(-StatusLagLimit), Lim: int32(s.reconcileBatch),
	})
	if err != nil {
		if ctx.Err() != nil {
			return 0, append(ms, stopped("stale transfers", 0, 0)), nil
		}
		return 0, nil, fmt.Errorf("list stale awaiting events: %w", err)
	}
	for i, ev := range stale {
		if ctx.Err() != nil {
			return 0, append(ms, stopped("stale transfers", i, len(stale))), nil
		}
		ms = append(ms, s.replayAwaiting(ctx, q, ev, h))
	}

	since := now.Add(-reconcileWindow)
	total, err := q.CountPaymentsToReconcile(ctx, since)
	if err == nil {
		var rows []store.Payment
		rows, err = q.ListPaymentsToReconcile(ctx, store.ListPaymentsToReconcileParams{
			Since: since, Lim: int32(s.reconcileBatch),
		})
		if err == nil {
			return s.checkPayments(ctx, q, rows, int(total), now, ms, stopped)
		}
	}
	if ctx.Err() != nil {
		return 0, append(ms, stopped("payments", 0, 0)), nil
	}
	return 0, nil, fmt.Errorf("list payments to reconcile: %w", err)
}

// checkPayments is steps (b) and (c) of ReconcilePayments over rows (total
// candidates in all).
func (s *Service) checkPayments(ctx context.Context, q *store.Queries, rows []store.Payment, total int, now time.Time,
	ms []Mismatch, stopped func(string, int, int) Mismatch) (int, []Mismatch, error) {
	for i, row := range rows {
		if ctx.Err() != nil {
			return i, append(ms, stopped("payments", i, len(rows))), nil
		}
		p := fromRow(row)
		m, err := s.checkPayment(ctx, q, p, now)
		if err != nil {
			if ctx.Err() != nil {
				return i, append(ms, stopped("payments", i, len(rows))), nil
			}
			s.log.Error("payment: reconcile check failed", "payment_id", p.ID, "err", err)
			m = &Mismatch{Kind: MismatchProviderError, PaymentID: &p.ID, OrderCode: &p.OrderCode, Detail: err.Error()}
		}
		if m != nil {
			ms = append(ms, *m)
		}
		if p.Status == StatusNeedsReview && p.ReviewResolvedAt == nil {
			ms = append(ms, Mismatch{
				Kind: MismatchNeedsReviewOpen, PaymentID: &p.ID, OrderCode: &p.OrderCode,
				Detail: "thanh toán cần xem xét chưa được xử lý",
			})
		}
	}
	if total > len(rows) {
		ms = append(ms, Mismatch{Kind: MismatchWindowTruncated, Detail: fmt.Sprintf(
			"đã kiểm tra %d/%d thanh toán (giới hạn mỗi lần chạy), phần còn lại chưa được đối soát", len(rows), total)})
	}
	return len(rows), ms, nil
}

// replayAwaiting runs the stored payload of a stale awaiting_status row
// through ProcessWebhook and reports what happened.
func (s *Service) replayAwaiting(ctx context.Context, q *store.Queries, ev store.ListStaleAwaitingEventsRow, h PaymentHandler) Mismatch {
	m := Mismatch{Kind: MismatchStaleAwaitingProcessed, PaymentID: ev.PaymentID}
	key := strings.TrimSuffix(ev.DedupeKey, ":awaiting")
	if wd, err := s.provider.VerifyWebhook(ev.Payload); err == nil {
		oc, ref := wd.OrderCode, TransferReference(wd)
		m.OrderCode, m.Reference = &oc, &ref
		amount := wd.Amount
		m.ProviderAmount = &amount
	}
	if err := s.ProcessWebhook(ctx, ev.Payload, h); err != nil {
		s.log.Error("payment: reconcile replay of an awaiting transfer failed", "dedupe_key", ev.DedupeKey, "err", err)
		m.Kind, m.Detail = MismatchProviderError, "xử lý lại giao dịch chờ xác nhận thất bại: "+err.Error()
		return m
	}
	res, err := q.GetPaymentEventResult(ctx, key)
	switch {
	case err != nil:
		m.Detail = "đã xử lý lại, không đọc được kết quả: " + err.Error()
	case res == nil:
		m.Detail = "đang xử lý"
	default:
		m.Detail = *res
	}
	return m
}

// checkPayment compares one payment with the provider (step (b) of
// ReconcilePayments). The provider is called outside any tx. A failed
// database read, or the provider failing because ctx ended, is an error.
func (s *Service) checkPayment(ctx context.Context, q *store.Queries, p Payment, now time.Time) (*Mismatch, error) {
	base := Mismatch{PaymentID: &p.ID, OrderCode: &p.OrderCode}
	cctx, cancel := context.WithTimeout(ctx, statusCallTimeout)
	st, err := s.provider.GetStatus(cctx, p.OrderCode)
	cancel()
	if err != nil {
		if IsOrderNotFound(err) && p.CheckoutURL == nil {
			return nil, nil // the link was never created
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		m := base
		m.Kind, m.Detail = MismatchProviderError, err.Error()
		return &m, nil
	}
	rec, err := q.PaymentRecorded(ctx, &p.ID)
	if err != nil {
		return nil, fmt.Errorf("payment recorded amount: %w", err)
	}
	provider, recorded := st.AmountPaid, rec.Recorded
	m := base
	m.ProviderAmount, m.RecordedAmount = &provider, &recorded
	cutoff := now.Add(-StatusLagLimit)
	switch {
	case provider > recorded:
		refs, newest, err := s.unrecordedTransfers(ctx, q, p.OrderCode, st.Transactions)
		if err != nil {
			return nil, err
		}
		if newest.IsZero() {
			newest = p.CreatedAt
			if rec.LastReceivedAt.After(newest) {
				newest = rec.LastReceivedAt
			}
		}
		if newest.After(cutoff) {
			return nil, nil // grace: the webhook (or its retries) may still come
		}
		m.Kind = MismatchMissingTransfer
		m.Detail = fmt.Sprintf("cổng thanh toán báo đã nhận %d, hệ thống ghi nhận %d", provider, recorded)
		if len(refs) > 0 {
			m.Reference = &refs[0]
			m.Detail += "; giao dịch chưa ghi nhận: " + strings.Join(refs, ", ")
		}
		return &m, nil
	case provider < recorded && !rec.LastReceivedAt.After(cutoff):
		m.Kind = MismatchRecordedExceedsProvider
		m.Detail = fmt.Sprintf("hệ thống ghi nhận %d, cổng thanh toán chỉ báo %d", recorded, provider)
		return &m, nil
	}
	return nil, nil
}

// unrecordedTransfers lists, for the alert only, the non-empty references
// of the provider's transactions with no event under DedupeKey(orderCode,
// reference), and the newest non-zero time among them (zero when none).
func (s *Service) unrecordedTransfers(ctx context.Context, q *store.Queries, orderCode int64, txs []StatusTransaction) ([]string, time.Time, error) {
	var out []string
	var newest time.Time
	for _, tx := range txs {
		if tx.Reference == "" {
			continue
		}
		seen, err := q.PaymentEventExists(ctx, DedupeKey(orderCode, tx.Reference))
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("payment event exists: %w", err)
		}
		if !seen {
			out = append(out, tx.Reference)
			if tx.At.After(newest) {
				newest = tx.At
			}
		}
	}
	return out, newest, nil
}

// SaveReconciliation stores the result of the run of runDate (its calendar
// day in its own location), replacing an earlier run of the same day:
// status ok when ms is empty, mismatch otherwise. runBy is nil for the
// scheduled job.
func (s *Service) SaveReconciliation(ctx context.Context, runDate time.Time, checked int, ms []Mismatch, runBy *uuid.UUID, started, finished time.Time) (Reconciliation, error) {
	return s.saveReconciliation(ctx, store.New(s.pool), runDate, checked, ms, runBy, started, finished)
}

// SaveReconciliationTx is SaveReconciliation inside the caller's tx.
func (s *Service) SaveReconciliationTx(ctx context.Context, tx pgx.Tx, runDate time.Time, checked int, ms []Mismatch, runBy *uuid.UUID, started, finished time.Time) (Reconciliation, error) {
	return s.saveReconciliation(ctx, store.New(tx), runDate, checked, ms, runBy, started, finished)
}

func (s *Service) saveReconciliation(ctx context.Context, q *store.Queries, runDate time.Time, checked int, ms []Mismatch, runBy *uuid.UUID, started, finished time.Time) (Reconciliation, error) {
	if ms == nil {
		ms = []Mismatch{}
	}
	raw, err := json.Marshal(ms)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("encode mismatches: %w", err)
	}
	status := ReconciliationOK
	if len(ms) > 0 {
		status = ReconciliationMismatch
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Reconciliation{}, err
	}
	row, err := q.UpsertReconciliation(ctx, store.UpsertReconciliationParams{
		ID: id, RunDate: pgDay(runDate), Status: status, CheckedPayments: int32(checked), Mismatches: raw,
		StartedAt: started, FinishedAt: finished, RunBy: runBy, Now: s.clock.Now(),
	})
	if err != nil {
		return Reconciliation{}, fmt.Errorf("upsert reconciliation: %w", err)
	}
	return reconciliationFromRow(row)
}

// ListReconciliations returns the stored runs, newest day first, strictly
// before the calendar day of before when given. limit <= 0 means 30; it is
// capped at maxReconciliations.
func (s *Service) ListReconciliations(ctx context.Context, limit int, before *time.Time) ([]Reconciliation, error) {
	if limit <= 0 {
		limit = 30
	}
	limit = min(limit, maxReconciliations)
	var b pgtype.Date
	if before != nil {
		b = pgDay(*before)
	}
	rows, err := store.New(s.pool).ListReconciliations(ctx, store.ListReconciliationsParams{Before: b, Lim: int32(limit)})
	if err != nil {
		return nil, fmt.Errorf("list reconciliations: %w", err)
	}
	out := make([]Reconciliation, 0, len(rows))
	for _, r := range rows {
		rec, err := reconciliationFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// GetReconciliation returns the run of the calendar day of runDate, or
// ErrReconciliationNotFound.
func (s *Service) GetReconciliation(ctx context.Context, runDate time.Time) (Reconciliation, error) {
	row, err := store.New(s.pool).GetReconciliation(ctx, pgDay(runDate))
	if errors.Is(err, pgx.ErrNoRows) {
		return Reconciliation{}, ErrReconciliationNotFound
	}
	if err != nil {
		return Reconciliation{}, fmt.Errorf("get reconciliation: %w", err)
	}
	return reconciliationFromRow(row)
}
