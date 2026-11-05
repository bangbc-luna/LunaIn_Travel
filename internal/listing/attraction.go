package listing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
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
	attractionServiceType = "attraction_ticket"

	maxAttractionText = 2000
	maxTicketName     = 100
	minTicketPrice    = 1_000
	maxTicketPrice    = 100_000_000

	audienceChild = "child"
	audienceAdult = "adult"

	actionAttraction = "attraction"
	actionTicketType = "ticket_type"
	actionTimeSlot   = "time_slot"
	actionClosedDate = "closed_date"

	dateLayout = "2006-01-02"

	// maxClosedDateAheadDays bounds how far ahead a closed date may be set.
	maxClosedDateAheadDays = 730
)

var (
	ErrWrongServiceType    = httpx.NewError(http.StatusUnprocessableEntity, "wrong_service_type", "Thao tác này chỉ áp dụng cho Listing vé tham quan")
	ErrOverlappingTimeSlot = httpx.NewError(http.StatusUnprocessableEntity, "overlapping_time_slot", "Khung giờ trùng với một khung giờ đang hoạt động khác")
	ErrInvalidTicketType   = httpx.NewError(http.StatusUnprocessableEntity, "invalid_ticket_type", "Loại vé không hợp lệ")
	ErrInvalidClosedDate   = httpx.NewError(http.StatusUnprocessableEntity, "invalid_closed_date", "Không thể đặt ngày nghỉ trong quá khứ")
	ErrClosedDateExists    = httpx.NewError(http.StatusConflict, "closed_date_exists", "Ngày nghỉ này đã tồn tại")
	// ErrHasActiveBookings: closing a date or moving/disabling a slot that
	// customers hold active bookings on.
	ErrHasActiveBookings = httpx.NewError(http.StatusConflict, "has_active_bookings", "Đã có đơn đặt chỗ đang hiệu lực, không thể thay đổi lịch này")
)

var hhmm = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// AttractionInfo is the free-text part of an attraction listing.
type AttractionInfo struct {
	OpeningNote         *string `json:"opening_note"`
	CheckinInstructions *string `json:"checkin_instructions"`
}

// TicketType is one sellable ticket of an attraction. Price is in VND.
type TicketType struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	Audience    string    `json:"audience"`
	Price       int64     `json:"price"`
	Active      bool      `json:"active"`
	SortOrder   int       `json:"sort_order"`
}

// TicketTypeInput creates a ticket type (always active).
type TicketTypeInput struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Audience    string  `json:"audience"`
	Price       int64   `json:"price"`
	SortOrder   int     `json:"sort_order"`
}

// TicketTypePatch updates a ticket type; nil fields are left unchanged. An
// empty description clears it.
type TicketTypePatch struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Audience    *string `json:"audience"`
	Price       *int64  `json:"price"`
	Active      *bool   `json:"active"`
	SortOrder   *int    `json:"sort_order"`
}

// TimeSlot is a daily visiting window. Weekdays is a bitmask: bit 0 =
// Monday ... bit 6 = Sunday. Times are "HH:MM" (24h).
type TimeSlot struct {
	ID        uuid.UUID `json:"id"`
	StartTime string    `json:"start_time"`
	EndTime   string    `json:"end_time"`
	Weekdays  int       `json:"weekdays"`
	Active    bool      `json:"active"`
}

// StartAt is the instant the slot starts on the Vietnam calendar date of
// date (date is read in clock.VN, so 17:00Z is already the next VN day).
func (t TimeSlot) StartAt(date time.Time) (time.Time, error) { return atVN(date, t.StartTime) }

// EndAt is the instant the slot ends on the Vietnam calendar date of date.
func (t TimeSlot) EndAt(date time.Time) (time.Time, error) { return atVN(date, t.EndTime) }

// atVN combines the Vietnam calendar date of date with an "HH:MM" clock.
func atVN(date time.Time, hm string) (time.Time, error) {
	m, err := minutes(hm, "time")
	if err != nil {
		return time.Time{}, err
	}
	y, mo, d := date.In(clock.VN).Date()
	return time.Date(y, mo, d, m/60, m%60, 0, 0, clock.VN), nil
}

// TimeSlotInput creates a time slot (always active).
type TimeSlotInput struct {
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	Weekdays  int    `json:"weekdays"`
}

// TimeSlotPatch updates a time slot; nil fields are left unchanged.
type TimeSlotPatch struct {
	StartTime *string `json:"start_time"`
	EndTime   *string `json:"end_time"`
	Weekdays  *int    `json:"weekdays"`
	Active    *bool   `json:"active"`
}

// ClosedDate is a date (YYYY-MM-DD, Vietnam calendar) the attraction is closed.
type ClosedDate struct {
	Date   string  `json:"date"`
	Reason *string `json:"reason"`
}

// AttractionDetail is everything a listing page needs about an attraction.
type AttractionDetail struct {
	Info        AttractionInfo `json:"info"`
	TicketTypes []TicketType   `json:"ticket_types"`
	TimeSlots   []TimeSlot     `json:"time_slots"`
	ClosedDates []ClosedDate   `json:"closed_dates"`
}

func ticketFromRow(r store.AttractionTicketType) TicketType {
	return TicketType{
		ID: r.ID, Name: r.Name, Description: r.Description, Audience: r.Audience,
		Price: r.Price, Active: r.Active, SortOrder: int(r.SortOrder),
	}
}

func clockText(t pgtype.Time) string {
	m := t.Microseconds / int64(time.Minute/time.Microsecond)
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

func slotFromRow(r store.AttractionTimeSlot) TimeSlot {
	return TimeSlot{
		ID: r.ID, StartTime: clockText(r.StartTime), EndTime: clockText(r.EndTime),
		Weekdays: int(r.Weekdays), Active: r.Active,
	}
}

// minutes parses "HH:MM" into minutes since midnight.
func minutes(s, field string) (int, error) {
	m := hhmm.FindStringSubmatch(s)
	if m == nil {
		return 0, invalidListing(field + " phải có dạng HH:MM (24 giờ)")
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return h*60 + mi, nil
}

func pgTime(min int) pgtype.Time {
	return pgtype.Time{Microseconds: int64(min) * int64(time.Minute/time.Microsecond), Valid: true}
}

// vnDate is the Vietnam calendar date of t as a UTC-midnight pgtype.Date.
func vnDate(t time.Time) pgtype.Date {
	y, m, d := t.In(clock.VN).Date()
	return pgtype.Date{Time: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Valid: true}
}

// weekdayBit maps the Vietnam weekday of t to its bit: Monday = 1<<0 ...
// Sunday = 1<<6 (Go's time.Weekday has Sunday = 0).
func weekdayBit(t time.Time) int {
	return 1 << ((int(t.In(clock.VN).Weekday()) + 6) % 7)
}

// lockAttraction runs the checks every attraction write starts with, in the
// lock order of DB §1.1: partner (FOR SHARE) -> listing (FOR NO KEY UPDATE).
// The listing must be an attraction_ticket (enforced here, not in the DB).
func (s *Service) lockAttraction(ctx context.Context, tx pgx.Tx, partnerID, id uuid.UUID) error {
	if err := s.requireActivePartner(ctx, tx, partnerID); err != nil {
		return err
	}
	l, err := s.lockOwned(ctx, tx, partnerID, id)
	if err != nil {
		return err
	}
	if l.ServiceType != attractionServiceType {
		return ErrWrongServiceType
	}
	return nil
}

// Booking guards (spec: a partner cannot close a date or move/disable a slot
// that active bookings stand on). Race with booking create/reschedule: every
// caller holds the listing FOR NO KEY UPDATE (lockAttraction) before asking,
// and every booking path that reads the schedule takes the listing FOR SHARE
// first (create/confirm via BookableListing, reschedule via ShareListingLock)
// in the same tx. The two lock modes conflict, so either the booking tx
// committed before the lock was granted (the count, a later statement in READ
// COMMITTED, sees it) or it waits until this write commits and then reads the
// new schedule. With a nil guard the checks are skipped.

// guardDate rejects a closed date on the Vietnam date of day with active
// bookings of listingID.
func (s *Service) guardDate(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, day time.Time) error {
	if s.bookings == nil {
		return nil
	}
	n, err := s.bookings.ActiveBookingsOnDate(ctx, tx, listingID, day)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrHasActiveBookings
	}
	return nil
}

// guardSlot rejects a change to slotID while it has active bookings from today
// (Vietnam) on.
func (s *Service) guardSlot(ctx context.Context, tx pgx.Tx, slotID uuid.UUID, now time.Time) error {
	if s.bookings == nil {
		return nil
	}
	n, err := s.bookings.ActiveBookingsForSlot(ctx, tx, slotID, vnDate(now).Time)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrHasActiveBookings
	}
	return nil
}

// SetAttractionInfo upserts the attraction's free-text fields (each at most
// 2000 characters; empty means none).
func (s *Service) SetAttractionInfo(ctx context.Context, partnerID, actor, id uuid.UUID, openingNote, checkinInstructions *string) (AttractionInfo, error) {
	now := s.clock.Now()
	var out AttractionInfo
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockAttraction(ctx, tx, partnerID, id); err != nil {
			return err
		}
		note, err := optionalText(openingNote, "Ghi chú giờ mở cửa", maxAttractionText)
		if err != nil {
			return err
		}
		checkin, err := optionalText(checkinInstructions, "Hướng dẫn check-in", maxAttractionText)
		if err != nil {
			return err
		}
		q := store.New(tx)
		old, err := q.GetAttraction(ctx, id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get attraction: %w", err)
		}
		row, err := q.UpsertAttraction(ctx, store.UpsertAttractionParams{
			ListingID: id, OpeningNote: note, CheckinInstructions: checkin, Now: now,
		})
		if err != nil {
			return fmt.Errorf("upsert attraction: %w", err)
		}
		out = AttractionInfo{OpeningNote: row.OpeningNote, CheckinInstructions: row.CheckinInstructions}
		changes := map[string]change{}
		if !eq(old.OpeningNote, note) {
			changes["opening_note"] = change{val(old.OpeningNote), val(note)}
		}
		if !eq(old.CheckinInstructions, checkin) {
			changes["checkin_instructions"] = change{val(old.CheckinInstructions), val(checkin)}
		}
		return s.logChange(ctx, tx, id, actor, actionAttraction, changes, now)
	})
	if err != nil {
		return AttractionInfo{}, err
	}
	return out, nil
}

type ticketFields struct {
	Name        string
	Description *string
	Audience    string
	Price       int64
	Active      bool
	SortOrder   int
}

func (f ticketFields) validate() (ticketFields, error) {
	name, err := normalizeText(f.Name, "Tên loại vé", true, maxTicketName)
	if err != nil {
		return f, err
	}
	f.Name = name
	if f.Description, err = optionalText(f.Description, "Mô tả loại vé", maxAttractionText); err != nil {
		return f, err
	}
	if f.Audience != audienceChild && f.Audience != audienceAdult {
		return f, invalidListing("Đối tượng phải là child hoặc adult")
	}
	if f.Price < minTicketPrice || f.Price > maxTicketPrice {
		return f, invalidListing(fmt.Sprintf("Giá vé phải từ %d đến %d VND", minTicketPrice, maxTicketPrice))
	}
	return f, nil
}

func (f ticketFields) diff(old *ticketFields) map[string]change {
	var o ticketFields
	if old != nil {
		o = *old
	}
	out := map[string]change{}
	add := func(name string, a, b any, same bool) {
		if !same {
			out[name] = change{a, b}
		}
	}
	add("name", o.Name, f.Name, o.Name == f.Name && old != nil)
	add("description", val(o.Description), val(f.Description), eq(o.Description, f.Description))
	add("audience", o.Audience, f.Audience, o.Audience == f.Audience && old != nil)
	add("price", o.Price, f.Price, o.Price == f.Price && old != nil)
	add("active", o.Active, f.Active, o.Active == f.Active && old != nil)
	add("sort_order", o.SortOrder, f.SortOrder, o.SortOrder == f.SortOrder && old != nil)
	return out
}

func ticketFieldsOf(r store.AttractionTicketType) ticketFields {
	return ticketFields{r.Name, r.Description, r.Audience, r.Price, r.Active, int(r.SortOrder)}
}

// CreateTicketType adds an active ticket type, creating the attractions row
// if it does not exist yet. Name 1..100 characters, price 1,000..100,000,000
// VND, audience child or adult. Ticket types are never deleted: patch
// active=false instead.
func (s *Service) CreateTicketType(ctx context.Context, partnerID, actor, listingID uuid.UUID, in TicketTypeInput) (TicketType, error) {
	now := s.clock.Now()
	var out TicketType
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockAttraction(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		f, err := ticketFields{in.Name, in.Description, in.Audience, in.Price, true, in.SortOrder}.validate()
		if err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("ticket type id: %w", err)
		}
		q := store.New(tx)
		if err := q.EnsureAttraction(ctx, store.EnsureAttractionParams{ListingID: listingID, Now: now}); err != nil {
			return fmt.Errorf("ensure attraction: %w", err)
		}
		row, err := q.InsertTicketType(ctx, store.InsertTicketTypeParams{
			ID: id, ListingID: listingID, Name: f.Name, Description: f.Description, Audience: f.Audience,
			Price: f.Price, Active: f.Active, SortOrder: int32(f.SortOrder), Now: now,
		})
		if err != nil {
			return fmt.Errorf("insert ticket type: %w", err)
		}
		out = ticketFromRow(row)
		changes := f.diff(nil)
		changes["id"] = change{nil, id.String()}
		return s.logChange(ctx, tx, listingID, actor, actionTicketType, changes, now)
	})
	if err != nil {
		return TicketType{}, err
	}
	return out, nil
}

// UpdateTicketType applies a patch to a ticket type of the listing.
func (s *Service) UpdateTicketType(ctx context.Context, partnerID, actor, listingID, ticketTypeID uuid.UUID, in TicketTypePatch) (TicketType, error) {
	now := s.clock.Now()
	var out TicketType
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockAttraction(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		q := store.New(tx)
		cur, err := q.GetTicketType(ctx, store.GetTicketTypeParams{ID: ticketTypeID, ListingID: listingID})
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get ticket type: %w", err)
		}
		old := ticketFieldsOf(cur)
		f := old
		if in.Name != nil {
			f.Name = *in.Name
		}
		if in.Description != nil {
			f.Description = in.Description
		}
		if in.Audience != nil {
			f.Audience = *in.Audience
		}
		if in.Price != nil {
			f.Price = *in.Price
		}
		if in.Active != nil {
			f.Active = *in.Active
		}
		if in.SortOrder != nil {
			f.SortOrder = *in.SortOrder
		}
		if f, err = f.validate(); err != nil {
			return err
		}
		changes := f.diff(&old)
		if len(changes) == 0 {
			// Like Update: a patch that changes nothing writes nothing.
			out = ticketFromRow(cur)
			return nil
		}
		row, err := q.UpdateTicketType(ctx, store.UpdateTicketTypeParams{
			ID: ticketTypeID, ListingID: listingID, Name: f.Name, Description: f.Description, Audience: f.Audience,
			Price: f.Price, Active: f.Active, SortOrder: int32(f.SortOrder), Now: now,
		})
		if err != nil {
			return fmt.Errorf("update ticket type: %w", err)
		}
		out = ticketFromRow(row)
		changes["id"] = change{ticketTypeID.String(), ticketTypeID.String()}
		return s.logChange(ctx, tx, listingID, actor, actionTicketType, changes, now)
	})
	if err != nil {
		return TicketType{}, err
	}
	return out, nil
}

type slotFields struct {
	Start, End int // minutes since midnight
	Weekdays   int
	Active     bool
}

func (f slotFields) validate() error {
	if f.End <= f.Start {
		return invalidListing("Giờ kết thúc phải sau giờ bắt đầu")
	}
	if f.Weekdays < 1 || f.Weekdays > 127 {
		return invalidListing("weekdays phải nằm trong khoảng 1..127")
	}
	return nil
}

func (f slotFields) overlaps(o store.AttractionTimeSlot) bool {
	os, oe := int(o.StartTime.Microseconds/60e6), int(o.EndTime.Microseconds/60e6)
	return f.Weekdays&int(o.Weekdays) != 0 && f.Start < oe && os < f.End
}

func (f slotFields) text() map[string]any {
	return map[string]any{
		"start_time": fmt.Sprintf("%02d:%02d", f.Start/60, f.Start%60),
		"end_time":   fmt.Sprintf("%02d:%02d", f.End/60, f.End%60),
		"weekdays":   f.Weekdays,
		"active":     f.Active,
	}
}

func (f slotFields) diff(old *slotFields) map[string]change {
	n := f.text()
	var o map[string]any
	if old != nil {
		o = old.text()
	}
	out := map[string]change{}
	for k, v := range n {
		if ov, ok := o[k]; !ok || ov != v {
			out[k] = change{o[k], v}
		}
	}
	return out
}

// checkOverlap rejects f if it is active and shares a weekday and a time
// range with another active slot of the listing (self excluded).
func checkOverlap(ctx context.Context, q *store.Queries, listingID, self uuid.UUID, f slotFields) error {
	if !f.Active {
		return nil
	}
	rows, err := q.ListTimeSlots(ctx, store.ListTimeSlotsParams{ListingID: listingID, IncludeInactive: false})
	if err != nil {
		return fmt.Errorf("list time slots: %w", err)
	}
	for _, r := range rows {
		if r.ID != self && f.overlaps(r) {
			return ErrOverlappingTimeSlot
		}
	}
	return nil
}

// CreateTimeSlot adds an active slot, creating the attractions row if needed.
// Two active slots may not share a weekday and overlap in time ([start, end)).
func (s *Service) CreateTimeSlot(ctx context.Context, partnerID, actor, listingID uuid.UUID, in TimeSlotInput) (TimeSlot, error) {
	now := s.clock.Now()
	var out TimeSlot
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockAttraction(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		start, err := minutes(in.StartTime, "Giờ bắt đầu")
		if err != nil {
			return err
		}
		end, err := minutes(in.EndTime, "Giờ kết thúc")
		if err != nil {
			return err
		}
		f := slotFields{start, end, in.Weekdays, true}
		if err := f.validate(); err != nil {
			return err
		}
		q := store.New(tx)
		if err := checkOverlap(ctx, q, listingID, uuid.Nil, f); err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("time slot id: %w", err)
		}
		if err := q.EnsureAttraction(ctx, store.EnsureAttractionParams{ListingID: listingID, Now: now}); err != nil {
			return fmt.Errorf("ensure attraction: %w", err)
		}
		row, err := q.InsertTimeSlot(ctx, store.InsertTimeSlotParams{
			ID: id, ListingID: listingID, StartTime: pgTime(f.Start), EndTime: pgTime(f.End),
			Weekdays: int16(f.Weekdays), Active: f.Active, Now: now,
		})
		if err != nil {
			return fmt.Errorf("insert time slot: %w", err)
		}
		out = slotFromRow(row)
		changes := f.diff(nil)
		changes["id"] = change{nil, id.String()}
		return s.logChange(ctx, tx, listingID, actor, actionTimeSlot, changes, now)
	})
	if err != nil {
		return TimeSlot{}, err
	}
	return out, nil
}

// UpdateTimeSlot applies a patch. The overlap rule is checked against the
// other active slots whenever the result is active (including re-activation).
func (s *Service) UpdateTimeSlot(ctx context.Context, partnerID, actor, listingID, slotID uuid.UUID, in TimeSlotPatch) (TimeSlot, error) {
	now := s.clock.Now()
	var out TimeSlot
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockAttraction(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		q := store.New(tx)
		cur, err := q.GetTimeSlot(ctx, store.GetTimeSlotParams{ID: slotID, ListingID: listingID})
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get time slot: %w", err)
		}
		old := slotFields{int(cur.StartTime.Microseconds / 60e6), int(cur.EndTime.Microseconds / 60e6), int(cur.Weekdays), cur.Active}
		f := old
		if in.StartTime != nil {
			if f.Start, err = minutes(*in.StartTime, "Giờ bắt đầu"); err != nil {
				return err
			}
		}
		if in.EndTime != nil {
			if f.End, err = minutes(*in.EndTime, "Giờ kết thúc"); err != nil {
				return err
			}
		}
		if in.Weekdays != nil {
			f.Weekdays = *in.Weekdays
		}
		if in.Active != nil {
			f.Active = *in.Active
		}
		if err := f.validate(); err != nil {
			return err
		}
		changes := f.diff(&old)
		if len(changes) == 0 {
			out = slotFromRow(cur)
			return nil
		}
		if err := checkOverlap(ctx, q, listingID, slotID, f); err != nil {
			return err
		}
		if f.Start != old.Start || f.End != old.End || f.Weekdays != old.Weekdays || (old.Active && !f.Active) {
			if err := s.guardSlot(ctx, tx, slotID, now); err != nil {
				return err
			}
		}
		row, err := q.UpdateTimeSlot(ctx, store.UpdateTimeSlotParams{
			ID: slotID, ListingID: listingID, StartTime: pgTime(f.Start), EndTime: pgTime(f.End),
			Weekdays: int16(f.Weekdays), Active: f.Active, Now: now,
		})
		if err != nil {
			return fmt.Errorf("update time slot: %w", err)
		}
		out = slotFromRow(row)
		changes["id"] = change{slotID.String(), slotID.String()}
		return s.logChange(ctx, tx, listingID, actor, actionTimeSlot, changes, now)
	})
	if err != nil {
		return TimeSlot{}, err
	}
	return out, nil
}

// AddClosedDate closes the attraction on the Vietnam calendar date of date.
// A date before today or more than 730 days after today (Vietnam time) is
// ErrInvalidClosedDate; an existing one is ErrClosedDateExists.
func (s *Service) AddClosedDate(ctx context.Context, partnerID, actor, listingID uuid.UUID, date time.Time, reason *string) error {
	now := s.clock.Now()
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockAttraction(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		r, err := optionalText(reason, "Lý do", maxAttractionText)
		if err != nil {
			return err
		}
		d := vnDate(date)
		today := vnDate(now).Time
		if d.Time.Before(today) {
			return ErrInvalidClosedDate
		}
		if d.Time.After(today.AddDate(0, 0, maxClosedDateAheadDays)) {
			return withMessage(ErrInvalidClosedDate, fmt.Sprintf("Chỉ đặt được ngày nghỉ trong vòng %d ngày tới", maxClosedDateAheadDays))
		}
		if err := s.guardDate(ctx, tx, listingID, d.Time); err != nil {
			return err
		}
		q := store.New(tx)
		if err := q.EnsureAttraction(ctx, store.EnsureAttractionParams{ListingID: listingID, Now: now}); err != nil {
			return fmt.Errorf("ensure attraction: %w", err)
		}
		n, err := q.InsertClosedDate(ctx, store.InsertClosedDateParams{ListingID: listingID, Date: d, Reason: r, Now: now})
		if err != nil {
			return fmt.Errorf("insert closed date: %w", err)
		}
		if n == 0 {
			return ErrClosedDateExists
		}
		return s.logChange(ctx, tx, listingID, actor, actionClosedDate, map[string]change{
			"date":   {nil, d.Time.Format(dateLayout)},
			"reason": {nil, val(r)},
		}, now)
	})
}

// RemoveClosedDate deletes a closed date; a date that is not closed is 404.
func (s *Service) RemoveClosedDate(ctx context.Context, partnerID, actor, listingID uuid.UUID, date time.Time) error {
	now := s.clock.Now()
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockAttraction(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		d := vnDate(date)
		q := store.New(tx)
		old, err := q.GetClosedDate(ctx, store.GetClosedDateParams{ListingID: listingID, Date: d})
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get closed date: %w", err)
		}
		if _, err := q.DeleteClosedDate(ctx, store.DeleteClosedDateParams{ListingID: listingID, Date: d}); err != nil {
			return fmt.Errorf("delete closed date: %w", err)
		}
		return s.logChange(ctx, tx, listingID, actor, actionClosedDate, map[string]change{
			"date":   {d.Time.Format(dateLayout), nil},
			"reason": {val(old.Reason), nil},
		}, now)
	})
}

// AttractionDetail reads the info, ticket types (sort_order, name), time slots
// (start_time) and the closed dates from today (Vietnam) on. Inactive ticket
// types and slots are included only when includeInactive is set.
func (s *Service) AttractionDetail(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, includeInactive bool) (AttractionDetail, error) {
	return s.attractionDetail(ctx, tx, listingID, includeInactive)
}

func (s *Service) attractionDetail(ctx context.Context, dbtx store.DBTX, listingID uuid.UUID, includeInactive bool) (AttractionDetail, error) {
	q := store.New(dbtx)
	out := AttractionDetail{TicketTypes: []TicketType{}, TimeSlots: []TimeSlot{}, ClosedDates: []ClosedDate{}}
	info, err := q.GetAttraction(ctx, listingID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("get attraction: %w", err)
	}
	out.Info = AttractionInfo{OpeningNote: info.OpeningNote, CheckinInstructions: info.CheckinInstructions}
	tts, err := q.ListTicketTypes(ctx, store.ListTicketTypesParams{ListingID: listingID, IncludeInactive: includeInactive})
	if err != nil {
		return out, fmt.Errorf("list ticket types: %w", err)
	}
	for _, r := range tts {
		out.TicketTypes = append(out.TicketTypes, ticketFromRow(r))
	}
	slots, err := q.ListTimeSlots(ctx, store.ListTimeSlotsParams{ListingID: listingID, IncludeInactive: includeInactive})
	if err != nil {
		return out, fmt.Errorf("list time slots: %w", err)
	}
	for _, r := range slots {
		out.TimeSlots = append(out.TimeSlots, slotFromRow(r))
	}
	closed, err := q.ListClosedDatesFrom(ctx, store.ListClosedDatesFromParams{ListingID: listingID, FromDate: vnDate(s.clock.Now())})
	if err != nil {
		return out, fmt.Errorf("list closed dates: %w", err)
	}
	for _, r := range closed {
		out.ClosedDates = append(out.ClosedDates, ClosedDate{Date: r.Date.Time.Format(dateLayout), Reason: r.Reason})
	}
	return out, nil
}

// SlotsForDate returns the active slots running on date (its Vietnam
// weekday), or none if that date is a closed date. For booking (plan 4).
func (s *Service) SlotsForDate(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, date time.Time) ([]TimeSlot, error) {
	q := store.New(tx)
	_, err := q.GetClosedDate(ctx, store.GetClosedDateParams{ListingID: listingID, Date: vnDate(date)})
	if err == nil {
		return []TimeSlot{}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get closed date: %w", err)
	}
	rows, err := q.ListTimeSlots(ctx, store.ListTimeSlotsParams{ListingID: listingID, IncludeInactive: false})
	if err != nil {
		return nil, fmt.Errorf("list time slots: %w", err)
	}
	bit := weekdayBit(date)
	out := []TimeSlot{}
	for _, r := range rows {
		if int(r.Weekdays)&bit != 0 {
			out = append(out, slotFromRow(r))
		}
	}
	return out, nil
}

// TicketTypesForBooking returns the requested ticket types keyed by id. Only
// active types of this listing count; if any id is missing, or ids is empty,
// the whole call is ErrInvalidTicketType. For booking (plan 4).
func (s *Service) TicketTypesForBooking(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]TicketType, error) {
	if len(ids) == 0 {
		return nil, ErrInvalidTicketType
	}
	rows, err := store.New(tx).ListActiveTicketTypesByIDs(ctx, store.ListActiveTicketTypesByIDsParams{ListingID: listingID, Ids: ids})
	if err != nil {
		return nil, fmt.Errorf("list ticket types: %w", err)
	}
	out := make(map[uuid.UUID]TicketType, len(rows))
	for _, r := range rows {
		out[r.ID] = ticketFromRow(r)
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, ErrInvalidTicketType
		}
	}
	return out, nil
}
