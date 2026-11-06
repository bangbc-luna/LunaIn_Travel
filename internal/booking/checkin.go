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

var (
	ErrCheckInWrongDate  = httpx.NewError(http.StatusUnprocessableEntity, "checkin_wrong_date", "Vé này không dùng cho ngày hôm nay")
	ErrCheckInNotAllowed = httpx.NewError(http.StatusConflict, "checkin_not_allowed", "Đơn đặt chỗ này không ở trạng thái có thể check-in")
	ErrInvalidCheckIn    = httpx.NewError(http.StatusUnprocessableEntity, "invalid_checkin", "Cần gửi đúng một trong hai: qr_token hoặc code")
	// ErrCheckInOutsideWindow is the code of a ride check-in outside
	// departure ± ride_checkin_window_minutes; the message names the window
	// (checkInOutsideWindow).
	ErrCheckInOutsideWindow = httpx.NewError(http.StatusUnprocessableEntity, "checkin_outside_window", "Chỉ check-in được trong khoảng quanh giờ khởi hành")
	// ErrTripCancelled: the booking's trip was cancelled.
	ErrTripCancelled = httpx.NewError(http.StatusConflict, "trip_cancelled", "Chuyến xe này đã bị hủy, không thể check-in")
)

func checkInOutsideWindow(minutes int) error {
	return httpx.NewError(ErrCheckInOutsideWindow.Status, ErrCheckInOutsideWindow.Code,
		fmt.Sprintf("Chỉ check-in được trong khoảng ±%d phút quanh giờ khởi hành", minutes))
}

// CheckInResult is the outcome of a check-in. Already: the booking had been
// checked in before (nothing changed); CheckedInAt is then the first time.
type CheckInResult struct {
	Already     bool        `json:"already"`
	CheckedInAt time.Time   `json:"checked_in_at"`
	Booking     BookingView `json:"booking"`
}

// CheckIn checks in the partner's booking with this QR token. A token of
// another partner's booking (or unknown) is ErrBookingNotFound (Review Focus
// 4). An attraction ticket is used up at check-in: the confirmed booking
// becomes completed (checked_in_at = completed_at = now), the change is
// recorded with the partner user as actor and the release is scheduled
// release_delay_days later. It must be the visit date in Vietnam
// (ErrCheckInWrongDate); a booking that is not confirmed is
// ErrCheckInNotAllowed. A scheduled ride booking follows checkInRide instead,
// a self-drive rental checkInRental (the vehicle handover) and a private
// charter checkInCharter (the customer pickup).
// Checking in again returns Already with the first check-in time.
func (s *Service) CheckIn(ctx context.Context, partnerID, userID uuid.UUID, qrToken string) (CheckInResult, error) {
	return s.checkIn(ctx, partnerID, userID, func(q *store.Queries) (store.Booking, error) {
		return q.LockPartnerBookingByQR(ctx, store.LockPartnerBookingByQRParams{QrToken: qrToken, PartnerID: partnerID})
	})
}

// CheckInByCode is CheckIn with the booking code typed in by hand.
func (s *Service) CheckInByCode(ctx context.Context, partnerID, userID uuid.UUID, code string) (CheckInResult, error) {
	return s.checkIn(ctx, partnerID, userID, func(q *store.Queries) (store.Booking, error) {
		return q.LockPartnerBookingByCode(ctx, store.LockPartnerBookingByCodeParams{Code: code, PartnerID: partnerID})
	})
}

func (s *Service) checkIn(ctx context.Context, partnerID, userID uuid.UUID, lock func(*store.Queries) (store.Booking, error)) (CheckInResult, error) {
	var res CheckInResult
	var bookingID uuid.UUID
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		b, err := lock(q)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBookingNotFound
		}
		if err != nil {
			return fmt.Errorf("lock booking: %w", err)
		}
		bookingID = b.ID
		if b.CheckedInAt != nil {
			res = CheckInResult{Already: true, CheckedInAt: *b.CheckedInAt}
			return nil
		}
		now := s.clock.Now()
		switch b.ServiceType {
		case serviceTypeRide:
			var err error
			res, err = s.checkInRide(ctx, tx, q, b, userID, now)
			return err
		case serviceTypeRental:
			var err error
			res, err = s.checkInRental(ctx, tx, q, b, userID, now)
			return err
		case serviceTypeCharter:
			var err error
			res, err = s.checkInCharter(ctx, tx, q, b, userID, now)
			return err
		}
		if b.Status != StatusConfirmed || b.ServiceType != serviceTypeAttraction {
			return ErrCheckInNotAllowed
		}
		d, err := q.GetAttractionDetail(ctx, b.ID)
		if err != nil {
			return fmt.Errorf("get attraction detail: %w", err)
		}
		if d.VisitDate != now.In(clock.VN).Format(dateLayout) {
			return ErrCheckInWrongDate
		}
		if err := q.CompleteCheckIn(ctx, store.CompleteCheckInParams{Now: now, ID: b.ID}); err != nil {
			return fmt.Errorf("complete check-in: %w", err)
		}
		if err := insertHistory(ctx, q, b, StatusCompleted, actorPartner, &userID, nil, now); err != nil {
			return err
		}
		if err := s.scheduleRelease(ctx, tx, b.ID, now); err != nil {
			return err
		}
		res = CheckInResult{CheckedInAt: now}
		return nil
	})
	if err != nil {
		return CheckInResult{}, err
	}
	res.Booking, err = s.GetForPartner(ctx, partnerID, bookingID)
	return res, err
}

// checkInRide checks in a locked, not yet checked-in scheduled ride booking:
// the passenger boarded, so the confirmed booking becomes checked_in
// (checked_in_at = now, history with the partner user as actor). Nothing is
// released here: CompleteRides completes it at service_end_at and schedules
// the release then. A booking whose trip was cancelled is ErrTripCancelled
// (its legs were cancelled with the trip; a leg checked in before is
// answered Already earlier and left to CompleteRides); one not confirmed is
// ErrCheckInNotAllowed; now must be within departure ±
// ride_checkin_window_minutes, both bounds included
// (ErrCheckInOutsideWindow).
//
// The trip is read, not locked: the trip comes before the booking in the
// lock order, and an admin trip cancel locks this booking before changing
// it, so it either sees it checked in or cancels it first.
func (s *Service) checkInRide(ctx context.Context, tx pgx.Tx, q *store.Queries, b store.Booking, userID uuid.UUID, now time.Time) (CheckInResult, error) {
	d, err := q.GetRideDetails(ctx, b.ID)
	if err != nil {
		return CheckInResult{}, fmt.Errorf("get ride details: %w", err)
	}
	st, err := s.listings.TripStatus(ctx, tx, d.TripID)
	if err != nil {
		return CheckInResult{}, err
	}
	if st == listing.TripCancelled {
		return CheckInResult{}, ErrTripCancelled
	}
	if b.Status != StatusConfirmed {
		return CheckInResult{}, ErrCheckInNotAllowed
	}
	cfg, err := s.settings.GetTx(ctx, tx)
	if err != nil {
		return CheckInResult{}, err
	}
	window := time.Duration(cfg.RideCheckinWindowMinutes) * time.Minute
	if now.Before(b.ServiceStartAt.Add(-window)) || now.After(b.ServiceStartAt.Add(window)) {
		return CheckInResult{}, checkInOutsideWindow(cfg.RideCheckinWindowMinutes)
	}
	if err := q.CheckInRide(ctx, store.CheckInRideParams{Now: now, ID: b.ID}); err != nil {
		return CheckInResult{}, fmt.Errorf("check in ride: %w", err)
	}
	if err := insertHistory(ctx, q, b, StatusCheckedIn, actorPartner, &userID, nil, now); err != nil {
		return CheckInResult{}, err
	}
	return CheckInResult{CheckedInAt: now}, nil
}

// checkInInput is the POST /v1/partner/check-ins body: exactly one of
// qr_token and code.
type checkInInput struct {
	QRToken *string `json:"qr_token"`
	Code    *string `json:"code"`
}
