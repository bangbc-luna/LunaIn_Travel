// Package reconcile runs the daily reconciliation: it owns no table, it asks
// payment to compare recorded money with the provider, asks ledger to verify
// every balance, stores the result through payment and alerts the Super
// Admins by email (outbox).
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
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
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
)

// vnZone is Vietnam time (UTC+7, no DST): run dates are Vietnam days.
var vnZone = time.FixedZone("ICT", 7*3600)

// maxAlertLines bounds the mismatches listed one by one in the alert email.
const maxAlertLines = 50

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
func (s *Service) Run(ctx context.Context, runBy *uuid.UUID) (payment.Reconciliation, error) {
	started := s.d.Clock.Now()
	runDate := started.In(vnZone)
	checked, ms, err := s.d.Payments.ReconcilePayments(ctx, started, s.d.Handler)
	if err != nil {
		return payment.Reconciliation{}, err
	}
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
