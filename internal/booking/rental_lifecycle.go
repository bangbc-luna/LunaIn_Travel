package booking

import (
	"context"
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

// Self-drive rental lifecycle after payment (spec §6.2, user decisions 3 and
// 4): the partner hands the vehicle over by scanning the QR (confirmed ->
// in_service), marks it returned at any time, early or late (in_service ->
// completed), or marks a customer who never came (confirmed -> no_show).
// FinishRentals ends what the partner forgot. Leaving in_service or
// confirmed stops holding the vehicle at once (holds_vehicle = false; the
// booked period is never shrunk) and schedules the partner's release
// release_delay_days later; an early return is not refunded.

const (
	// rentalHandoverEarly: the vehicle can be handed over from this long
	// before pickup_at until return_at (excluded).
	rentalHandoverEarly = 2 * time.Hour
	// rentalAutoCompleteAfter: a handed-over rental never marked returned is
	// completed this long after its scheduled return.
	rentalAutoCompleteAfter = 24 * time.Hour

	reasonRentalAutoComplete = "tự hoàn thành sau 24 giờ quá giờ trả"
	reasonRentalAutoNoShow   = "khách không nhận xe trước giờ trả"
)

var (
	// ErrHandoverOutsideWindow: the QR was scanned outside
	// [pickup_at − 2h, return_at).
	ErrHandoverOutsideWindow = httpx.NewError(http.StatusUnprocessableEntity, "handover_outside_window",
		"Chỉ giao xe được từ 2 giờ trước giờ nhận đến trước giờ trả")
	// ErrNoShowTooEarly: a no-show (rental or charter) is marked before
	// pickup_at.
	ErrNoShowTooEarly = httpx.NewError(http.StatusConflict, "no_show_too_early",
		"Chỉ đánh dấu khách không đến sau giờ đón/nhận xe")

	errNotRental = httpx.NewError(ErrInvalidBookingState.Status, ErrInvalidBookingState.Code,
		"Thao tác này chỉ dành cho đơn thuê xe tự lái")
	errNoShowState = httpx.NewError(ErrInvalidBookingState.Status, ErrInvalidBookingState.Code,
		"Chỉ đánh dấu khách không đến cho đơn thuê xe đã xác nhận và chưa giao xe")
	errReturnState = httpx.NewError(ErrInvalidBookingState.Status, ErrInvalidBookingState.Code,
		"Chỉ nhận lại xe cho đơn thuê xe đang trong thời gian thuê")
)

// checkInRental hands over the vehicle of a locked, not yet checked-in
// rental booking: confirmed (else ErrCheckInNotAllowed) and now in
// [pickup_at − rentalHandoverEarly, return_at) (else
// ErrHandoverOutsideWindow), it becomes in_service with in_service_at =
// checked_in_at = now (a second scan is answered Already), history with the
// partner user as actor. Nothing is released and the vehicle stays held:
// MarkRentalReturned or FinishRentals ends it. The model is not locked: the
// hold does not change.
func (s *Service) checkInRental(ctx context.Context, _ pgx.Tx, q *store.Queries, b store.Booking, userID uuid.UUID, now time.Time) (CheckInResult, error) {
	if b.Status != StatusConfirmed {
		return CheckInResult{}, ErrCheckInNotAllowed
	}
	if now.Before(b.ServiceStartAt.Add(-rentalHandoverEarly)) || !now.Before(b.ServiceEndAt) {
		return CheckInResult{}, ErrHandoverOutsideWindow
	}
	if err := q.HandOverRental(ctx, store.HandOverRentalParams{Now: now, ID: b.ID}); err != nil {
		return CheckInResult{}, fmt.Errorf("hand over rental: %w", err)
	}
	if err := insertHistory(ctx, q, b, StatusInService, actorPartner, &userID, nil, now); err != nil {
		return CheckInResult{}, err
	}
	return CheckInResult{CheckedInAt: now}, nil
}

// partnerRental reads (unlocked) a booking of partnerID that must be a
// self-drive rental: another partner's or unknown is ErrBookingNotFound,
// another service type errNotRental (409 invalid_booking_state).
func (s *Service) partnerRental(ctx context.Context, partnerID, bookingID uuid.UUID) error {
	b, err := s.partnerBooking(ctx, partnerID, bookingID)
	if err != nil {
		return err
	}
	if b.ServiceType != serviceTypeRental {
		return errNotRental
	}
	return nil
}

// lockRental locks, in tx, the vehicle model of a rental booking and then
// the booking (DB §1.1: rental_vehicle_models before bookings) and returns
// the booking as locked. The model id is read unlocked: it never changes.
func (s *Service) lockRental(ctx context.Context, tx pgx.Tx, bookingID uuid.UUID) (store.Booking, error) {
	q := store.New(tx)
	d, err := q.GetRentalDetails(ctx, bookingID)
	if err != nil {
		return store.Booking{}, fmt.Errorf("get rental details: %w", err)
	}
	if _, err := s.listings.LockRentalModels(ctx, tx, []uuid.UUID{d.VehicleModelID}); err != nil {
		return store.Booking{}, err
	}
	b, err := q.LockBooking(ctx, bookingID)
	if err != nil {
		return store.Booking{}, fmt.Errorf("lock booking: %w", err)
	}
	return b, nil
}

// endService moves a locked self-drive rental (whose model the tx holds) or
// private charter booking to completed (from in_service) or no_show (from
// confirmed): a rental's vehicle is no longer held (a charter holds
// nothing) and the release is scheduled release_delay_days after now.
// actorUser is nil for the system.
func (s *Service) endService(ctx context.Context, tx pgx.Tx, b store.Booking, to, actorKind string, actorUser *uuid.UUID, reason *string, now time.Time) error {
	q := store.New(tx)
	switch to {
	case StatusCompleted:
		if err := q.CompleteRide(ctx, store.CompleteRideParams{Now: now, ID: b.ID}); err != nil {
			return fmt.Errorf("complete service: %w", err)
		}
	case StatusNoShow:
		if err := q.MarkNoShow(ctx, store.MarkNoShowParams{Now: now, ID: b.ID}); err != nil {
			return fmt.Errorf("mark no-show: %w", err)
		}
	default:
		return fmt.Errorf("booking: cannot end service of %s as %s", b.ID, to)
	}
	if err := insertHistory(ctx, q, b, to, actorKind, actorUser, reason, now); err != nil {
		return err
	}
	if err := s.releaseHold(ctx, tx, b); err != nil {
		return err
	}
	return s.scheduleRelease(ctx, tx, b.ID, now)
}

// MarkRentalNoShow marks, as userID of partnerID, that the customer never
// came for a confirmed rental (not handed over): from pickup_at on (else
// ErrNoShowTooEarly), it becomes no_show, the vehicle is freed and the
// partner is paid release_delay_days later; nothing is refunded. Another
// partner's booking is ErrBookingNotFound; another service type or status
// is 409 invalid_booking_state.
func (s *Service) MarkRentalNoShow(ctx context.Context, partnerID, userID, bookingID uuid.UUID) (BookingView, error) {
	if err := s.partnerRental(ctx, partnerID, bookingID); err != nil {
		return BookingView{}, err
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		b, err := s.lockRental(ctx, tx, bookingID)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		if b.Status != StatusConfirmed {
			return errNoShowState
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

// MarkRentalReturned records, as userID of partnerID, that the vehicle of
// an in_service rental came back, at any time (early: no refund; late:
// settled at the shop): it becomes completed, the vehicle is freed at once
// and the partner is paid release_delay_days later. An already completed
// booking is returned unchanged; another status is 409
// invalid_booking_state; another partner's booking ErrBookingNotFound.
func (s *Service) MarkRentalReturned(ctx context.Context, partnerID, userID, bookingID uuid.UUID) (BookingView, error) {
	if err := s.partnerRental(ctx, partnerID, bookingID); err != nil {
		return BookingView{}, err
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		b, err := s.lockRental(ctx, tx, bookingID)
		if err != nil {
			return err
		}
		switch b.Status {
		case StatusCompleted:
			return nil
		case StatusInService:
			return s.endService(ctx, tx, b, StatusCompleted, actorPartner, &userID, nil, s.clock.Now())
		}
		return errReturnState
	})
	if err != nil {
		return BookingView{}, err
	}
	return s.GetForPartner(ctx, partnerID, bookingID)
}

// FinishRentalsArgs is the periodic rental auto-finish job.
type FinishRentalsArgs struct{}

func (FinishRentalsArgs) Kind() string { return "booking_finish_rentals" }

// FinishRentalsWorker runs Service.FinishRentals.
type FinishRentalsWorker struct {
	river.WorkerDefaults[FinishRentalsArgs]
	Service *Service
}

func (w *FinishRentalsWorker) Work(ctx context.Context, _ *river.Job[FinishRentalsArgs]) error {
	_, err := w.Service.FinishRentals(ctx)
	return err
}

// FinishRentals ends the rentals the partner did not (user decision 4), so
// the partner's money is never stuck: a confirmed rental never handed over
// by its scheduled return (service_end_at <= now) becomes no_show, an
// in_service rental not marked returned rentalAutoCompleteAfter after it
// (service_end_at <= now − 24h) becomes completed; both free the vehicle and
// schedule the release release_delay_days after now, history actor system.
// Two lists, one backoff; each booking in its own tx, model then booking
// locked and the state re-checked, so a partner's click that came first
// wins. It returns how many it ended.
func (s *Service) FinishRentals(ctx context.Context) (int, error) {
	now := s.clock.Now()
	cutoff := now.Add(-rentalAutoCompleteAfter)
	q := store.New(s.pool)
	noShows, err := q.ListRentalAutoNoShow(ctx, store.ListRentalAutoNoShowParams{Now: now, Lim: s.batch(), Skip: s.finishBackoff.skip(now)})
	if err != nil {
		return 0, fmt.Errorf("list rental auto no-show: %w", err)
	}
	completes, err := q.ListRentalAutoComplete(ctx, store.ListRentalAutoCompleteParams{Cutoff: cutoff, Lim: s.batch(), Skip: s.finishBackoff.skip(now)})
	if err != nil {
		return 0, fmt.Errorf("list rental auto complete: %w", err)
	}
	total := 0
	for _, id := range append(noShows, completes...) {
		var done bool
		err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			if s.beforeItem != nil {
				if err := s.beforeItem(id); err != nil {
					return err
				}
			}
			b, err := s.lockRental(ctx, tx, id)
			if err != nil {
				return err
			}
			var to, reason string
			switch {
			case b.ServiceType != serviceTypeRental:
				return nil
			case b.Status == StatusConfirmed && !b.ServiceEndAt.After(now):
				to, reason = StatusNoShow, reasonRentalAutoNoShow
			case b.Status == StatusInService && !b.ServiceEndAt.After(cutoff):
				to, reason = StatusCompleted, reasonRentalAutoComplete
			default:
				return nil
			}
			done = true
			return s.endService(ctx, tx, b, to, actorSystem, nil, &reason, now)
		})
		if err != nil {
			s.itemFailed(&s.finishBackoff, "booking: finish rental failed", "booking_id", id, now, err)
			continue
		}
		s.finishBackoff.ok(id)
		if done {
			total++
		}
	}
	return total, nil
}
