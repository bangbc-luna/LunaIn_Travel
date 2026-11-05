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
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

var (
	ErrCheckInWrongDate  = httpx.NewError(http.StatusUnprocessableEntity, "checkin_wrong_date", "Vé này không dùng cho ngày hôm nay")
	ErrCheckInNotAllowed = httpx.NewError(http.StatusConflict, "checkin_not_allowed", "Đơn đặt chỗ này không ở trạng thái có thể check-in")
	ErrInvalidCheckIn    = httpx.NewError(http.StatusUnprocessableEntity, "invalid_checkin", "Cần gửi đúng một trong hai: qr_token hoặc code")
)

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
// ErrCheckInNotAllowed. Checking in again returns Already with the first
// check-in time.
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
		if b.Status != StatusConfirmed || b.ServiceType != serviceTypeAttraction {
			return ErrCheckInNotAllowed
		}
		now := s.clock.Now()
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

// checkInInput is the POST /v1/partner/check-ins body: exactly one of
// qr_token and code.
type checkInInput struct {
	QRToken *string `json:"qr_token"`
	Code    *string `json:"code"`
}
