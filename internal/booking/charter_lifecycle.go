package booking

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// Private charter lifecycle after payment (spec §6.2, plan 8 default C10):
// the partner picks the customer up by scanning the QR (confirmed ->
// in_service), completes the trip at any time (in_service -> completed) or
// marks a customer who never came from pickup_at on (confirmed -> no_show).
// FinishCharters ends what the partner forgot, charter_auto_complete_hours
// after service_end_at. A charter holds no capacity: ending it only
// schedules the partner's release release_delay_days later (endService);
// a no-show is not refunded.

// charterPickupEarly: the customer can be picked up from this long before
// pickup_at until the trip is ended by FinishCharters.
const charterPickupEarly = 2 * time.Hour

const reasonCharterAutoNoShow = "nhà xe không xác nhận đón khách trước khi chuyến tự kết thúc"

func reasonCharterAutoComplete(hours int) string {
	return fmt.Sprintf("tự hoàn thành sau %d giờ quá giờ kết thúc chuyến", hours)
}

var (
	// ErrPickupOutsideWindow: the QR of a charter was scanned outside
	// [pickup_at − 2h, service_end_at + charter_auto_complete_hours).
	ErrPickupOutsideWindow = httpx.NewError(http.StatusUnprocessableEntity, "pickup_outside_window",
		"Chỉ đón khách được từ 2 giờ trước giờ đón đến khi chuyến tự kết thúc")

	errNotCharter = httpx.NewError(ErrInvalidBookingState.Status, ErrInvalidBookingState.Code,
		"Thao tác này chỉ dành cho đơn xe bao chuyến")
	errCompleteState = httpx.NewError(ErrInvalidBookingState.Status, ErrInvalidBookingState.Code,
		"Chỉ hoàn thành chuyến cho đơn xe bao chuyến đã đón khách")
	errCharterNoShowState = httpx.NewError(ErrInvalidBookingState.Status, ErrInvalidBookingState.Code,
		"Chỉ đánh dấu khách không đến cho đơn xe bao chuyến đã xác nhận và chưa đón khách")
	errNoShowType = httpx.NewError(ErrInvalidBookingState.Status, ErrInvalidBookingState.Code,
		"Chỉ đánh dấu khách không đến cho đơn thuê xe tự lái hoặc xe bao chuyến")
)

// checkInCharter picks up the customer of a locked, not yet checked-in
// charter booking: confirmed (else ErrCheckInNotAllowed) and now in
// [pickup_at − charterPickupEarly, service_end_at +
// charter_auto_complete_hours) (else ErrPickupOutsideWindow), it becomes
// in_service with in_service_at = checked_in_at = now (a second scan is
// answered Already), history with the partner user as actor. Nothing is
// released: CompleteCharter or FinishCharters ends it.
func (s *Service) checkInCharter(ctx context.Context, tx pgx.Tx, q *store.Queries, b store.Booking, userID uuid.UUID, now time.Time) (CheckInResult, error) {
	if b.Status != StatusConfirmed {
		return CheckInResult{}, ErrCheckInNotAllowed
	}
	cfg, err := s.settings.GetTx(ctx, tx)
	if err != nil {
		return CheckInResult{}, err
	}
	end := b.ServiceEndAt.Add(time.Duration(cfg.CharterAutoCompleteHours) * time.Hour)
	if now.Before(b.ServiceStartAt.Add(-charterPickupEarly)) || !now.Before(end) {
		return CheckInResult{}, ErrPickupOutsideWindow
	}
	if err := q.HandOverRental(ctx, store.HandOverRentalParams{Now: now, ID: b.ID}); err != nil {
		return CheckInResult{}, fmt.Errorf("pick up charter: %w", err)
	}
	if err := insertHistory(ctx, q, b, StatusInService, actorPartner, &userID, nil, now); err != nil {
		return CheckInResult{}, err
	}
	return CheckInResult{CheckedInAt: now}, nil
}

// partnerBooking reads (unlocked) a booking of partnerID: another partner's
// or unknown is ErrBookingNotFound.
func (s *Service) partnerBooking(ctx context.Context, partnerID, bookingID uuid.UUID) (store.Booking, error) {
	b, err := store.New(s.pool).GetBooking(ctx, bookingID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && b.PartnerID != partnerID) {
		return store.Booking{}, ErrBookingNotFound
	}
	if err != nil {
		return store.Booking{}, fmt.Errorf("get booking: %w", err)
	}
	return b, nil
}

// lockCharter locks a charter booking in tx (a charter has no capacity row
// before bookings in the lock order) and returns it as locked.
func lockCharter(ctx context.Context, tx pgx.Tx, bookingID uuid.UUID) (store.Booking, error) {
	b, err := store.New(tx).LockBooking(ctx, bookingID)
	if err != nil {
		return store.Booking{}, fmt.Errorf("lock booking: %w", err)
	}
	return b, nil
}

// CompleteCharter records, as userID of partnerID, that the trip of an
// in_service charter is over, at any time: it becomes completed and the
// partner is paid release_delay_days later. An already completed booking is
// returned unchanged; another status is 409 invalid_booking_state, as is
// another service type; another partner's booking is ErrBookingNotFound.
func (s *Service) CompleteCharter(ctx context.Context, partnerID, userID, bookingID uuid.UUID) (BookingView, error) {
	b, err := s.partnerBooking(ctx, partnerID, bookingID)
	if err != nil {
		return BookingView{}, err
	}
	if b.ServiceType != serviceTypeCharter {
		return BookingView{}, errNotCharter
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		b, err := lockCharter(ctx, tx, bookingID)
		if err != nil {
			return err
		}
		switch b.Status {
		case StatusCompleted:
			return nil
		case StatusInService:
			return s.endService(ctx, tx, b, StatusCompleted, actorPartner, &userID, nil, s.clock.Now())
		}
		return errCompleteState
	})
	if err != nil {
		return BookingView{}, err
	}
	return s.GetForPartner(ctx, partnerID, bookingID)
}

// MarkNoShow marks, as userID of partnerID, that the customer never came:
// a self-drive rental follows MarkRentalNoShow; a private charter must be
// confirmed (not picked up, else 409 invalid_booking_state) and now at or
// after pickup_at (else ErrNoShowTooEarly), it becomes no_show and the
// partner is paid release_delay_days later; nothing is refunded. Another
// service type is 409 invalid_booking_state; another partner's booking
// ErrBookingNotFound.
func (s *Service) MarkNoShow(ctx context.Context, partnerID, userID, bookingID uuid.UUID) (BookingView, error) {
	b, err := s.partnerBooking(ctx, partnerID, bookingID)
	if err != nil {
		return BookingView{}, err
	}
	switch b.ServiceType {
	case serviceTypeRental:
		return s.MarkRentalNoShow(ctx, partnerID, userID, bookingID)
	case serviceTypeCharter:
	default:
		return BookingView{}, errNoShowType
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		b, err := lockCharter(ctx, tx, bookingID)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		if b.Status != StatusConfirmed {
			return errCharterNoShowState
		}
		if now.Before(b.ServiceStartAt) {
			return ErrNoShowTooEarly
		}
		return s.endService(ctx, tx, b, StatusNoShow, actorPartner, &userID, nil, now)
	})
	if err != nil {
		return BookingView{}, err
	}
	return s.GetForPartner(ctx, partnerID, bookingID)
}

// FinishChartersArgs is the periodic charter auto-finish job.
type FinishChartersArgs struct{}

func (FinishChartersArgs) Kind() string { return "booking_finish_charters" }

// FinishChartersWorker runs Service.FinishCharters.
type FinishChartersWorker struct {
	river.WorkerDefaults[FinishChartersArgs]
	Service *Service
}

func (w *FinishChartersWorker) Work(ctx context.Context, _ *river.Job[FinishChartersArgs]) error {
	_, err := w.Service.FinishCharters(ctx)
	return err
}

// FinishCharters ends the charters the partner did not, so the partner's
// money is never stuck: charter_auto_complete_hours (read on every run)
// after service_end_at (service_end_at <= cutoff = now − that), an
// in_service charter becomes completed and a confirmed one (never picked
// up) no_show; both schedule the release release_delay_days after now,
// history actor system. Each booking in its own tx, locked and its state
// re-checked, so a partner's click that came first wins. It returns how
// many it ended.
func (s *Service) FinishCharters(ctx context.Context) (int, error) {
	now := s.clock.Now()
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-time.Duration(cfg.CharterAutoCompleteHours) * time.Hour)
	ids, err := store.New(s.pool).ListCharterAutoFinish(ctx, store.ListCharterAutoFinishParams{
		Cutoff: cutoff, Lim: s.batch(), Skip: s.charterFinishBackoff.skip(now),
	})
	if err != nil {
		return 0, fmt.Errorf("list charter auto finish: %w", err)
	}
	total := 0
	for _, id := range ids {
		var done bool
		err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			if s.beforeItem != nil {
				if err := s.beforeItem(id); err != nil {
					return err
				}
			}
			b, err := lockCharter(ctx, tx, id)
			if err != nil {
				return err
			}
			if b.ServiceType != serviceTypeCharter || b.ServiceEndAt.After(cutoff) {
				return nil
			}
			var to, reason string
			switch b.Status {
			case StatusInService:
				to, reason = StatusCompleted, reasonCharterAutoComplete(cfg.CharterAutoCompleteHours)
			case StatusConfirmed:
				to, reason = StatusNoShow, reasonCharterAutoNoShow
			default:
				return nil
			}
			done = true
			return s.endService(ctx, tx, b, to, actorSystem, nil, &reason, now)
		})
		if err != nil {
			s.itemFailed(&s.charterFinishBackoff, "booking: finish charter failed", "booking_id", id, now, err)
			continue
		}
		s.charterFinishBackoff.ok(id)
		if done {
			total++
		}
	}
	return total, nil
}
