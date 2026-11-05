package booking_test

import (
	"context"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

func vnDay(s string) time.Time {
	d, err := time.ParseInLocation("2006-01-02", s, clock.VN)
	if err != nil {
		panic(err)
	}
	return d
}

func (e *env) closeDate(day string) error {
	return e.Listing.AddClosedDate(context.Background(), e.P.ID, e.P.OwnerUserID, e.L.ID, vnDay(day), nil)
}

func (e *env) patchSlot(p listing.TimeSlotPatch) error {
	_, err := e.Listing.UpdateTimeSlot(context.Background(), e.P.ID, e.P.OwnerUserID, e.L.ID, e.Slot.ID, p)
	return err
}

func TestCloseDateWithConfirmedBooking409(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, v.ID))

	requireErr(t, e.closeDate(visitDate), http.StatusConflict, "has_active_bookings")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM attraction_closed_dates`))
	// Another date has no booking.
	require.NoError(t, e.closeDate("2026-11-08"))

	// A pending_payment booking blocks too; a cancelled one does not.
	in := e.input()
	in.VisitDate = "2026-11-10"
	p, err := e.Svc.CreateAttraction(context.Background(), e.Customer, "pending", in)
	require.NoError(t, err)
	require.Equal(t, booking.StatusPendingPayment, p.Status)
	requireErr(t, e.closeDate("2026-11-10"), http.StatusConflict, "has_active_bookings")
	e.cancel(t, v.ID)
	require.NoError(t, e.closeDate(visitDate))
}

func TestDeactivateSlotWithFutureBooking409(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	off, start, wd := false, "09:00", 0b0011111

	requireErr(t, e.patchSlot(listing.TimeSlotPatch{Active: &off}), http.StatusConflict, "has_active_bookings")
	requireErr(t, e.patchSlot(listing.TimeSlotPatch{StartTime: &start}), http.StatusConflict, "has_active_bookings")
	requireErr(t, e.patchSlot(listing.TimeSlotPatch{Weekdays: &wd}), http.StatusConflict, "has_active_bookings")
	require.Equal(t, "08:00", e.str(t, `SELECT to_char(start_time, 'HH24:MI') FROM attraction_time_slots WHERE id = $1`, e.Slot.ID))

	// A booking whose date is before today (VN) no longer blocks.
	e.Clock.T = time.Date(2026, 11, 8, 9, 0, 0, 0, clock.VN)
	require.Equal(t, booking.StatusConfirmed, e.bookingStatus(t, v.ID))
	require.NoError(t, e.patchSlot(listing.TimeSlotPatch{Active: &off}))
}

func TestChangeSlotTimeWithoutBookingsOK(t *testing.T) {
	e := newEnv(t)
	start, end, wd := "09:00", "18:00", 0b0011111
	require.NoError(t, e.patchSlot(listing.TimeSlotPatch{StartTime: &start, EndTime: &end, Weekdays: &wd}))
	require.Equal(t, "09:00", e.str(t, `SELECT to_char(start_time, 'HH24:MI') FROM attraction_time_slots WHERE id = $1`, e.Slot.ID))
	require.NoError(t, e.closeDate(visitDate))
}

// TestRescheduleWaitsForPartnerScheduleWrite: a reschedule takes the listing
// FOR SHARE before reading the slots, so a partner write holding the listing
// FOR NO KEY UPDATE (here: closing the target date) is seen once it commits.
func TestRescheduleWaitsForPartnerScheduleWrite(t *testing.T) {
	e := newEnv(t)
	v := e.paidBooking(t)
	ctx := context.Background()
	tx, err := e.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT 1 FROM listings WHERE id = $1 FOR NO KEY UPDATE`, e.L.ID)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := e.Svc.Reschedule(ctx, e.Customer, v.ID, newVisit, e.Slot.ID)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("reschedule did not wait for the listing lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	_, err = tx.Exec(ctx, `INSERT INTO attraction_closed_dates (listing_id, date) VALUES ($1, $2)`, e.L.ID, newVisit)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	requireErr(t, <-done, http.StatusUnprocessableEntity, "slot_unavailable")
	require.Equal(t, visitDate, e.str(t, `SELECT visit_date::text FROM booking_attraction_details WHERE booking_id = $1`, v.ID))
}

// Ruling 24: a weekday edit is guarded only for the removed weekdays; adding
// weekdays is always allowed. The fixture booking is on Saturday 2026-11-07.
func TestSlotWeekdayGuardChecksOnlyRemovedWeekdays(t *testing.T) {
	e := newEnv(t)
	e.paidBooking(t)
	noMonday, all, noSaturday := 0b1111110, everyDay, 0b1011111

	require.NoError(t, e.patchSlot(listing.TimeSlotPatch{Weekdays: &noMonday}), "remove an unbooked weekday")
	require.NoError(t, e.patchSlot(listing.TimeSlotPatch{Weekdays: &all}), "add a weekday with bookings on the slot")
	requireErr(t, e.patchSlot(listing.TimeSlotPatch{Weekdays: &noSaturday}), http.StatusConflict, "has_active_bookings")
	end := "18:00"
	requireErr(t, e.patchSlot(listing.TimeSlotPatch{EndTime: &end}), http.StatusConflict, "has_active_bookings")
	require.Equal(t, everyDay, e.count(t, `SELECT weekdays FROM attraction_time_slots WHERE id = $1`, e.Slot.ID))
}

// Ruling 25: a slot whose times changed after the hold expired is not the
// service that was sold: the late transfer is refunded in full, no ledger.
func TestLatePaid_RefundsWhenSlotTimeChanged(t *testing.T) {
	e := newEnv(t)
	v := e.create(t, "k")
	oc := e.orderCode(t, v.ID)
	e.expire(t, v.ID)
	start := "09:00"
	require.NoError(t, e.patchSlot(listing.TimeSlotPatch{StartTime: &start}), "the expired booking no longer blocks")
	e.pay(t, oc, gross)
	e.requireLateRefund(t, v, oc)
	require.NoError(t, e.Ledger.Verify(context.Background(), e.P.ID))
}

// --- Rental peak concurrency (plan 7 Task 3) ---

// guardRentalEnv is env plus a rental partner whose listing has a store, one
// pickup point and two models: Vision (motorbike, quantity 2) and Vios (car,
// quantity 1). Holds are inserted with raw SQL.
type guardRentalEnv struct {
	*env
	RP     partner.Partner
	RL     listing.Listing
	Point  listing.PickupPoint
	Vision listing.RentalModel
	Vios   listing.RentalModel
}

func newGuardRentalEnv(t *testing.T) *guardRentalEnv {
	t.Helper()
	ctx := context.Background()
	e := newBareEnv(t)
	r := &guardRentalEnv{env: e}
	p, err := e.Partner.Create(ctx, e.Admin, partner.CreateInput{
		LegalName: "Công ty Thuê Xe", DisplayName: "Thuê Xe",
		OwnerEmail: "owner@thuexe.vn", OwnerPassword: "matkhau123",
		ServiceTypes: []string{"self_drive_rental"},
	})
	require.NoError(t, err)
	r.RP = p
	r.RL, err = e.Listing.Create(ctx, p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: "self_drive_rental", Title: "Thuê xe Đà Lạt"})
	require.NoError(t, err)
	_, err = e.Listing.SetRentalStore(ctx, p.ID, p.OwnerUserID, r.RL.ID, listing.RentalStoreInput{HandoverOpen: "06:00", HandoverClose: "22:00"})
	require.NoError(t, err)
	var province, ward string
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT province_code, code FROM wards ORDER BY code LIMIT 1`).Scan(&province, &ward))
	r.Point, err = e.Listing.CreatePickupPoint(ctx, p.ID, p.OwnerUserID, r.RL.ID, listing.PickupPointInput{
		Name: "Cửa hàng", Address: "1 Trần Phú", ProvinceCode: province, WardCode: ward,
	})
	require.NoError(t, err)
	r.Vision = r.model(t, "motorbike", "Honda Vision", 150_000, 2)
	r.Vios = r.model(t, "car", "Toyota Vios", 800_000, 1)
	return r
}

func (r *guardRentalEnv) model(t *testing.T, kind, name string, price int64, qty int) listing.RentalModel {
	t.Helper()
	m, err := r.Listing.CreateRentalModel(context.Background(), r.RP.ID, r.RP.OwnerUserID, r.RL.ID, listing.RentalModelInput{
		VehicleKind: kind, Name: name, ModelYear: 2023, Seats: 2, PricePerDay: price, Quantity: qty,
	})
	require.NoError(t, err)
	return m
}

// hold inserts a requested rental booking of model over [pickup, ret) that
// holds the vehicle until holdUntil (nil: with no deadline, as once confirmed).
func (r *guardRentalEnv) hold(t *testing.T, m listing.RentalModel, pickup, ret time.Time, holdUntil *time.Time) uuid.UUID {
	t.Helper()
	customer := r.customer(t, uuid.NewString()+"@gmail.com")
	checkout, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	due := pickup.Add(-2 * time.Hour)
	if holdUntil != nil {
		due = *holdUntil
	}
	r.exec(t, `INSERT INTO checkouts (id, code, customer_id, total_amount, status) VALUES ($1, $2, $3, 300000, 'open')`,
		checkout, guardCode("CO"), customer)
	r.exec(t, `INSERT INTO bookings (
		id, code, checkout_id, customer_id, partner_id, listing_id, service_type, booking_mode, status,
		service_start_at, service_end_at, original_service_start_at,
		gross_amount, commission_rate_bps, commission_amount, cancellation_policy_snapshot,
		contact_name, contact_phone, contact_email, response_due_at, qr_token
	) VALUES ($1, $2, $3, $4, $5, $6, 'self_drive_rental', 'request', 'requested', $7, $8, $7,
		300000, 1000, 30000, '[]', 'A', '0900000000', 'a@customer.test', $9, $10)`,
		id, guardCode("LT"), checkout, customer, r.RP.ID, r.RL.ID, pickup, ret, due, uuid.NewString())
	r.exec(t, `INSERT INTO booking_rental_details (
		booking_id, listing_id, vehicle_model_id, pickup_point_id, pickup_at, return_at, days,
		model_name, pickup_point_name, pickup_address, destination_province_code, holds_vehicle, hold_until)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7, 'Cửa hàng', '1 Trần Phú', $8, true, $9)`,
		id, r.RL.ID, m.ID, r.Point.ID, pickup, ret, m.Name, r.Point.ProvinceCode, holdUntil)
	return id
}

// guardCode is a random booking/checkout code (prefix + 8 Crockford chars).
func guardCode(prefix string) string {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	out := []byte(prefix)
	for _, c := range b {
		out = append(out, crockford[int(c)%len(crockford)])
	}
	return string(out)
}

// peak runs Guard.RentalPeakHeld in its own tx at the env clock.
func (r *guardRentalEnv) peak(t *testing.T, ids []uuid.UUID, from time.Time, to *time.Time) map[uuid.UUID]int {
	t.Helper()
	ctx := context.Background()
	var out map[uuid.UUID]int
	require.NoError(t, db.InTx(ctx, r.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = booking.Guard{}.RentalPeakHeld(ctx, tx, ids, from, to, r.Clock.Now())
		return err
	}))
	return out
}

// mon is Monday 2026-11-09 08:00 (VN) plus d days.
func mon(d int) time.Time { return vnAt(2026, 11, 9+d, 8, 0) }

func timePtr(t time.Time) *time.Time { return &t }

func TestGuard_RentalPeakHeld_DisjointHolds(t *testing.T) {
	r := newGuardRentalEnv(t)
	r.hold(t, r.Vision, mon(0), mon(1), nil)
	r.hold(t, r.Vision, mon(2), mon(3), nil)
	got := r.peak(t, []uuid.UUID{r.Vision.ID}, mon(0), timePtr(mon(3)))
	require.Equal(t, map[uuid.UUID]int{r.Vision.ID: 1}, got)
}

func TestGuard_RentalPeakHeld_OverlappingHolds(t *testing.T) {
	r := newGuardRentalEnv(t)
	vision := []uuid.UUID{r.Vision.ID}
	r.hold(t, r.Vision, mon(0), mon(2), nil)
	r.hold(t, r.Vision, mon(1), mon(3), nil)
	require.Equal(t, map[uuid.UUID]int{r.Vision.ID: 2}, r.peak(t, vision, mon(1), timePtr(mon(2))))
	// Windows touching only one of the holds see one.
	require.Equal(t, map[uuid.UUID]int{r.Vision.ID: 1}, r.peak(t, vision, mon(0), timePtr(mon(1))))
	require.Equal(t, map[uuid.UUID]int{r.Vision.ID: 1}, r.peak(t, vision, mon(2), timePtr(mon(3))))
	// A window ending where both holds start, or starting where both end, sees none.
	require.Empty(t, r.peak(t, vision, mon(-1), timePtr(mon(0))))
	require.Empty(t, r.peak(t, vision, mon(3), nil))
}

func TestGuard_RentalPeakHeld_Edges(t *testing.T) {
	r := newGuardRentalEnv(t)
	ctx := context.Background()
	vision := []uuid.UUID{r.Vision.ID}

	// Back-to-back holds never overlap: [0,1) and [1,2) peak at 1.
	a := r.hold(t, r.Vision, mon(0), mon(1), nil)
	r.hold(t, r.Vision, mon(1), mon(2), nil)
	require.Equal(t, map[uuid.UUID]int{r.Vision.ID: 1}, r.peak(t, vision, mon(0), timePtr(mon(2))))

	// A requested booking at or past its hold_until (the job has not swept it
	// yet) is not counted; one still inside it is.
	r.hold(t, r.Vision, mon(0), mon(2), timePtr(r.Clock.Now()))
	require.Equal(t, map[uuid.UUID]int{r.Vision.ID: 1}, r.peak(t, vision, mon(0), timePtr(mon(2))))
	r.hold(t, r.Vision, mon(0), mon(2), timePtr(r.Clock.Now().Add(time.Hour)))
	require.Equal(t, map[uuid.UUID]int{r.Vision.ID: 2}, r.peak(t, vision, mon(0), timePtr(mon(2))))

	// except leaves the booking itself out.
	require.NoError(t, db.InTx(ctx, r.Pool, func(tx pgx.Tx) error {
		got, err := booking.PeakRentalHolds(ctx, tx, vision, mon(0), timePtr(mon(1)), r.Clock.Now(), a)
		require.NoError(t, err)
		require.Equal(t, map[uuid.UUID]int{r.Vision.ID: 1}, got)
		return nil
	}))

	// Open-ended window over Vios holds [10,12), [11,13), [12,14) days: the
	// first and third only touch, so the peak is 2.
	vios := []uuid.UUID{r.Vios.ID}
	r.hold(t, r.Vios, mon(10), mon(12), nil)
	r.hold(t, r.Vios, mon(11), mon(13), nil)
	r.hold(t, r.Vios, mon(12), mon(14), nil)
	require.Equal(t, map[uuid.UUID]int{r.Vios.ID: 2}, r.peak(t, vios, mon(9), nil))
	require.Equal(t, map[uuid.UUID]int{r.Vios.ID: 2}, r.peak(t, vios, mon(12), nil), "the clipped [12,13) and [12,14)")
	require.Equal(t, map[uuid.UUID]int{r.Vios.ID: 1}, r.peak(t, vios, mon(13), nil))

	// Two models answer two numbers; a model without holds is absent.
	both := r.peak(t, []uuid.UUID{r.Vision.ID, r.Vios.ID}, mon(0), nil)
	require.Equal(t, map[uuid.UUID]int{r.Vision.ID: 2, r.Vios.ID: 2}, both)
	scooter := r.model(t, "motorbike", "Yamaha Janus", 120_000, 3)
	require.Equal(t, map[uuid.UUID]int{r.Vision.ID: 2}, r.peak(t, []uuid.UUID{r.Vision.ID, scooter.ID}, mon(0), timePtr(mon(2))))
}
