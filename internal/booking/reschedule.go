package booking

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// ErrRescheduleNotAllowed is the code of a booking that cannot be
// rescheduled; the message says why (rescheduleBlock).
var ErrRescheduleNotAllowed = httpx.NewError(http.StatusConflict, "reschedule_not_allowed", "Không thể đổi lịch đơn đặt chỗ này")

// RescheduleInput is the POST /v1/bookings/{id}/reschedule body.
type RescheduleInput struct {
	VisitDate  string    `json:"visit_date"`
	TimeSlotID uuid.UUID `json:"time_slot_id"`
}

// rescheduleBlock is the reschedule predicate shared by Reschedule and
// BookingView.can_reschedule (spec §6.4): it returns why the booking cannot
// be rescheduled at now, or "" when it can. rescheduled: a
// booking_reschedules row exists; windowDays: settings.reschedule_window_days.
func rescheduleBlock(b store.Booking, rescheduled bool, windowDays int, now time.Time) string {
	switch {
	case b.ServiceType != serviceTypeAttraction:
		return "Chỉ vé tham quan mới đổi lịch được"
	case b.Status != StatusConfirmed:
		return "Chỉ đổi lịch được đơn đặt chỗ đã xác nhận"
	case rescheduled:
		return "Mỗi đơn đặt chỗ chỉ được đổi lịch một lần"
	case b.ConfirmedAt == nil || now.After(b.ConfirmedAt.Add(time.Duration(windowDays)*24*time.Hour)):
		return fmt.Sprintf("Chỉ đổi lịch được trong %d ngày kể từ lúc thanh toán", windowDays)
	case !b.OriginalServiceStartAt.After(now):
		return "Ngày tham quan ban đầu đã qua, không thể đổi lịch"
	}
	return ""
}

// rescheduleState is rescheduleBlock after reading, through q (tx or pool),
// whether the booking was already rescheduled.
func (s *Service) rescheduleState(ctx context.Context, q *store.Queries, b store.Booking, windowDays int, now time.Time) (string, error) {
	if b.Status != StatusConfirmed {
		return rescheduleBlock(b, false, windowDays, now), nil
	}
	done, err := q.HasReschedule(ctx, b.ID)
	if err != nil {
		return "", fmt.Errorf("has reschedule: %w", err)
	}
	return rescheduleBlock(b, done, windowDays, now), nil
}

func rescheduleNotAllowed(msg string) error {
	return httpx.NewError(ErrRescheduleNotAllowed.Status, ErrRescheduleNotAllowed.Code, msg)
}

// Reschedule moves the customer's confirmed attraction booking to slotID on
// visitDate, once (spec §6.4). Another customer's booking is
// ErrBookingNotFound; a booking that cannot be rescheduled (rescheduleBlock)
// is ErrRescheduleNotAllowed; a new date in the past, a slot not offered on
// it (closed date, inactive slot, weekday) or already started is 422. The
// listing does not have to be bookable any more (BookableListing is not
// called): an existing booking is still served. Price and
// original_service_start_at are unchanged; service_start_at/end_at and the
// attraction details follow the new slot; a booking_reschedules row and the
// history record the move.
//
// Lock order: the listing FOR SHARE (ShareListingLock), the slot checks
// (reads), then the booking FOR UPDATE, re-checked.
func (s *Service) Reschedule(ctx context.Context, customerID, bookingID uuid.UUID, visitDate string, slotID uuid.UUID) (BookingView, error) {
	b, err := store.New(s.pool).GetBooking(ctx, bookingID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && b.CustomerID != customerID) {
		return BookingView{}, ErrBookingNotFound
	}
	if err != nil {
		return BookingView{}, fmt.Errorf("get booking: %w", err)
	}
	day, err := time.ParseInLocation(dateLayout, visitDate, clock.VN)
	if err != nil {
		return BookingView{}, invalidBooking("visit_date phải có dạng YYYY-MM-DD")
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		now := s.clock.Now()
		cfg, err := s.settings.GetTx(ctx, tx)
		if err != nil {
			return err
		}
		// Unlocked first: a booking that cannot move is 409 before any
		// date or slot error.
		if msg, err := s.rescheduleState(ctx, q, b, cfg.RescheduleWindowDays, now); err != nil {
			return err
		} else if msg != "" {
			return rescheduleNotAllowed(msg)
		}
		if day.Before(startOfDayVN(now)) {
			return ErrVisitDatePast
		}
		// Listing FOR SHARE before reading the schedule: a partner closing
		// the date or changing the slot holds it FOR NO KEY UPDATE while the
		// booking guard counts, so the two serialize (listing.guardDate).
		if err := s.listings.ShareListingLock(ctx, tx, b.ListingID); err != nil {
			return err
		}
		slots, err := s.listings.SlotsForDate(ctx, tx, b.ListingID, day)
		if err != nil {
			return err
		}
		var slot *listing.TimeSlot
		for i := range slots {
			if slots[i].ID == slotID {
				slot = &slots[i]
			}
		}
		if slot == nil {
			return ErrSlotUnavailable
		}
		startAt, err := slot.StartAt(day)
		if err != nil {
			return err
		}
		endAt := attractionServiceEnd(day)
		if !startAt.After(now) {
			return ErrSlotStarted
		}

		locked, err := q.LockBooking(ctx, b.ID)
		if err != nil {
			return fmt.Errorf("lock booking: %w", err)
		}
		if msg, err := s.rescheduleState(ctx, q, locked, cfg.RescheduleWindowDays, now); err != nil {
			return err
		} else if msg != "" {
			return rescheduleNotAllowed(msg)
		}
		from, err := q.GetAttractionDetail(ctx, locked.ID)
		if err != nil {
			return fmt.Errorf("get attraction detail: %w", err)
		}
		to := day.Format(dateLayout)
		if from.VisitDate == to && from.TimeSlotID == slot.ID {
			return invalidBooking("Lịch mới trùng với lịch hiện tại")
		}
		if err := q.RescheduleBooking(ctx, store.RescheduleBookingParams{
			ServiceStartAt: startAt, ServiceEndAt: endAt, Now: now, ID: locked.ID,
		}); err != nil {
			return fmt.Errorf("reschedule booking: %w", err)
		}
		if err := q.UpdateAttractionDetail(ctx, store.UpdateAttractionDetailParams{
			VisitDate: to, TimeSlotID: slot.ID, SlotStart: slot.StartTime, SlotEnd: slot.EndTime,
			Now: now, BookingID: locked.ID,
		}); err != nil {
			return fmt.Errorf("update attraction detail: %w", err)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if err := q.InsertReschedule(ctx, store.InsertRescheduleParams{
			ID: id, BookingID: locked.ID, ListingID: locked.ListingID,
			FromVisitDate: from.VisitDate, FromSlotID: from.TimeSlotID, ToVisitDate: to, ToSlotID: slot.ID,
			CreatedBy: customerID, Now: now,
		}); err != nil {
			return fmt.Errorf("insert reschedule: %w", err)
		}
		const when = "15:04 02/01/2006"
		reason := "đổi lịch từ " + locked.ServiceStartAt.In(clock.VN).Format(when) + " sang " + startAt.In(clock.VN).Format(when)
		return insertHistory(ctx, q, locked, StatusConfirmed, actorCustomer, &customerID, &reason, now)
	})
	if err != nil {
		return BookingView{}, err
	}
	return s.GetMine(ctx, customerID, bookingID)
}
