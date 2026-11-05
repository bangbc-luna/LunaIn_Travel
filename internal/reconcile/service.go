// Package reconcile runs the daily reconciliation: it owns no table, it asks
// payment to compare recorded money with the provider, asks ledger to verify
// every balance, stores the result through payment and alerts the Super
// Admins by email (outbox).
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
)

// vnZone is Vietnam time (UTC+7, no DST): run dates are Vietnam days.
var vnZone = time.FixedZone("ICT", 7*3600)

const (
	// maxAlertLines bounds the mismatches listed one by one in the alert
	// email.
	maxAlertLines = 50
	// finishTimeout bounds the ledger check, the save and the alert, which
	// run even when the run's own context ended.
	finishTimeout = 2 * time.Minute
	// unlockTimeout bounds releasing the run lock.
	unlockTimeout = 10 * time.Second
)

// RunLockKey is the Postgres advisory lock key held while a reconciliation
// runs.
const RunLockKey int64 = 0x4c756e6152656331

// ErrRunning: another reconciliation is running.
var ErrRunning = httpx.NewError(http.StatusConflict, "reconciliation_running",
	"Đối soát đang chạy, vui lòng thử lại sau ít phút")

// lock takes the run lock (session-level, on a connection held until
// unlock) or answers ErrRunning. If the unlock fails, the connection is
// closed, which releases the lock too.
func (s *Service) lock(ctx context.Context) (func(), error) {
	conn, err := s.d.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire reconcile lock connection: %w", err)
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, RunLockKey).Scan(&ok); err != nil {
		conn.Release()
		return nil, fmt.Errorf("take reconcile lock: %w", err)
	}
	if !ok {
		conn.Release()
		return nil, ErrRunning
	}
	return func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, RunLockKey); err != nil {
			s.d.Log.Error("reconcile: unlock failed, closing the connection", "err", err)
			_ = conn.Conn().Close(uctx)
		}
		conn.Release()
	}, nil
}

// Deps are the collaborators of Service. Jobs may be nil (tests): the alert
// is then only logged.
type Deps struct {
	Pool     *pgxpool.Pool
	Clock    clock.Clock
	Payments *payment.Service
	Ledger   *ledger.Service
	// Handler applies replayed transfers (booking.Service).
	Handler  payment.PaymentHandler
	Identity *identity.Service
	Jobs     *river.Client[pgx.Tx]
	Log      *slog.Logger
}

// Service runs reconciliations.
type Service struct {
	d Deps
}

// NewService builds a Service. A nil Log is slog.Default.
func NewService(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Service{d: d}
}

// Run reconciles now; runBy is the Super Admin of a manual run, nil for the
// scheduled job:
//  1. the run date is the Vietnam day of now;
//  2. payment.ReconcilePayments (outside any tx, provider calls one by one);
//  3. ledger.VerifyAll, each drift a balance_drift mismatch;
//  4. in one tx: the result is stored (replacing an earlier run of the same
//     day), an alert listing the mismatches by kind is queued to every
//     active Super Admin when there are any, and a manual run is audited
//     (reconciliation.run).
//
// Only one run at a time (a session advisory lock on a pinned connection):
// another one gets ErrRunning. When ctx ends during step 2 the provider is
// no longer called (payment.ReconcilePayments records it as a
// provider_error); steps 3 and 4 then run on a detached context bounded by
// finishTimeout, so the result is still stored and the admins alerted.
func (s *Service) Run(ctx context.Context, runBy *uuid.UUID) (payment.Reconciliation, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return payment.Reconciliation{}, err
	}
	defer unlock()

	started := s.d.Clock.Now()
	runDate := started.In(vnZone)
	checked, ms, err := s.d.Payments.ReconcilePayments(ctx, started, s.d.Handler)
	if err != nil {
		return payment.Reconciliation{}, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	drifts, err := s.d.Ledger.VerifyAll(ctx)
	if err != nil {
		return payment.Reconciliation{}, err
	}
	for _, d := range drifts {
		pid := d.PartnerID
		ms = append(ms, payment.Mismatch{
			Kind: payment.MismatchBalanceDrift, PartnerID: &pid, BookingID: d.BookingID, Detail: d.Problem,
		})
	}
	finished := s.d.Clock.Now()

	var rec payment.Reconciliation
	err = db.InTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var err error
		rec, err = s.d.Payments.SaveReconciliationTx(ctx, tx, runDate, checked, ms, runBy, started, finished)
		if err != nil {
			return err
		}
		if len(rec.Mismatches) > 0 {
			if err := s.alert(ctx, tx, rec); err != nil {
				return err
			}
		}
		if runBy != nil {
			return audit.Record(ctx, tx, finished, audit.Entry{
				ActorUserID: *runBy, Action: "reconciliation.run", TargetKind: "payment_reconciliation",
				TargetID: &rec.ID,
				After: map[string]any{
					"run_date": rec.RunDate, "status": rec.Status, "checked_payments": rec.CheckedPayments,
					"mismatches": len(rec.Mismatches),
				},
			})
		}
		return nil
	})
	if err != nil {
		return payment.Reconciliation{}, err
	}
	if len(rec.Mismatches) > 0 {
		s.d.Log.Warn("reconcile: mismatches found", "run_date", rec.RunDate, "mismatches", len(rec.Mismatches))
	}
	return rec, nil
}

// alert queues, in tx, the mismatch email to every active Super Admin.
func (s *Service) alert(ctx context.Context, tx pgx.Tx, rec payment.Reconciliation) error {
	subject := fmt.Sprintf("[LunaIn] Đối soát %s: %d chênh lệch", rec.RunDate, len(rec.Mismatches))
	if s.d.Jobs == nil || s.d.Identity == nil {
		s.d.Log.Info("reconcile: alert not queued (no job client)", "subject", subject)
		return nil
	}
	to, err := s.d.Identity.SuperAdminEmails(ctx, tx)
	if err != nil {
		return err
	}
	if len(to) == 0 {
		s.d.Log.Error("reconcile: no active super admin to alert", "subject", subject)
		return nil
	}
	text := alertText(rec)
	for _, addr := range to {
		if err := mailer.EnqueueTx(ctx, tx, s.d.Jobs, mailer.Message{To: addr, Subject: subject, Text: text}); err != nil {
			return fmt.Errorf("enqueue reconciliation alert: %w", err)
		}
	}
	return nil
}

// alertText lists the mismatches grouped by kind (kinds sorted), at most
// maxAlertLines of them in detail.
func alertText(rec payment.Reconciliation) string {
	byKind := map[string][]payment.Mismatch{}
	for _, m := range rec.Mismatches {
		byKind[m.Kind] = append(byKind[m.Kind], m)
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	var b strings.Builder
	fmt.Fprintf(&b, "Đối soát ngày %s: đã kiểm tra %d thanh toán, phát hiện %d chênh lệch.\n\n",
		rec.RunDate, rec.CheckedPayments, len(rec.Mismatches))
	for _, k := range kinds {
		fmt.Fprintf(&b, "- %s: %d\n", k, len(byKind[k]))
	}
	b.WriteString("\nChi tiết:\n")
	lines := 0
	for _, k := range kinds {
		for _, m := range byKind[k] {
			if lines == maxAlertLines {
				fmt.Fprintf(&b, "... và %d chênh lệch khác, xem trang quản trị đối soát.\n", len(rec.Mismatches)-lines)
				return b.String()
			}
			b.WriteString(mismatchLine(m))
			lines++
		}
	}
	return b.String()
}

func mismatchLine(m payment.Mismatch) string {
	parts := []string{"[" + m.Kind + "]"}
	if m.OrderCode != nil {
		parts = append(parts, "mã đơn "+strconv.FormatInt(*m.OrderCode, 10))
	}
	if m.PaymentID != nil {
		parts = append(parts, "thanh toán "+m.PaymentID.String())
	}
	if m.PartnerID != nil {
		parts = append(parts, "đối tác "+m.PartnerID.String())
	}
	if m.BookingID != nil {
		parts = append(parts, "booking "+m.BookingID.String())
	}
	if m.Reference != nil {
		parts = append(parts, "tham chiếu "+*m.Reference)
	}
	if m.ProviderAmount != nil {
		parts = append(parts, "cổng báo "+strconv.FormatInt(*m.ProviderAmount, 10))
	}
	if m.RecordedAmount != nil {
		parts = append(parts, "đã ghi "+strconv.FormatInt(*m.RecordedAmount, 10))
	}
	if m.Detail != "" {
		parts = append(parts, m.Detail)
	}
	return strings.Join(parts, " | ") + "\n"
}
