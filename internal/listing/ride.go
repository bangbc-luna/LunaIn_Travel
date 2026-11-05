package listing

import (
	"bytes"
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

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	rideServiceType = "scheduled_ride"

	maxRidePointName  = 200
	maxRideText       = 500
	maxRideDuration   = 4320 // minutes (3 days)
	maxRideSeats      = 80
	minRideSeatPrice  = 1_000
	maxRideSeatPrice  = 100_000_000
	tripHorizonDays   = 60
	scheduleCancelMsg = "Lịch chạy đã thay đổi"

	actionRideRoute    = "ride_route"
	actionRideSchedule = "ride_schedule"
)

var (
	ErrNotRideListing      = httpx.NewError(http.StatusUnprocessableEntity, "wrong_service_type", "Thao tác này chỉ áp dụng cho Listing xe tuyến")
	ErrInvalidRide         = httpx.NewError(http.StatusUnprocessableEntity, "invalid_ride", "Thông tin tuyến hoặc lịch chạy không hợp lệ")
	ErrRouteRequired       = httpx.NewError(http.StatusUnprocessableEntity, "route_required", "Cần khai báo tuyến (điểm đi, điểm đến) trước khi tạo lịch chạy")
	ErrOverlappingSchedule = httpx.NewError(http.StatusUnprocessableEntity, "overlapping_schedule", "Lịch chạy trùng giờ khởi hành, ngày trong tuần và thời gian hiệu lực với một lịch đang hoạt động khác")
)

func invalidRide(msg string) error { return withMessage(ErrInvalidRide, msg) }

// asInvalidRide relabels a validation error of the shared helpers
// (normalizeText, minutes) as ErrInvalidRide, keeping its message.
func asInvalidRide(err error) error {
	var fe *fieldError
	if errors.As(err, &fe) {
		return invalidRide(fe.app.Message)
	}
	return err
}

// RidePoint is the origin or the destination of a route.
type RidePoint struct {
	ProvinceCode string `json:"province_code"`
	WardCode     string `json:"ward_code"`
	Name         string `json:"name"`
	Address      string `json:"address"`
}

// RideRouteInput is the one-way route of a scheduled_ride listing.
type RideRouteInput struct {
	Origin             RidePoint `json:"origin"`
	Dest               RidePoint `json:"dest"`
	DurationMinutes    int       `json:"duration_minutes"`
	VehicleDescription string    `json:"vehicle_description"`
}

// RideRoute is a stored route.
type RideRoute struct {
	ListingID uuid.UUID `json:"listing_id"`
	RideRouteInput
}

// RideSchedule is a recurring departure. DepartureTime is "HH:MM" (Vietnam
// time); Weekdays is a bitmask (bit 0 = Monday ... bit 6 = Sunday); dates are
// YYYY-MM-DD Vietnam calendar dates, ValidTo nil meaning open-ended.
type RideSchedule struct {
	ID            uuid.UUID `json:"id"`
	ListingID     uuid.UUID `json:"listing_id"`
	DepartureTime string    `json:"departure_time"`
	Weekdays      int       `json:"weekdays"`
	SeatCapacity  int       `json:"seat_capacity"`
	PricePerSeat  int64     `json:"price_per_seat"`
	ValidFrom     string    `json:"valid_from"`
	ValidTo       *string   `json:"valid_to"`
	Active        bool      `json:"active"`
}

// RideScheduleInput creates a schedule (always active).
type RideScheduleInput struct {
	DepartureTime string  `json:"departure_time"`
	Weekdays      int     `json:"weekdays"`
	SeatCapacity  int     `json:"seat_capacity"`
	PricePerSeat  int64   `json:"price_per_seat"`
	ValidFrom     string  `json:"valid_from"`
	ValidTo       *string `json:"valid_to"`
}

// RideSchedulePatch updates a schedule; nil fields are left unchanged.
// ClearValidTo makes the schedule open-ended. In JSON, "valid_to": null
// clears it and a missing valid_to keeps it; departure_time, weekdays and
// valid_from cannot change (create a new schedule instead) and are rejected
// with 422 when present.
type RideSchedulePatch struct {
	SeatCapacity *int
	PricePerSeat *int64
	ValidTo      *string
	ClearValidTo bool
	Active       *bool

	// immutable lists the fixed fields present in the decoded JSON body.
	immutable []string
}

// UnmarshalJSON decodes the PATCH body; unknown fields are an error (400).
func (p *RideSchedulePatch) UnmarshalJSON(b []byte) error {
	var raw struct {
		SeatCapacity  *int            `json:"seat_capacity"`
		PricePerSeat  *int64          `json:"price_per_seat"`
		ValidTo       json.RawMessage `json:"valid_to"`
		Active        *bool           `json:"active"`
		DepartureTime json.RawMessage `json:"departure_time"`
		Weekdays      json.RawMessage `json:"weekdays"`
		ValidFrom     json.RawMessage `json:"valid_from"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	out := RideSchedulePatch{SeatCapacity: raw.SeatCapacity, PricePerSeat: raw.PricePerSeat, Active: raw.Active}
	if len(raw.ValidTo) > 0 {
		if string(raw.ValidTo) == "null" {
			out.ClearValidTo = true
		} else {
			var s string
			if err := json.Unmarshal(raw.ValidTo, &s); err != nil {
				return err
			}
			out.ValidTo = &s
		}
	}
	for _, f := range []struct {
		name string
		v    json.RawMessage
	}{{"departure_time", raw.DepartureTime}, {"weekdays", raw.Weekdays}, {"valid_from", raw.ValidFrom}} {
		if len(f.v) > 0 {
			out.immutable = append(out.immutable, f.name)
		}
	}
	*p = out
	return nil
}

// RideDetail is the route (nil until set) and the schedules of a listing.
type RideDetail struct {
	Route     *RideRoute     `json:"route"`
	Schedules []RideSchedule `json:"schedules"`
}

func routeFromRow(r store.RideRoute) RideRoute {
	return RideRoute{ListingID: r.ListingID, RideRouteInput: RideRouteInput{
		Origin:             RidePoint{r.OriginProvinceCode, r.OriginWardCode, r.OriginName, r.OriginAddress},
		Dest:               RidePoint{r.DestProvinceCode, r.DestWardCode, r.DestName, r.DestAddress},
		DurationMinutes:    int(r.DurationMinutes),
		VehicleDescription: r.VehicleDescription,
	}}
}

func dateText(d pgtype.Date) *string {
	if !d.Valid {
		return nil
	}
	s := d.Time.Format(dateLayout)
	return &s
}

func scheduleFromRow(r store.RideSchedule) RideSchedule {
	return RideSchedule{
		ID: r.ID, ListingID: r.ListingID, DepartureTime: clockText(r.DepartureTime), Weekdays: int(r.Weekdays),
		SeatCapacity: int(r.SeatCapacity), PricePerSeat: r.PricePerSeat,
		ValidFrom: r.ValidFrom.Time.Format(dateLayout), ValidTo: dateText(r.ValidTo), Active: r.Active,
	}
}

// lockRide runs the checks every ride write starts with, in the lock order of
// DB §1.1: partner (FOR SHARE) -> listing (FOR NO KEY UPDATE). The listing
// must be a scheduled_ride.
func (s *Service) lockRide(ctx context.Context, tx pgx.Tx, partnerID, id uuid.UUID) error {
	if err := s.requireActivePartner(ctx, tx, partnerID); err != nil {
		return err
	}
	l, err := s.lockOwned(ctx, tx, partnerID, id)
	if err != nil {
		return err
	}
	if l.ServiceType != rideServiceType {
		return ErrNotRideListing
	}
	return nil
}

func (p RidePoint) normalize(label string) (RidePoint, error) {
	p.ProvinceCode = strings.TrimSpace(p.ProvinceCode)
	p.WardCode = strings.TrimSpace(p.WardCode)
	if p.ProvinceCode == "" || p.WardCode == "" {
		return p, invalidRide(label + ": cần chọn tỉnh/thành và phường/xã")
	}
	var err error
	if p.Name, err = normalizeText(p.Name, label+": tên", true, maxRidePointName); err != nil {
		return p, asInvalidRide(err)
	}
	if p.Address, err = normalizeText(p.Address, label+": địa chỉ", true, maxRideText); err != nil {
		return p, asInvalidRide(err)
	}
	return p, nil
}

func (in RideRouteInput) normalize() (RideRouteInput, error) {
	var err error
	if in.Origin, err = in.Origin.normalize("Điểm đi"); err != nil {
		return in, err
	}
	if in.Dest, err = in.Dest.normalize("Điểm đến"); err != nil {
		return in, err
	}
	if in.Origin.ProvinceCode == in.Dest.ProvinceCode && in.Origin.WardCode == in.Dest.WardCode &&
		strings.EqualFold(in.Origin.Name, in.Dest.Name) {
		return in, invalidRide("Điểm đi và điểm đến không được trùng nhau")
	}
	if in.DurationMinutes < 1 || in.DurationMinutes > maxRideDuration {
		return in, invalidRide(fmt.Sprintf("Thời gian chạy phải từ 1 đến %d phút", maxRideDuration))
	}
	if in.VehicleDescription, err = normalizeText(in.VehicleDescription, "Mô tả xe", true, maxRideText); err != nil {
		return in, asInvalidRide(err)
	}
	return in, nil
}

// routeFields flattens a route for the audit diff.
func routeFields(r *RideRouteInput) map[string]any {
	if r == nil {
		return map[string]any{}
	}
	return map[string]any{
		"origin_province_code": r.Origin.ProvinceCode, "origin_ward_code": r.Origin.WardCode,
		"origin_name": r.Origin.Name, "origin_address": r.Origin.Address,
		"dest_province_code": r.Dest.ProvinceCode, "dest_ward_code": r.Dest.WardCode,
		"dest_name": r.Dest.Name, "dest_address": r.Dest.Address,
		"duration_minutes": r.DurationMinutes, "vehicle_description": r.VehicleDescription,
	}
}

func fieldDiff(old, cur map[string]any) map[string]change {
	out := map[string]change{}
	for k, v := range cur {
		if ov, ok := old[k]; !ok || ov != v {
			out[k] = change{old[k], v}
		}
	}
	return out
}

// SetRideRoute creates or replaces the route of a ride listing. Names are
// 1..200 characters, addresses and the vehicle description 1..500, the
// duration 1..4320 minutes; each ward must belong to its province, and the
// origin and destination may not be the same place. Moving the origin or
// destination (province or ward) while a future trip has reserved seats is
// ErrHasActiveBookings.
func (s *Service) SetRideRoute(ctx context.Context, partnerID, actor, listingID uuid.UUID, in RideRouteInput) (RideRoute, error) {
	now := s.clock.Now()
	var out RideRoute
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockRide(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		r, err := in.normalize()
		if err != nil {
			return err
		}
		for _, p := range []RidePoint{r.Origin, r.Dest} {
			if err := s.geo.ValidateLocation(ctx, tx, p.ProvinceCode, p.WardCode); err != nil {
				return err
			}
		}
		q := store.New(tx)
		var old *RideRouteInput
		oldRow, err := q.GetRideRoute(ctx, listingID)
		switch {
		case err == nil:
			o := routeFromRow(oldRow).RideRouteInput
			old = &o
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("get ride route: %w", err)
		}
		changes := fieldDiff(routeFields(old), routeFields(&r))
		if old != nil && len(changes) == 0 {
			out = RideRoute{ListingID: listingID, RideRouteInput: r}
			return nil
		}
		if old != nil && (old.Origin.ProvinceCode != r.Origin.ProvinceCode || old.Origin.WardCode != r.Origin.WardCode ||
			old.Dest.ProvinceCode != r.Dest.ProvinceCode || old.Dest.WardCode != r.Dest.WardCode) {
			booked, err := q.HasBookedFutureTrips(ctx, store.HasBookedFutureTripsParams{ListingID: listingID, Now: now})
			if err != nil {
				return fmt.Errorf("booked trips: %w", err)
			}
			if booked {
				return ErrHasActiveBookings
			}
		}
		row, err := q.UpsertRideRoute(ctx, store.UpsertRideRouteParams{
			ListingID:          listingID,
			OriginProvinceCode: r.Origin.ProvinceCode, OriginWardCode: r.Origin.WardCode,
			OriginName: r.Origin.Name, OriginAddress: r.Origin.Address,
			DestProvinceCode: r.Dest.ProvinceCode, DestWardCode: r.Dest.WardCode,
			DestName: r.Dest.Name, DestAddress: r.Dest.Address,
			DurationMinutes: int32(r.DurationMinutes), VehicleDescription: r.VehicleDescription, Now: now,
		})
		if err != nil {
			return fmt.Errorf("upsert ride route: %w", err)
		}
		out = routeFromRow(row)
		return s.logChange(ctx, tx, listingID, actor, actionRideRoute, changes, now)
	})
	if err != nil {
		return RideRoute{}, err
	}
	return out, nil
}

// GetRide returns the route and every schedule (inactive included) of the
// partner's ride listing.
func (s *Service) GetRide(ctx context.Context, partnerID, listingID uuid.UUID) (RideDetail, error) {
	l, err := s.Get(ctx, partnerID, listingID)
	if err != nil {
		return RideDetail{}, err
	}
	if l.ServiceType != rideServiceType {
		return RideDetail{}, ErrNotRideListing
	}
	return s.rideDetail(ctx, s.pool, listingID, true)
}

// RideDetail reads the route and the schedules (departure time order) of a
// listing; inactive schedules only when includeInactive is set.
func (s *Service) RideDetail(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, includeInactive bool) (RideDetail, error) {
	return s.rideDetail(ctx, tx, listingID, includeInactive)
}

func (s *Service) rideDetail(ctx context.Context, dbtx store.DBTX, listingID uuid.UUID, includeInactive bool) (RideDetail, error) {
	q := store.New(dbtx)
	out := RideDetail{Schedules: []RideSchedule{}}
	r, err := q.GetRideRoute(ctx, listingID)
	switch {
	case err == nil:
		route := routeFromRow(r)
		out.Route = &route
	case !errors.Is(err, pgx.ErrNoRows):
		return out, fmt.Errorf("get ride route: %w", err)
	}
	rows, err := q.ListRideSchedules(ctx, store.ListRideSchedulesParams{ListingID: listingID, IncludeInactive: includeInactive})
	if err != nil {
		return out, fmt.Errorf("list ride schedules: %w", err)
	}
	for _, row := range rows {
		out.Schedules = append(out.Schedules, scheduleFromRow(row))
	}
	return out, nil
}

// scheduleFields is the comparable state of a schedule.
type scheduleFields struct {
	Minute       int // departure, minutes since midnight
	Weekdays     int
	SeatCapacity int
	PricePerSeat int64
	ValidFrom    time.Time  // UTC midnight of the VN date
	ValidTo      *time.Time // nil: open-ended
	Active       bool
}

func scheduleFieldsOf(r store.RideSchedule) scheduleFields {
	f := scheduleFields{
		Minute: int(r.DepartureTime.Microseconds / 60e6), Weekdays: int(r.Weekdays),
		SeatCapacity: int(r.SeatCapacity), PricePerSeat: r.PricePerSeat, ValidFrom: r.ValidFrom.Time, Active: r.Active,
	}
	if r.ValidTo.Valid {
		t := r.ValidTo.Time
		f.ValidTo = &t
	}
	return f
}

func (f scheduleFields) validate() error {
	if f.Weekdays < 1 || f.Weekdays > 127 {
		return invalidRide("weekdays phải nằm trong khoảng 1..127")
	}
	if f.SeatCapacity < 1 || f.SeatCapacity > maxRideSeats {
		return invalidRide(fmt.Sprintf("Số ghế phải từ 1 đến %d", maxRideSeats))
	}
	if f.PricePerSeat < minRideSeatPrice || f.PricePerSeat > maxRideSeatPrice {
		return invalidRide(fmt.Sprintf("Giá mỗi ghế phải từ %d đến %d VND", minRideSeatPrice, maxRideSeatPrice))
	}
	if f.ValidTo != nil && f.ValidTo.Before(f.ValidFrom) {
		return invalidRide("Ngày kết thúc phải không sớm hơn ngày bắt đầu")
	}
	return nil
}

// overlaps reports whether f and o depart at the same time on a shared
// weekday within intersecting validity ranges.
func (f scheduleFields) overlaps(o scheduleFields) bool {
	if f.Minute != o.Minute || f.Weekdays&o.Weekdays == 0 {
		return false
	}
	// [from, to] ranges, nil to = open-ended.
	if f.ValidTo != nil && f.ValidTo.Before(o.ValidFrom) {
		return false
	}
	if o.ValidTo != nil && o.ValidTo.Before(f.ValidFrom) {
		return false
	}
	return true
}

func (f scheduleFields) text() map[string]any {
	var to any
	if f.ValidTo != nil {
		to = f.ValidTo.Format(dateLayout)
	}
	return map[string]any{
		"departure_time": fmt.Sprintf("%02d:%02d", f.Minute/60, f.Minute%60),
		"weekdays":       f.Weekdays,
		"seat_capacity":  f.SeatCapacity,
		"price_per_seat": f.PricePerSeat,
		"valid_from":     f.ValidFrom.Format(dateLayout),
		"valid_to":       to,
		"active":         f.Active,
	}
}

func pgDate(t *time.Time) pgtype.Date {
	if t == nil {
		return pgtype.Date{}
	}
	return pgtype.Date{Time: *t, Valid: true}
}

// parseDate parses a YYYY-MM-DD date as UTC midnight (the pgtype.Date form).
func parseDate(s, field string) (time.Time, error) {
	t, err := time.Parse(dateLayout, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, invalidRide(field + " phải có dạng YYYY-MM-DD")
	}
	return t, nil
}

// checkScheduleOverlap rejects f if it is active and overlaps another active
// schedule of the listing (self excluded).
func checkScheduleOverlap(ctx context.Context, q *store.Queries, listingID, self uuid.UUID, f scheduleFields) error {
	if !f.Active {
		return nil
	}
	rows, err := q.ListRideSchedules(ctx, store.ListRideSchedulesParams{ListingID: listingID, IncludeInactive: false})
	if err != nil {
		return fmt.Errorf("list ride schedules: %w", err)
	}
	for _, r := range rows {
		if r.ID != self && f.overlaps(scheduleFieldsOf(r)) {
			return ErrOverlappingSchedule
		}
	}
	return nil
}

// CreateRideSchedule adds an active schedule to a listing that has a route
// (ErrRouteRequired otherwise) and generates its trips in the same tx. Seats
// 1..80, price 1,000..100,000,000 VND, valid_from today (Vietnam) or later.
// An active schedule with the same departure time, a shared weekday and an
// intersecting validity is ErrOverlappingSchedule.
func (s *Service) CreateRideSchedule(ctx context.Context, partnerID, actor, listingID uuid.UUID, in RideScheduleInput) (RideSchedule, error) {
	now := s.clock.Now()
	var out RideSchedule
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockRide(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		minute, err := minutes(in.DepartureTime, "Giờ khởi hành")
		if err != nil {
			return asInvalidRide(err)
		}
		f := scheduleFields{Minute: minute, Weekdays: in.Weekdays, SeatCapacity: in.SeatCapacity, PricePerSeat: in.PricePerSeat, Active: true}
		if f.ValidFrom, err = parseDate(in.ValidFrom, "valid_from"); err != nil {
			return err
		}
		if f.ValidFrom.Before(vnDate(now).Time) {
			return invalidRide("Ngày bắt đầu không được trước hôm nay")
		}
		if in.ValidTo != nil {
			to, err := parseDate(*in.ValidTo, "valid_to")
			if err != nil {
				return err
			}
			f.ValidTo = &to
		}
		if err := f.validate(); err != nil {
			return err
		}
		q := store.New(tx)
		if _, err := q.GetRideRoute(ctx, listingID); errors.Is(err, pgx.ErrNoRows) {
			return ErrRouteRequired
		} else if err != nil {
			return fmt.Errorf("get ride route: %w", err)
		}
		if err := checkScheduleOverlap(ctx, q, listingID, uuid.Nil, f); err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("ride schedule id: %w", err)
		}
		row, err := q.InsertRideSchedule(ctx, store.InsertRideScheduleParams{
			ID: id, ListingID: listingID, DepartureTime: pgTime(f.Minute), Weekdays: int16(f.Weekdays),
			SeatCapacity: int32(f.SeatCapacity), PricePerSeat: f.PricePerSeat,
			ValidFrom: pgDate(&f.ValidFrom), ValidTo: pgDate(f.ValidTo), Now: now,
		})
		if err != nil {
			return fmt.Errorf("insert ride schedule: %w", err)
		}
		if _, err := s.generateForSchedule(ctx, tx, row, now); err != nil {
			return err
		}
		out = scheduleFromRow(row)
		changes := fieldDiff(map[string]any{}, f.text())
		changes["id"] = change{nil, id.String()}
		return s.logChange(ctx, tx, listingID, actor, actionRideSchedule, changes, now)
	})
	if err != nil {
		return RideSchedule{}, err
	}
	return out, nil
}

// UpdateRideSchedule applies a patch to a schedule. Only the future trips of
// the schedule that are still scheduled and have no reserved seats follow it:
// a new price or capacity is applied to them in place; deactivating the
// schedule, or moving valid_to earlier, cancels those outside the new range
// (cancel_source schedule); reactivating it, or moving valid_to later,
// regenerates its trips now, reopening the ones it had cancelled. Trips with
// reserved seats never change.
func (s *Service) UpdateRideSchedule(ctx context.Context, partnerID, actor, listingID, scheduleID uuid.UUID, in RideSchedulePatch) (RideSchedule, error) {
	now := s.clock.Now()
	var out RideSchedule
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockRide(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		q := store.New(tx)
		cur, err := q.LockRideSchedule(ctx, store.LockRideScheduleParams{ID: scheduleID, ListingID: listingID})
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock ride schedule: %w", err)
		}
		if len(in.immutable) > 0 {
			return invalidRide("Không thể đổi " + strings.Join(in.immutable, ", ") + " của lịch chạy; hãy tạo lịch mới")
		}
		if in.ValidTo != nil && in.ClearValidTo {
			return invalidRide("valid_to và clear_valid_to không dùng cùng nhau")
		}
		old := scheduleFieldsOf(cur)
		f := old
		if in.SeatCapacity != nil {
			f.SeatCapacity = *in.SeatCapacity
		}
		if in.PricePerSeat != nil {
			f.PricePerSeat = *in.PricePerSeat
		}
		if in.ClearValidTo {
			f.ValidTo = nil
		}
		if in.ValidTo != nil {
			to, err := parseDate(*in.ValidTo, "valid_to")
			if err != nil {
				return err
			}
			f.ValidTo = &to
		}
		if in.Active != nil {
			f.Active = *in.Active
		}
		if err := f.validate(); err != nil {
			return err
		}
		changes := fieldDiff(old.text(), f.text())
		if len(changes) == 0 {
			out = scheduleFromRow(cur)
			return nil
		}
		_, activeChanged := changes["active"]
		_, toChanged := changes["valid_to"]
		if activeChanged || toChanged {
			if err := checkScheduleOverlap(ctx, q, listingID, scheduleID, f); err != nil {
				return err
			}
		}
		extended := old.ValidTo != nil && (f.ValidTo == nil || f.ValidTo.After(*old.ValidTo))
		shortened := f.ValidTo != nil && (old.ValidTo == nil || f.ValidTo.Before(*old.ValidTo))
		deactivated := old.Active && !f.Active
		regenerate := f.Active && (!old.Active || extended)
		generatedUntil := cur.GeneratedUntil
		if regenerate {
			generatedUntil = pgtype.Date{}
		}
		row, err := q.UpdateRideSchedule(ctx, store.UpdateRideScheduleParams{
			ID: scheduleID, SeatCapacity: int32(f.SeatCapacity), PricePerSeat: f.PricePerSeat,
			ValidTo: pgDate(f.ValidTo), Active: f.Active, GeneratedUntil: generatedUntil, Now: now,
		})
		if err != nil {
			return fmt.Errorf("update ride schedule: %w", err)
		}
		if f.SeatCapacity != old.SeatCapacity || f.PricePerSeat != old.PricePerSeat {
			if _, err := q.UpdateOpenScheduleTrips(ctx, store.UpdateOpenScheduleTripsParams{
				ScheduleID: scheduleID, SeatCapacity: int32(f.SeatCapacity), PricePerSeat: f.PricePerSeat, Now: now,
			}); err != nil {
				return fmt.Errorf("update schedule trips: %w", err)
			}
		}
		cancelFrom := time.Time{}
		switch {
		case deactivated:
			cancelFrom = now
		case shortened:
			// The first instant after valid_to, Vietnam time.
			cancelFrom = time.Date(f.ValidTo.Year(), f.ValidTo.Month(), f.ValidTo.Day()+1, 0, 0, 0, 0, clock.VN)
		}
		if !cancelFrom.IsZero() {
			if _, err := q.CancelOpenScheduleTrips(ctx, store.CancelOpenScheduleTripsParams{
				ScheduleID: scheduleID, FromAt: cancelFrom, Reason: scheduleCancelMsg, Actor: &actor, Now: now,
			}); err != nil {
				return fmt.Errorf("cancel schedule trips: %w", err)
			}
		}
		if regenerate {
			if _, err := s.generateForSchedule(ctx, tx, row, now); err != nil {
				return err
			}
		}
		out = scheduleFromRow(row)
		changes["id"] = change{scheduleID.String(), scheduleID.String()}
		return s.logChange(ctx, tx, listingID, actor, actionRideSchedule, changes, now)
	})
	if err != nil {
		return RideSchedule{}, err
	}
	return out, nil
}

// generateForSchedule creates the trips of an active schedule from
// max(valid_from, today, generated_until + 1) to min(valid_to, today + 59)
// (Vietnam dates, tripHorizonDays days), one per date on one of its weekdays
// at departure_time Vietnam time, skipping departures not after now. A trip
// the schedule had cancelled at the same instant is reopened; any other
// existing trip there is kept. It then records generated_until and returns
// how many trips it inserted or reopened. The caller holds the schedule row.
func (s *Service) generateForSchedule(ctx context.Context, tx pgx.Tx, sc store.RideSchedule, now time.Time) (int, error) {
	if !sc.Active {
		return 0, nil
	}
	today := vnDate(now).Time
	from := today
	if sc.ValidFrom.Time.After(from) {
		from = sc.ValidFrom.Time
	}
	if sc.GeneratedUntil.Valid {
		if next := sc.GeneratedUntil.Time.AddDate(0, 0, 1); next.After(from) {
			from = next
		}
	}
	to := today.AddDate(0, 0, tripHorizonDays-1)
	if sc.ValidTo.Valid && sc.ValidTo.Time.Before(to) {
		to = sc.ValidTo.Time
	}
	if from.After(to) {
		return 0, nil
	}
	hm := clockText(sc.DepartureTime)
	var ids []uuid.UUID
	var departures []time.Time
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, clock.VN)
		if int(sc.Weekdays)&weekdayBit(day) == 0 {
			continue
		}
		at, err := atVN(day, hm)
		if err != nil {
			return 0, err
		}
		if !at.After(now) {
			continue
		}
		id, err := uuid.NewV7()
		if err != nil {
			return 0, fmt.Errorf("ride trip id: %w", err)
		}
		ids = append(ids, id)
		departures = append(departures, at)
	}
	q := store.New(tx)
	var n int64
	if len(ids) > 0 {
		var err error
		n, err = q.InsertScheduleTrips(ctx, store.InsertScheduleTripsParams{
			Ids: ids, Departures: departures, ListingID: sc.ListingID, ScheduleID: sc.ID,
			SeatCapacity: sc.SeatCapacity, PricePerSeat: sc.PricePerSeat, Now: now,
		})
		if err != nil {
			return 0, fmt.Errorf("insert schedule trips: %w", err)
		}
	}
	if err := q.SetScheduleGeneratedUntil(ctx, store.SetScheduleGeneratedUntilParams{
		ID: sc.ID, GeneratedUntil: pgtype.Date{Time: to, Valid: true}, Now: now,
	}); err != nil {
		return 0, fmt.Errorf("set generated_until: %w", err)
	}
	return int(n), nil
}
