package booking

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

// Periodic jobs (River): every minute, at most jobBatchSize bookings per run,
// one transaction per checkout or booking. A failing item is logged and
// skipped; the next run picks it up again.
const (
	jobInterval  = time.Minute
	jobBatchSize = 200

	StatusNoShow = "no_show"

	checkoutOpen = "open"
	// cancelLinkTimeout bounds each provider call cancelling an expired link.
	cancelLinkTimeout = 10 * time.Second

	// reasonHoldExpired: history reason of the hold expiry job.
	reasonHoldExpired = "hết thời gian giữ chỗ"
	actorPartner      = "partner"
)

// ExpireHoldsArgs is the periodic hold expiry job.
type ExpireHoldsArgs struct{}

func (ExpireHoldsArgs) Kind() string { return "booking_expire_holds" }

// NoShowArgs is the periodic no-show job.
type NoShowArgs struct{}

func (NoShowArgs) Kind() string { return "booking_no_show" }

// ReleaseArgs is the periodic settlement release job.
type ReleaseArgs struct{}

func (ReleaseArgs) Kind() string { return "booking_release" }

// ExpireHoldsWorker runs Service.ExpireHolds. Service is set before the job
// client starts.
type ExpireHoldsWorker struct {
	river.WorkerDefaults[ExpireHoldsArgs]
	Service *Service
}

func (w *ExpireHoldsWorker) Work(ctx context.Context, _ *river.Job[ExpireHoldsArgs]) error {
	_, err := w.Service.ExpireHolds(ctx)
	return err
}

// NoShowWorker runs Service.MarkNoShows.
type NoShowWorker struct {
	river.WorkerDefaults[NoShowArgs]
	Service *Service
}

func (w *NoShowWorker) Work(ctx context.Context, _ *river.Job[NoShowArgs]) error {
	_, err := w.Service.MarkNoShows(ctx)
	return err
}

// ReleaseWorker runs Service.ReleaseDue.
type ReleaseWorker struct {
	river.WorkerDefaults[ReleaseArgs]
	Service *Service
}

func (w *ReleaseWorker) Work(ctx context.Context, _ *river.Job[ReleaseArgs]) error {
	_, err := w.Service.ReleaseDue(ctx)
	return err
}

// PeriodicJobs are the booking jobs, every minute, not run on start. A run
// is never retried: the next tick does the same work.
func PeriodicJobs() []*river.PeriodicJob {
	every := river.PeriodicInterval(jobInterval)
	opts := &river.InsertOpts{MaxAttempts: 1}
	job := func(args river.JobArgs) *river.PeriodicJob {
		return river.NewPeriodicJob(every, func() (river.JobArgs, *river.InsertOpts) { return args, opts },
			&river.PeriodicJobOpts{RunOnStart: false})
	}
	return []*river.PeriodicJob{job(ExpireHoldsArgs{}), job(NoShowArgs{}), job(ReleaseArgs{})}
}

// ExpireHolds expires the pending_payment bookings whose hold passed release
// (hold_release_at <= now) and returns how many it expired.
//
// Candidates are read unlocked, then each checkout is handled in its own tx
// with the webhook's lock order (checkout -> bookings -> payments) so that a
// concurrent HandlePaid serializes with it; the state is re-checked under the
// locks: every still-pending booking past release becomes expired (history,
// actor system). When no booking of the checkout remains pending or went past
// payment (confirmed or later), its pending payments become expired and the
// checkout closed. After commit the expired payments' links are cancelled at
// the provider, best effort. Money arriving later is a late payment (Review
// Focus 2, HandlePaid).
func (s *Service) ExpireHolds(ctx context.Context) (int, error) {
	now := s.clock.Now()
	rows, err := store.New(s.pool).ListHoldsToRelease(ctx, store.ListHoldsToReleaseParams{Now: now, Lim: jobBatchSize})
	if err != nil {
		return 0, fmt.Errorf("list holds to release: %w", err)
	}
	var checkouts []uuid.UUID
	for _, r := range rows {
		if !slices.Contains(checkouts, r.CheckoutID) {
			checkouts = append(checkouts, r.CheckoutID)
		}
	}
	total := 0
	for _, id := range checkouts {
		var n int
		var codes []int64
		err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			var err error
			n, codes, err = s.expireCheckout(ctx, tx, id, now)
			return err
		})
		if err != nil {
			s.log.Error("booking: expire holds failed", "checkout_id", id, "err", err)
			continue
		}
		total += n
		for _, oc := range codes {
			s.cancelLink(ctx, oc)
		}
	}
	return total, nil
}

// cancelLink cancels an expired payment's link at the provider after commit,
// best effort: detached from the job's cancellation (the expiry already
// committed) but bounded by cancelLinkTimeout.
func (s *Service) cancelLink(ctx context.Context, orderCode int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelLinkTimeout)
	defer cancel()
	s.payments.CancelLinkBestEffort(ctx, orderCode)
}

// expireCheckout is one checkout of ExpireHolds; it returns the number of
// bookings expired and the order codes of the payments expired.
func (s *Service) expireCheckout(ctx context.Context, tx pgx.Tx, checkoutID uuid.UUID, now time.Time) (int, []int64, error) {
	q := store.New(tx)
	co, err := q.LockCheckout(ctx, checkoutID)
	if err != nil {
		return 0, nil, fmt.Errorf("lock checkout: %w", err)
	}
	bookings, err := q.LockCheckoutBookings(ctx, checkoutID)
	if err != nil {
		return 0, nil, fmt.Errorf("lock checkout bookings: %w", err)
	}
	n := 0
	done := true
	reason := reasonHoldExpired
	for i, b := range bookings {
		if b.Status == StatusPendingPayment && b.HoldReleaseAt != nil && !b.HoldReleaseAt.After(now) {
			if err := q.ExpireBooking(ctx, store.ExpireBookingParams{Now: now, ID: b.ID}); err != nil {
				return 0, nil, fmt.Errorf("expire booking: %w", err)
			}
			if err := insertHistory(ctx, q, b, StatusExpired, actorSystem, nil, &reason, now); err != nil {
				return 0, nil, err
			}
			bookings[i].Status = StatusExpired
			n++
		}
		if st := bookings[i].Status; st != StatusExpired && st != StatusCancelled {
			done = false
		}
	}
	if n == 0 || !done {
		return n, nil, nil
	}
	pays, err := s.payments.LockCheckoutPayments(ctx, tx, checkoutID)
	if err != nil {
		return 0, nil, err
	}
	var codes []int64
	for _, p := range pays {
		if p.Status != payment.StatusPending {
			continue
		}
		if err := s.payments.ExpirePending(ctx, tx, p.ID); err != nil {
			return 0, nil, err
		}
		codes = append(codes, p.OrderCode)
	}
	if co.Status == checkoutOpen {
		if err := q.SetCheckoutStatus(ctx, store.SetCheckoutStatusParams{Status: checkoutClosed, Now: now, ID: co.ID}); err != nil {
			return 0, nil, fmt.Errorf("close checkout: %w", err)
		}
	}
	return n, codes, nil
}

// MarkNoShows moves confirmed attraction bookings whose service ended
// (service_end_at < now) to no_show and schedules their release
// release_delay_days later. It returns how many it marked.
func (s *Service) MarkNoShows(ctx context.Context) (int, error) {
	now := s.clock.Now()
	ids, err := store.New(s.pool).ListNoShowCandidates(ctx, store.ListNoShowCandidatesParams{Now: now, Lim: jobBatchSize})
	if err != nil {
		return 0, fmt.Errorf("list no-show candidates: %w", err)
	}
	total := 0
	for _, id := range ids {
		var marked bool
		err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			q := store.New(tx)
			b, err := q.LockBooking(ctx, id)
			if err != nil {
				return fmt.Errorf("lock booking: %w", err)
			}
			if b.Status != StatusConfirmed || b.ServiceType != serviceTypeAttraction || !b.ServiceEndAt.Before(now) {
				return nil
			}
			if err := q.MarkNoShow(ctx, store.MarkNoShowParams{Now: now, ID: b.ID}); err != nil {
				return fmt.Errorf("mark no-show: %w", err)
			}
			if err := insertHistory(ctx, q, b, StatusNoShow, actorSystem, nil, nil, now); err != nil {
				return err
			}
			if err := s.scheduleRelease(ctx, tx, b.ID, now); err != nil {
				return err
			}
			marked = true
			return nil
		})
		if err != nil {
			s.log.Error("booking: no-show failed", "booking_id", id, "err", err)
			continue
		}
		if marked {
			total++
		}
	}
	return total, nil
}

// ReleaseDue releases the settlements due at now (release_due_at <= now,
// not released yet), listed unlocked: each booking is locked FOR UPDATE, the
// settlement re-checked as still due (ledger.ReleaseIfDue; skipped otherwise)
// and its net pending moved to available. It returns how many it released.
func (s *Service) ReleaseDue(ctx context.Context) (int, error) {
	now := s.clock.Now()
	ids, err := s.ledger.DueReleases(ctx, now, jobBatchSize)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, id := range ids {
		var released bool
		err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			if _, err := store.New(tx).LockBooking(ctx, id); err != nil {
				return fmt.Errorf("lock booking: %w", err)
			}
			var err error
			released, err = s.ledger.ReleaseIfDue(ctx, tx, id, now)
			return err
		})
		if err != nil {
			s.log.Error("booking: release failed", "booking_id", id, "err", err)
			continue
		}
		if released {
			total++
		}
	}
	return total, nil
}

// scheduleRelease schedules the release of a locked booking
// release_delay_days after at.
func (s *Service) scheduleRelease(ctx context.Context, tx pgx.Tx, bookingID uuid.UUID, at time.Time) error {
	cfg, err := s.settings.GetTx(ctx, tx)
	if err != nil {
		return err
	}
	due := at.Add(time.Duration(cfg.ReleaseDelayDays) * 24 * time.Hour)
	if err := s.ledger.ScheduleRelease(ctx, tx, bookingID, due); err != nil {
		return fmt.Errorf("schedule release: %w", err)
	}
	return nil
}

// insertHistory records a status change of a locked booking.
func insertHistory(ctx context.Context, q *store.Queries, b store.Booking, to, actorKind string, actorUserID *uuid.UUID, reason *string, now time.Time) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	from := b.Status
	if err := q.InsertStatusHistory(ctx, store.InsertStatusHistoryParams{
		ID: id, BookingID: b.ID, FromStatus: &from, ToStatus: to,
		ActorKind: actorKind, ActorUserID: actorUserID, Reason: reason, Now: now,
	}); err != nil {
		return fmt.Errorf("insert status history: %w", err)
	}
	return nil
}
