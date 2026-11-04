package listing_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

func (e *env) ticket(t *testing.T, p partner.Partner, l listing.Listing, name string, price int64) listing.TicketType {
	t.Helper()
	tt, err := e.Svc.CreateTicketType(context.Background(), p.ID, p.OwnerUserID, l.ID,
		listing.TicketTypeInput{Name: name, Audience: "adult", Price: price})
	require.NoError(t, err)
	return tt
}

func (e *env) slot(t *testing.T, p partner.Partner, l listing.Listing, start, end string, weekdays int) (listing.TimeSlot, error) {
	t.Helper()
	return e.Svc.CreateTimeSlot(context.Background(), p.ID, p.OwnerUserID, l.ID,
		listing.TimeSlotInput{StartTime: start, EndTime: end, Weekdays: weekdays})
}

// Weekday bits: bit 0 = Monday ... bit 6 = Sunday.
const (
	mon      = 1 << 0
	sat      = 1 << 5
	sun      = 1 << 6
	monToFri = 0b0011111
)

func inTx(t *testing.T, e *env, f func(tx pgx.Tx) error) {
	t.Helper()
	require.NoError(t, db.InTx(context.Background(), e.Pool, f))
}

func TestTicketType_CreateUpdateDeactivate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")

	tt, err := e.Svc.CreateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, listing.TicketTypeInput{
		Name: " Người lớn ", Audience: "adult", Price: 150000, SortOrder: 1, Description: ptr("Từ 12 tuổi"),
	})
	require.NoError(t, err)
	require.Equal(t, "Người lớn", tt.Name)
	require.True(t, tt.Active)
	require.Equal(t, int64(150000), tt.Price)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM attractions WHERE listing_id = $1`, l.ID))

	got, err := e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, tt.ID, listing.TicketTypePatch{
		Price: ptr(int64(180000)), Name: ptr("Người lớn (VN)"),
	})
	require.NoError(t, err)
	require.Equal(t, int64(180000), got.Price)
	require.Equal(t, "Người lớn (VN)", got.Name)
	require.Equal(t, "adult", got.Audience)
	require.Equal(t, ptr("Từ 12 tuổi"), got.Description)

	got, err = e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, tt.ID, listing.TicketTypePatch{Active: ptr(false)})
	require.NoError(t, err)
	require.False(t, got.Active)

	// Inactive types are listed only with includeInactive.
	inTx(t, e, func(tx pgx.Tx) error {
		d, err := e.Svc.AttractionDetail(ctx, tx, l.ID, false)
		require.NoError(t, err)
		require.Empty(t, d.TicketTypes)
		d, err = e.Svc.AttractionDetail(ctx, tx, l.ID, true)
		require.NoError(t, err)
		require.Len(t, d.TicketTypes, 1)
		return nil
	})

	// One audit row per write: the listing's creation, the ticket type's
	// creation and the two updates.
	au := e.audits(t, l.ID)
	require.Len(t, au, 4)
	require.Equal(t, "ticket_type", au[2].Action)
	require.EqualValues(t, 150000, au[2].Changes["price"].Old)
	require.EqualValues(t, 180000, au[2].Changes["price"].New)

	// Unknown ticket type id is 404.
	_, err = e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, uuid.New(), listing.TicketTypePatch{Active: ptr(true)})
	st, code := appCode(t, err)
	require.Equal(t, http.StatusNotFound, st)
	require.Equal(t, "not_found", code)
}

func TestTicketType_Validation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	bad := []listing.TicketTypeInput{
		{Name: "A", Audience: "adult", Price: 999},
		{Name: "A", Audience: "adult", Price: 100_000_001},
		{Name: " ", Audience: "adult", Price: 5000},
		{Name: strings.Repeat("a", 101), Audience: "adult", Price: 5000},
		{Name: "A", Audience: "senior", Price: 5000},
	}
	for _, in := range bad {
		_, err := e.Svc.CreateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, in)
		st, code := appCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, st)
		require.Equal(t, "invalid_listing", code)
	}
	require.Zero(t, e.count(t, `SELECT count(*) FROM attraction_ticket_types`))
	_, err := e.Svc.CreateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, listing.TicketTypeInput{Name: "A", Audience: "child", Price: 1000})
	require.NoError(t, err)
	_, err = e.Svc.CreateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, listing.TicketTypeInput{Name: "A", Audience: "child", Price: 100_000_000})
	require.NoError(t, err)
	tt := e.ticket(t, p, l, "B", 5000)
	_, err = e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, tt.ID, listing.TicketTypePatch{Price: ptr(int64(999))})
	st, _ := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, st)
	_, err = e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, tt.ID, listing.TicketTypePatch{Name: ptr("")})
	st, _ = appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, st)
}

func TestTicketType_WrongServiceType422(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction, "self_drive_rental")
	l, err := e.Svc.Create(ctx, p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: "self_drive_rental", Title: "Xe"})
	require.NoError(t, err)

	_, err = e.Svc.CreateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, listing.TicketTypeInput{Name: "A", Audience: "adult", Price: 5000})
	st, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, st)
	require.Equal(t, "wrong_service_type", code)
	_, err = e.slot(t, p, l, "08:00", "10:00", mon)
	_, code = appCode(t, err)
	require.Equal(t, "wrong_service_type", code)
	_, err = e.Svc.SetAttractionInfo(ctx, p.ID, p.OwnerUserID, l.ID, ptr("x"), nil)
	_, code = appCode(t, err)
	require.Equal(t, "wrong_service_type", code)
	err = e.Svc.AddClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), nil)
	_, code = appCode(t, err)
	require.Equal(t, "wrong_service_type", code)

	for _, tbl := range []string{"attractions", "attraction_ticket_types", "attraction_time_slots", "attraction_closed_dates"} {
		require.Zero(t, e.count(t, `SELECT count(*) FROM `+tbl))
	}
	require.Len(t, e.audits(t, l.ID), 1) // only the listing's own creation
}

func TestTimeSlot_Overlap422(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")

	a, err := e.slot(t, p, l, "08:00", "10:00", monToFri)
	require.NoError(t, err)
	require.Equal(t, "08:00", a.StartTime)
	require.Equal(t, "10:00", a.EndTime)
	require.Equal(t, monToFri, a.Weekdays)

	_, err = e.slot(t, p, l, "09:00", "11:00", mon)
	st, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, st)
	require.Equal(t, "overlapping_time_slot", code)

	b, err := e.slot(t, p, l, "09:00", "11:00", sat)
	require.NoError(t, err)
	// Touching boundaries do not overlap: [08,10) and [10,12) on Monday.
	_, err = e.slot(t, p, l, "10:00", "12:00", mon)
	require.NoError(t, err)

	// Updating a slot into a clash is rejected; updating itself in place is fine.
	_, err = e.Svc.UpdateTimeSlot(ctx, p.ID, p.OwnerUserID, l.ID, b.ID, listing.TimeSlotPatch{Weekdays: ptr(sat | mon)})
	_, code = appCode(t, err)
	require.Equal(t, "overlapping_time_slot", code)
	_, err = e.Svc.UpdateTimeSlot(ctx, p.ID, p.OwnerUserID, l.ID, a.ID, listing.TimeSlotPatch{EndTime: ptr("09:30")})
	require.NoError(t, err)

	for _, in := range []listing.TimeSlotInput{
		{StartTime: "10:00", EndTime: "10:00", Weekdays: mon},
		{StartTime: "11:00", EndTime: "10:00", Weekdays: mon},
		{StartTime: "8h", EndTime: "10:00", Weekdays: mon},
		{StartTime: "08:00", EndTime: "24:00", Weekdays: mon},
		{StartTime: "13:00", EndTime: "14:00", Weekdays: 0},
		{StartTime: "13:00", EndTime: "14:00", Weekdays: 128},
	} {
		_, err := e.Svc.CreateTimeSlot(ctx, p.ID, p.OwnerUserID, l.ID, in)
		st, code := appCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, st, in)
		require.Equal(t, "invalid_listing", code, in)
	}
}

func TestTimeSlot_InactiveDoesNotBlockOverlap(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")

	a, err := e.slot(t, p, l, "08:00", "10:00", mon)
	require.NoError(t, err)
	_, err = e.Svc.UpdateTimeSlot(ctx, p.ID, p.OwnerUserID, l.ID, a.ID, listing.TimeSlotPatch{Active: ptr(false)})
	require.NoError(t, err)
	_, err = e.slot(t, p, l, "09:00", "11:00", mon)
	require.NoError(t, err)

	// Re-activating the old slot now clashes with the new one.
	_, err = e.Svc.UpdateTimeSlot(ctx, p.ID, p.OwnerUserID, l.ID, a.ID, listing.TimeSlotPatch{Active: ptr(true)})
	_, code := appCode(t, err)
	require.Equal(t, "overlapping_time_slot", code)

	// While it stays inactive it can still be edited.
	_, err = e.Svc.UpdateTimeSlot(ctx, p.ID, p.OwnerUserID, l.ID, a.ID, listing.TimeSlotPatch{EndTime: ptr("09:30")})
	require.NoError(t, err)
}

func TestSlotsForDate_WeekdayAndClosed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	_, err := e.slot(t, p, l, "08:00", "10:00", mon)
	require.NoError(t, err)
	_, err = e.slot(t, p, l, "13:00", "15:00", sun)
	require.NoError(t, err)
	inactive, err := e.slot(t, p, l, "10:00", "12:00", mon)
	require.NoError(t, err)
	_, err = e.Svc.UpdateTimeSlot(ctx, p.ID, p.OwnerUserID, l.ID, inactive.ID, listing.TimeSlotPatch{Active: ptr(false)})
	require.NoError(t, err)

	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	slots := func(d time.Time) []listing.TimeSlot {
		var out []listing.TimeSlot
		inTx(t, e, func(tx pgx.Tx) error {
			var err error
			out, err = e.Svc.SlotsForDate(ctx, tx, l.ID, d)
			return err
		})
		return out
	}
	require.Len(t, slots(day(9)), 1) // Monday
	require.Equal(t, "08:00", slots(day(9))[0].StartTime)
	require.Empty(t, slots(day(10))) // Tuesday
	require.Len(t, slots(day(15)), 1)
	require.Equal(t, "13:00", slots(day(15))[0].StartTime) // Sunday

	require.NoError(t, e.Svc.AddClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, day(9), ptr("Bảo trì")))
	require.Empty(t, slots(day(9)))
	require.Len(t, slots(day(15)), 1)
	require.NoError(t, e.Svc.RemoveClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, day(9)))
	require.Len(t, slots(day(9)), 1)
}

func TestSlotsForDate_UsesVietnamDate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	_, err := e.slot(t, p, l, "08:00", "10:00", mon)
	require.NoError(t, err)

	// 17:30Z on Sunday 8 Nov is 00:30 Monday 9 Nov in Vietnam.
	at := time.Date(2026, 11, 8, 17, 30, 0, 0, time.UTC)
	inTx(t, e, func(tx pgx.Tx) error {
		got, err := e.Svc.SlotsForDate(ctx, tx, l.ID, at)
		require.NoError(t, err)
		require.Len(t, got, 1)
		got, err = e.Svc.SlotsForDate(ctx, tx, l.ID, at.Add(-time.Hour))
		require.NoError(t, err)
		require.Empty(t, got) // 16:30Z is still Sunday in Vietnam
		return nil
	})
	// A closed date given as the same instant closes Monday 9 Nov.
	require.NoError(t, e.Svc.AddClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, at, nil))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM attraction_closed_dates WHERE date = '2026-11-09'`))
}

func TestTicketTypesForBooking_InactiveOrForeign422(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l1 := e.create(t, p, "Vé 1")
	l2 := e.create(t, p, "Vé 2")
	a := e.ticket(t, p, l1, "A", 5000)
	b := e.ticket(t, p, l1, "B", 6000)
	other := e.ticket(t, p, l2, "C", 7000)
	_, err := e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l1.ID, b.ID, listing.TicketTypePatch{Active: ptr(false)})
	require.NoError(t, err)

	inTx(t, e, func(tx pgx.Tx) error {
		m, err := e.Svc.TicketTypesForBooking(ctx, tx, l1.ID, []uuid.UUID{a.ID, a.ID})
		require.NoError(t, err)
		require.Len(t, m, 1)
		require.Equal(t, int64(5000), m[a.ID].Price)
		return nil
	})
	for _, ids := range [][]uuid.UUID{{b.ID}, {other.ID}, {a.ID, uuid.New()}} {
		err := db.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
			_, err := e.Svc.TicketTypesForBooking(ctx, tx, l1.ID, ids)
			return err
		})
		st, code := appCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, st)
		require.Equal(t, "invalid_ticket_type", code)
	}
}

func TestClosedDate_PastRejected(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")

	// testNow is 2026-11-04 09:00 ICT.
	err := e.Svc.AddClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), nil)
	st, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, st)
	require.Equal(t, "invalid_closed_date", code)
	today := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	require.NoError(t, e.Svc.AddClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, today, ptr("Hôm nay")))

	// Duplicate is 409.
	err = e.Svc.AddClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, today, nil)
	st, code = appCode(t, err)
	require.Equal(t, http.StatusConflict, st)
	require.Equal(t, "closed_date_exists", code)

	// Removing a missing date is 404.
	err = e.Svc.RemoveClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	st, _ = appCode(t, err)
	require.Equal(t, http.StatusNotFound, st)

	// Detail lists closed dates from today on: today is there, tomorrow it is gone.
	inTx(t, e, func(tx pgx.Tx) error {
		d, err := e.Svc.AttractionDetail(ctx, tx, l.ID, true)
		require.NoError(t, err)
		require.Len(t, d.ClosedDates, 1)
		require.Equal(t, "2026-11-04", d.ClosedDates[0].Date)
		return nil
	})
	e.Clock.T = e.Clock.T.AddDate(0, 0, 1)
	inTx(t, e, func(tx pgx.Tx) error {
		d, err := e.Svc.AttractionDetail(ctx, tx, l.ID, true)
		require.NoError(t, err)
		require.Empty(t, d.ClosedDates)
		return nil
	})
}

func TestAttraction_InfoAndAudit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	base := len(e.audits(t, l.ID))

	info, err := e.Svc.SetAttractionInfo(ctx, p.ID, p.OwnerUserID, l.ID, ptr("Mở 8h-17h"), ptr("Đến cổng chính"))
	require.NoError(t, err)
	require.Equal(t, ptr("Mở 8h-17h"), info.OpeningNote)
	info, err = e.Svc.SetAttractionInfo(ctx, p.ID, p.OwnerUserID, l.ID, ptr("Mở 7h-17h"), nil)
	require.NoError(t, err)
	require.Nil(t, info.CheckinInstructions)

	_, err = e.Svc.SetAttractionInfo(ctx, p.ID, p.OwnerUserID, l.ID, ptr(strings.Repeat("ạ", 2001)), nil)
	st, _ := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, st)
	_, err = e.Svc.SetAttractionInfo(ctx, p.ID, p.OwnerUserID, l.ID, ptr(strings.Repeat("ạ", 2000)), nil)
	require.NoError(t, err)

	// Every write logs one audit row, with old and new values.
	_, err = e.slot(t, p, l, "08:00", "10:00", mon)
	require.NoError(t, err)
	d := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, e.Svc.AddClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, d, nil))
	require.NoError(t, e.Svc.RemoveClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, d))
	au := e.audits(t, l.ID)
	require.Len(t, au, base+6)
	require.Equal(t, "attraction", au[base+1].Action)
	require.Equal(t, "Mở 8h-17h", au[base+1].Changes["opening_note"].Old)
	require.Equal(t, "Mở 7h-17h", au[base+1].Changes["opening_note"].New)
	require.Equal(t, "time_slot", au[base+3].Action)
	require.Equal(t, "closed_date", au[base+4].Action)
	require.Equal(t, "closed_date", au[base+5].Action)
}

func TestAttraction_SuspendedPartner403(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, p.ID, "Vi phạm"))
	_, err := e.Svc.CreateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, listing.TicketTypeInput{Name: "A", Audience: "adult", Price: 5000})
	st, code := appCode(t, err)
	require.Equal(t, http.StatusForbidden, st)
	require.Equal(t, "partner_suspended", code)
}

func TestAttraction_OtherPartner404(t *testing.T) {
	e := newEnv(t)
	a := e.partner(t, "Alpha", attraction)
	b := e.partner(t, "Beta", attraction)
	lb := e.create(t, b, "Vé của B")
	ttB := e.ticket(t, b, lb, "Vé B", 5000)
	slB, err := e.slot(t, b, lb, "08:00", "10:00", mon)
	require.NoError(t, err)
	tokA := e.token(t, a.OwnerUserID)
	base := "/v1/partner/listings/" + lb.ID.String()

	for _, r := range []struct {
		method, path string
		body         any
	}{
		{"PUT", base + "/attraction", map[string]any{"opening_note": "hack"}},
		{"GET", base + "/attraction", nil},
		{"POST", base + "/ticket-types", map[string]any{"name": "X", "audience": "adult", "price": 5000}},
		{"PATCH", base + "/ticket-types/" + ttB.ID.String(), map[string]any{"price": 1000}},
		{"POST", base + "/time-slots", map[string]any{"start_time": "11:00", "end_time": "12:00", "weekdays": 1}},
		{"PATCH", base + "/time-slots/" + slB.ID.String(), map[string]any{"active": false}},
		{"PUT", base + "/closed-dates/2099-01-01", map[string]any{}},
		{"DELETE", base + "/closed-dates/2099-01-01", nil},
	} {
		requireCode(t, e.do(t, r.method, r.path, tokA, r.body), http.StatusNotFound, "not_found")
	}
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM attraction_ticket_types`))
	require.Equal(t, 5000, e.count(t, `SELECT price FROM attraction_ticket_types`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM attraction_time_slots WHERE active`))
	require.Zero(t, e.count(t, `SELECT count(*) FROM attraction_closed_dates`))
}

func TestAttraction_HTTPFlow(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	tok := e.token(t, p.OwnerUserID)
	base := "/v1/partner/listings/" + l.ID.String()

	require.Equal(t, http.StatusOK, e.do(t, "PUT", base+"/attraction", tok, map[string]any{"opening_note": "8h"}).Code)
	rec := e.do(t, "POST", base+"/ticket-types", tok, map[string]any{"name": "NL", "audience": "adult", "price": 5000})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.do(t, "POST", base+"/time-slots", tok, map[string]any{"start_time": "08:00", "end_time": "10:00", "weekdays": 3})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	requireCode(t, e.do(t, "POST", base+"/time-slots", tok, map[string]any{"start_time": "09:00", "end_time": "11:00", "weekdays": 1}),
		http.StatusUnprocessableEntity, "overlapping_time_slot")
	require.Equal(t, http.StatusNoContent, e.do(t, "PUT", base+"/closed-dates/2099-01-01", tok, map[string]any{"reason": "Tết"}).Code)
	requireCode(t, e.do(t, "PUT", base+"/closed-dates/2099-01-01", tok, map[string]any{}), http.StatusConflict, "closed_date_exists")
	requireCode(t, e.do(t, "PUT", base+"/closed-dates/not-a-date", tok, map[string]any{}), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "PUT", base+"/closed-dates/2099-01-02", tok, map[string]any{"zzz": 1}), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "POST", base+"/ticket-types", tok, map[string]any{"name": "NL", "audience": "adult", "price": 5000, "x": 1}),
		http.StatusBadRequest, "invalid_request")

	rec = e.do(t, "GET", base+"/attraction", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"08:00"`)
	require.Contains(t, rec.Body.String(), `"2099-01-01"`)
	require.Equal(t, http.StatusNoContent, e.do(t, "DELETE", base+"/closed-dates/2099-01-01", tok, nil).Code)
	requireCode(t, e.do(t, "DELETE", base+"/closed-dates/2099-01-01", tok, nil), http.StatusNotFound, "not_found")
}
