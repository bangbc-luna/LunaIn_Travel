// Package settings owns platform_settings (Super Admin tunable parameters)
// and the holidays catalog.
package settings

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/settings/store"
)

// Business errors. ErrInvalidSettings/ErrInvalidHoliday are sentinels; returned
// errors wrap them with a field-specific message.
var (
	ErrHolidayExists   = httpx.NewError(http.StatusConflict, "holiday_exists", "Ngày lễ này đã tồn tại")
	ErrHolidayNotFound = httpx.NewError(http.StatusNotFound, "holiday_not_found", "Không tìm thấy ngày lễ")
	ErrInvalidHoliday  = httpx.NewError(http.StatusUnprocessableEntity, "invalid_holiday", "Ngày lễ không hợp lệ")
)

func invalidSettings(field, msg string) error {
	return httpx.NewError(http.StatusUnprocessableEntity, "invalid_settings",
		fmt.Sprintf("Cấu hình không hợp lệ: %s %s", field, msg))
}

// Settings mirrors platform_settings. JSON names equal the column names.
type Settings struct {
	HoldMinutes                int   `json:"hold_minutes"`
	HoldGraceMinutes           int   `json:"hold_grace_minutes"`
	RequestResponseHours       int   `json:"request_response_hours"`
	RequestPaymentHours        int   `json:"request_payment_hours"`
	CharterLateConfirmMinHours int   `json:"charter_late_confirm_min_hours"`
	RideCheckinWindowMinutes   int   `json:"ride_checkin_window_minutes"`
	RideNoShowAfterMinutes     int   `json:"ride_no_show_after_minutes"`
	CharterAutoCompleteHours   int   `json:"charter_auto_complete_hours"`
	ReleaseDelayDays           int   `json:"release_delay_days"`
	MinPayoutAmount            int64 `json:"min_payout_amount"`
	RescheduleWindowDays       int   `json:"reschedule_window_days"`
	RideBookingCutoffMinutes   int   `json:"ride_booking_cutoff_minutes"`
}

// Patch is a partial update: nil fields keep their current value.
type Patch struct {
	HoldMinutes                *int   `json:"hold_minutes"`
	HoldGraceMinutes           *int   `json:"hold_grace_minutes"`
	RequestResponseHours       *int   `json:"request_response_hours"`
	RequestPaymentHours        *int   `json:"request_payment_hours"`
	CharterLateConfirmMinHours *int   `json:"charter_late_confirm_min_hours"`
	RideCheckinWindowMinutes   *int   `json:"ride_checkin_window_minutes"`
	RideNoShowAfterMinutes     *int   `json:"ride_no_show_after_minutes"`
	CharterAutoCompleteHours   *int   `json:"charter_auto_complete_hours"`
	ReleaseDelayDays           *int   `json:"release_delay_days"`
	MinPayoutAmount            *int64 `json:"min_payout_amount"`
	RescheduleWindowDays       *int   `json:"reschedule_window_days"`
	RideBookingCutoffMinutes   *int   `json:"ride_booking_cutoff_minutes"`
}

// Holiday is one catalog entry; Date is formatted YYYY-MM-DD.
type Holiday struct {
	Date string `json:"date"`
	Name string `json:"name"`
}

const (
	dateLayout          = "2006-01-02"
	maxHolidayNameRunes = 100
)

// Service reads and updates platform settings and holidays.
type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewService builds a Service.
func NewService(pool *pgxpool.Pool, c clock.Clock) *Service {
	return &Service{pool: pool, clock: c}
}

func fromRow(r store.PlatformSetting) Settings {
	return Settings{
		HoldMinutes:                int(r.HoldMinutes),
		HoldGraceMinutes:           int(r.HoldGraceMinutes),
		RequestResponseHours:       int(r.RequestResponseHours),
		RequestPaymentHours:        int(r.RequestPaymentHours),
		CharterLateConfirmMinHours: int(r.CharterLateConfirmMinHours),
		RideCheckinWindowMinutes:   int(r.RideCheckinWindowMinutes),
		RideNoShowAfterMinutes:     int(r.RideNoShowAfterMinutes),
		CharterAutoCompleteHours:   int(r.CharterAutoCompleteHours),
		ReleaseDelayDays:           int(r.ReleaseDelayDays),
		MinPayoutAmount:            r.MinPayoutAmount,
		RescheduleWindowDays:       int(r.RescheduleWindowDays),
		RideBookingCutoffMinutes:   int(r.RideBookingCutoffMinutes),
	}
}

// Get returns the current settings.
func (s *Service) Get(ctx context.Context) (Settings, error) {
	r, err := store.New(s.pool).GetSettings(ctx)
	if err != nil {
		return Settings{}, fmt.Errorf("get settings: %w", err)
	}
	return fromRow(r), nil
}

// GetTx returns the current settings inside tx (used when creating bookings).
func (s *Service) GetTx(ctx context.Context, tx pgx.Tx) (Settings, error) {
	r, err := store.New(tx).GetSettings(ctx)
	if err != nil {
		return Settings{}, fmt.Errorf("get settings: %w", err)
	}
	return fromRow(r), nil
}

// apply returns cur with the non-nil fields of p overwritten.
func (p Patch) apply(cur Settings) Settings {
	setInt := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	setInt(&cur.HoldMinutes, p.HoldMinutes)
	setInt(&cur.HoldGraceMinutes, p.HoldGraceMinutes)
	setInt(&cur.RequestResponseHours, p.RequestResponseHours)
	setInt(&cur.RequestPaymentHours, p.RequestPaymentHours)
	setInt(&cur.CharterLateConfirmMinHours, p.CharterLateConfirmMinHours)
	setInt(&cur.RideCheckinWindowMinutes, p.RideCheckinWindowMinutes)
	setInt(&cur.RideNoShowAfterMinutes, p.RideNoShowAfterMinutes)
	setInt(&cur.CharterAutoCompleteHours, p.CharterAutoCompleteHours)
	setInt(&cur.ReleaseDelayDays, p.ReleaseDelayDays)
	setInt(&cur.RescheduleWindowDays, p.RescheduleWindowDays)
	setInt(&cur.RideBookingCutoffMinutes, p.RideBookingCutoffMinutes)
	if p.MinPayoutAmount != nil {
		cur.MinPayoutAmount = *p.MinPayoutAmount
	}
	return cur
}

const maxInt32 = 1<<31 - 1

// validate mirrors the CHECK constraints of platform_settings (and the int4
// range of the columns) so callers get a precise message instead of a DB error.
func (v Settings) validate() error {
	positive := []struct {
		name string
		val  int
	}{
		{"hold_minutes", v.HoldMinutes},
		{"request_response_hours", v.RequestResponseHours},
		{"request_payment_hours", v.RequestPaymentHours},
		{"ride_checkin_window_minutes", v.RideCheckinWindowMinutes},
		{"charter_auto_complete_hours", v.CharterAutoCompleteHours},
		{"reschedule_window_days", v.RescheduleWindowDays},
	}
	nonNegative := []struct {
		name string
		val  int
	}{
		{"hold_grace_minutes", v.HoldGraceMinutes},
		{"charter_late_confirm_min_hours", v.CharterLateConfirmMinHours},
		{"ride_no_show_after_minutes", v.RideNoShowAfterMinutes},
		{"release_delay_days", v.ReleaseDelayDays},
		{"ride_booking_cutoff_minutes", v.RideBookingCutoffMinutes},
	}
	for _, f := range positive {
		if f.val <= 0 {
			return invalidSettings(f.name, "phải lớn hơn 0")
		}
		if f.val > maxInt32 {
			return invalidSettings(f.name, "quá lớn")
		}
	}
	for _, f := range nonNegative {
		if f.val < 0 {
			return invalidSettings(f.name, "không được âm")
		}
		if f.val > maxInt32 {
			return invalidSettings(f.name, "quá lớn")
		}
	}
	if v.MinPayoutAmount < 0 {
		return invalidSettings("min_payout_amount", "không được âm")
	}
	return nil
}

// Update applies p, validates the result, and audits the change. The settings
// row is locked FOR UPDATE so concurrent patches serialize.
func (s *Service) Update(ctx context.Context, actor uuid.UUID, p Patch) (Settings, error) {
	var after Settings
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		row, err := q.GetSettingsForUpdate(ctx)
		if err != nil {
			return fmt.Errorf("lock settings: %w", err)
		}
		before := fromRow(row)
		after = p.apply(before)
		if err := after.validate(); err != nil {
			return err
		}
		now := s.clock.Now()
		if err := q.UpdateSettings(ctx, store.UpdateSettingsParams{
			HoldMinutes:                int32(after.HoldMinutes),
			HoldGraceMinutes:           int32(after.HoldGraceMinutes),
			RequestResponseHours:       int32(after.RequestResponseHours),
			RequestPaymentHours:        int32(after.RequestPaymentHours),
			CharterLateConfirmMinHours: int32(after.CharterLateConfirmMinHours),
			RideCheckinWindowMinutes:   int32(after.RideCheckinWindowMinutes),
			RideNoShowAfterMinutes:     int32(after.RideNoShowAfterMinutes),
			CharterAutoCompleteHours:   int32(after.CharterAutoCompleteHours),
			ReleaseDelayDays:           int32(after.ReleaseDelayDays),
			MinPayoutAmount:            after.MinPayoutAmount,
			RescheduleWindowDays:       int32(after.RescheduleWindowDays),
			RideBookingCutoffMinutes:   int32(after.RideBookingCutoffMinutes),
			UpdatedBy:                  pgtype.UUID{Bytes: actor, Valid: true},
			Now:                        now,
		}); err != nil {
			return fmt.Errorf("update settings: %w", err)
		}
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor,
			Action:      "platform_settings.update",
			TargetKind:  "platform_settings",
			Before:      before,
			After:       after,
		})
	})
	if err != nil {
		return Settings{}, err
	}
	return after, nil
}

// pgDate returns the calendar date of d in Vietnam time (clock.VN). A date
// parsed from YYYY-MM-DD (UTC midnight) keeps its calendar date.
func pgDate(d time.Time) pgtype.Date {
	d = d.In(clock.VN)
	return pgtype.Date{Time: time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
}

// ListHolidays returns the holidays of a calendar year, ordered by date.
func (s *Service) ListHolidays(ctx context.Context, year int) ([]Holiday, error) {
	from := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	rows, err := store.New(s.pool).ListHolidaysByYear(ctx, store.ListHolidaysByYearParams{
		FromDate: pgDate(from),
		ToDate:   pgDate(from.AddDate(1, 0, 0)),
	})
	if err != nil {
		return nil, fmt.Errorf("list holidays: %w", err)
	}
	out := make([]Holiday, 0, len(rows))
	for _, r := range rows {
		out = append(out, Holiday{Date: r.Date.Time.Format(dateLayout), Name: r.Name})
	}
	return out, nil
}

// AddHoliday inserts a holiday; a duplicate date is ErrHolidayExists and a
// blank name or one over 100 characters (after trimming) is ErrInvalidHoliday.
func (s *Service) AddHoliday(ctx context.Context, actor uuid.UUID, date time.Time, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxHolidayNameRunes {
		return ErrInvalidHoliday
	}
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		now := s.clock.Now()
		n, err := store.New(tx).InsertHoliday(ctx, store.InsertHolidayParams{Date: pgDate(date), Name: name, Now: now})
		if err != nil {
			return fmt.Errorf("insert holiday: %w", err)
		}
		if n == 0 {
			return ErrHolidayExists
		}
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor,
			Action:      "holiday.create",
			TargetKind:  "holiday",
			After:       Holiday{Date: pgDate(date).Time.Format(dateLayout), Name: name},
		})
	})
}

// RemoveHoliday hard-deletes a holiday (a configuration catalog, not business
// data); a missing date is ErrHolidayNotFound.
func (s *Service) RemoveHoliday(ctx context.Context, actor uuid.UUID, date time.Time) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		name, err := q.GetHolidayName(ctx, pgDate(date))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrHolidayNotFound
		}
		if err != nil {
			return fmt.Errorf("get holiday: %w", err)
		}
		n, err := q.DeleteHoliday(ctx, pgDate(date))
		if err != nil {
			return fmt.Errorf("delete holiday: %w", err)
		}
		if n == 0 {
			return ErrHolidayNotFound
		}
		return audit.Record(ctx, tx, s.clock.Now(), audit.Entry{
			ActorUserID: actor,
			Action:      "holiday.delete",
			TargetKind:  "holiday",
			Before:      Holiday{Date: pgDate(date).Time.Format(dateLayout), Name: name},
		})
	})
}

// IsHoliday reports whether the Vietnam calendar date of d (d.In(clock.VN))
// is in the holidays catalog, so 2026-09-01T17:30Z is 2026-09-02 in Vietnam.
func (s *Service) IsHoliday(ctx context.Context, tx pgx.Tx, d time.Time) (bool, error) {
	ok, err := store.New(tx).HolidayExists(ctx, pgDate(d))
	if err != nil {
		return false, fmt.Errorf("holiday exists: %w", err)
	}
	return ok, nil
}
