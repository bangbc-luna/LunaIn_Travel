package listing_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

const ride = "scheduled_ride"

// rideNow is Thursday 2026-11-05 09:00 Vietnam time.
var rideNow = time.Date(2026, 11, 5, 9, 0, 0, 0, clock.VN)

// rideEnv is newEnv with the clock at rideNow and a partner selling rides.
func rideEnv(t *testing.T) (*env, partner.Partner) {
	t.Helper()
	e := newEnv(t)
	e.Clock.T = rideNow
	return e, e.partner(t, "Alpha", ride, attraction)
}

func (e *env) rideListing(t *testing.T, p partner.Partner, title string) listing.Listing {
	t.Helper()
	l, err := e.Svc.Create(context.Background(), p.ID, p.OwnerUserID, listing.CreateInput{
		ServiceType: ride, Title: title, Description: ptr("Xe giường nằm"),
	})
	require.NoError(t, err)
	return l
}

// wardIn returns the first ward (by code) of province.
func (e *env) wardIn(t *testing.T, province string) string {
	t.Helper()
	var w string
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT code FROM wards WHERE province_code = $1 ORDER BY code LIMIT 1`, province).Scan(&w))
	return w
}

// hcmToDalat is a route TP.HCM (79) -> Lâm Đồng (68).
func (e *env) hcmToDalat(t *testing.T) listing.RideRouteInput {
	t.Helper()
	return listing.RideRouteInput{
		Origin:             listing.RidePoint{ProvinceCode: "79", WardCode: e.wardIn(t, "79"), Name: "Bến xe Miền Đông", Address: "292 Đinh Bộ Lĩnh"},
		Dest:               listing.RidePoint{ProvinceCode: "68", WardCode: e.wardIn(t, "68"), Name: "Bến xe Đà Lạt", Address: "1 Tô Hiến Thành"},
		DurationMinutes:    420,
		VehicleDescription: "Xe giường nằm 34 chỗ",
	}
}

// routed is a ride listing with the HCM -> Đà Lạt route.
func (e *env) routed(t *testing.T, p partner.Partner) listing.Listing {
	t.Helper()
	l := e.rideListing(t, p, "Sài Gòn - Đà Lạt")
	_, err := e.Svc.SetRideRoute(context.Background(), p.ID, p.OwnerUserID, l.ID, e.hcmToDalat(t))
	require.NoError(t, err)
	return l
}

func (e *env) schedule(t *testing.T, p partner.Partner, l listing.Listing, in listing.RideScheduleInput) listing.RideSchedule {
	t.Helper()
	s, err := e.Svc.CreateRideSchedule(context.Background(), p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
	return s
}

func daily(at string) listing.RideScheduleInput {
	return listing.RideScheduleInput{DepartureTime: at, Weekdays: 127, SeatCapacity: 30, PricePerSeat: 200_000, ValidFrom: "2026-11-05"}
}

type tripRow struct {
	ID            uuid.UUID
	ScheduleID    *uuid.UUID
	DepartureAt   time.Time
	SeatCapacity  int
	SeatsReserved int
	Price         int64
	Status        string
	CancelSource  *string
}

// trips returns the trips of a listing by departure.
func (e *env) trips(t *testing.T, listingID uuid.UUID) []tripRow {
	t.Helper()
	rows, err := e.Pool.Query(context.Background(), `SELECT id, schedule_id, departure_at, seat_capacity, seats_reserved,
		price_per_seat, status, cancel_source FROM ride_trips WHERE listing_id = $1 ORDER BY departure_at`, listingID)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (tripRow, error) {
		var x tripRow
		err := r.Scan(&x.ID, &x.ScheduleID, &x.DepartureAt, &x.SeatCapacity, &x.SeatsReserved, &x.Price, &x.Status, &x.CancelSource)
		return x, err
	})
	require.NoError(t, err)
	return out
}

// tripAt returns the trip of listingID departing at the given VN wall time.
func (e *env) tripAt(t *testing.T, listingID uuid.UUID, at time.Time) tripRow {
	t.Helper()
	for _, x := range e.trips(t, listingID) {
		if x.DepartureAt.Equal(at) {
			return x
		}
	}
	t.Fatalf("no trip at %s", at)
	return tripRow{}
}

func vnAt(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, clock.VN)
}

// reserve marks seats as held on the trip (as a booking would).
func (e *env) reserve(t *testing.T, tripID uuid.UUID, seats int) {
	t.Helper()
	_, err := e.Pool.Exec(context.Background(), `UPDATE ride_trips SET seats_reserved = $2 WHERE id = $1`, tripID, seats)
	require.NoError(t, err)
}

func TestSetRideRoute_UpsertAndAudit(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.rideListing(t, p, "Sài Gòn - Đà Lạt")
	in := e.hcmToDalat(t)
	in.Origin.Name = "  Bến xe Miền Đông  "

	r, err := e.Svc.SetRideRoute(ctx, p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
	require.Equal(t, l.ID, r.ListingID)
	require.Equal(t, "Bến xe Miền Đông", r.Origin.Name)
	require.Equal(t, "68", r.Dest.ProvinceCode)
	require.Equal(t, 420, r.DurationMinutes)

	in.VehicleDescription = "Limousine 22 chỗ"
	in.DurationMinutes = 360
	r, err = e.Svc.SetRideRoute(ctx, p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
	require.Equal(t, "Limousine 22 chỗ", r.VehicleDescription)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ride_routes WHERE listing_id = $1 AND duration_minutes = 360`, l.ID))

	got, err := e.Svc.GetRide(ctx, p.ID, l.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Route)
	require.Equal(t, "Limousine 22 chỗ", got.Route.VehicleDescription)
	require.Empty(t, got.Schedules)

	var routeAudits []auditRow
	for _, a := range e.audits(t, l.ID) {
		if a.Action == "ride_route" {
			routeAudits = append(routeAudits, a)
		}
	}
	require.Len(t, routeAudits, 2)
	require.Equal(t, p.OwnerUserID, routeAudits[1].Actor)
	require.Equal(t, "Limousine 22 chỗ", routeAudits[1].Changes["vehicle_description"].New)
	require.NotContains(t, routeAudits[1].Changes, "origin_name")

	// Invalid input.
	bad := e.hcmToDalat(t)
	bad.DurationMinutes = 0
	_, err = e.Svc.SetRideRoute(ctx, p.ID, p.OwnerUserID, l.ID, bad)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_ride")
	bad = e.hcmToDalat(t)
	bad.Dest = bad.Origin
	_, err = e.Svc.SetRideRoute(ctx, p.ID, p.OwnerUserID, l.ID, bad)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_ride")
	bad = e.hcmToDalat(t)
	bad.VehicleDescription = " "
	_, err = e.Svc.SetRideRoute(ctx, p.ID, p.OwnerUserID, l.ID, bad)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_ride")
}

func TestSetRideRoute_WardNotInProvince422(t *testing.T) {
	e, p := rideEnv(t)
	l := e.rideListing(t, p, "Sài Gòn - Đà Lạt")
	in := e.hcmToDalat(t)
	in.Dest.WardCode = e.wardIn(t, "79")
	_, err := e.Svc.SetRideRoute(context.Background(), p.ID, p.OwnerUserID, l.ID, in)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_location")
	require.Zero(t, e.count(t, `SELECT count(*) FROM ride_routes`))
}

func TestSetRideRoute_OnAttraction422(t *testing.T) {
	e, p := rideEnv(t)
	a := e.create(t, p, "Vé A")
	_, err := e.Svc.SetRideRoute(context.Background(), p.ID, p.OwnerUserID, a.ID, e.hcmToDalat(t))
	requireErr(t, err, http.StatusUnprocessableEntity, "wrong_service_type")
	require.Contains(t, err.Error(), "Listing xe tuyến")
	_, err = e.Svc.CreateRideSchedule(context.Background(), p.ID, p.OwnerUserID, a.ID, daily("07:00"))
	requireErr(t, err, http.StatusUnprocessableEntity, "wrong_service_type")

	// Another partner's ride listing is 404.
	other := e.partner(t, "Beta", ride)
	l := e.rideListing(t, other, "Tuyến B")
	_, err = e.Svc.SetRideRoute(context.Background(), p.ID, p.OwnerUserID, l.ID, e.hcmToDalat(t))
	requireErr(t, err, http.StatusNotFound, "not_found")
}

func TestSetRideRoute_ChangeOriginWithBookedTrip409(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	e.schedule(t, p, l, daily("07:00"))
	e.reserve(t, e.tripAt(t, l.ID, vnAt(2026, 11, 7, 7, 0)).ID, 2)

	in := e.hcmToDalat(t)
	var otherWard string
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT code FROM wards WHERE province_code = '79' AND code <> $1 ORDER BY code LIMIT 1`,
		in.Origin.WardCode).Scan(&otherWard))
	in.Origin.WardCode = otherWard
	_, err := e.Svc.SetRideRoute(ctx, p.ID, p.OwnerUserID, l.ID, in)
	requireErr(t, err, http.StatusConflict, "has_active_bookings")

	// Names, addresses, duration and vehicle may still change.
	in = e.hcmToDalat(t)
	in.Origin.Name = "Bến xe Miền Đông mới"
	in.VehicleDescription = "Limousine"
	_, err = e.Svc.SetRideRoute(ctx, p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)

	// A booked trip in the past does not block.
	_, err = e.Pool.Exec(ctx, `UPDATE ride_trips SET seats_reserved = 0`)
	require.NoError(t, err)
	in.Origin.WardCode = otherWard
	_, err = e.Svc.SetRideRoute(ctx, p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
}

func TestCreateSchedule_NeedsRoute422(t *testing.T) {
	e, p := rideEnv(t)
	l := e.rideListing(t, p, "Sài Gòn - Đà Lạt")
	_, err := e.Svc.CreateRideSchedule(context.Background(), p.ID, p.OwnerUserID, l.ID, daily("07:00"))
	requireErr(t, err, http.StatusUnprocessableEntity, "route_required")
	require.Zero(t, e.count(t, `SELECT count(*) FROM ride_schedules`))
}

func TestCreateSchedule_Validation422(t *testing.T) {
	e, p := rideEnv(t)
	l := e.routed(t, p)
	for name, mod := range map[string]func(*listing.RideScheduleInput){
		"bad time":       func(in *listing.RideScheduleInput) { in.DepartureTime = "7:00" },
		"no weekdays":    func(in *listing.RideScheduleInput) { in.Weekdays = 0 },
		"zero seats":     func(in *listing.RideScheduleInput) { in.SeatCapacity = 0 },
		"too many":       func(in *listing.RideScheduleInput) { in.SeatCapacity = 81 },
		"cheap":          func(in *listing.RideScheduleInput) { in.PricePerSeat = 999 },
		"expensive":      func(in *listing.RideScheduleInput) { in.PricePerSeat = 100_000_001 },
		"past":           func(in *listing.RideScheduleInput) { in.ValidFrom = "2026-11-04" },
		"bad date":       func(in *listing.RideScheduleInput) { in.ValidFrom = "05/11/2026" },
		"to before from": func(in *listing.RideScheduleInput) { in.ValidTo = ptr("2026-11-04") },
	} {
		t.Run(name, func(t *testing.T) {
			in := daily("07:00")
			mod(&in)
			_, err := e.Svc.CreateRideSchedule(context.Background(), p.ID, p.OwnerUserID, l.ID, in)
			requireErr(t, err, http.StatusUnprocessableEntity, "invalid_ride")
		})
	}
	require.Zero(t, e.count(t, `SELECT count(*) FROM ride_schedules`))
}

func TestCreateSchedule_Overlap422(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	weekdays := e.schedule(t, p, l, listing.RideScheduleInput{
		DepartureTime: "07:00", Weekdays: monToFri, SeatCapacity: 30, PricePerSeat: 200_000,
		ValidFrom: "2026-11-05", ValidTo: ptr("2026-11-30"),
	})

	// Same time, shared Monday, overlapping validity.
	_, err := e.Svc.CreateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, listing.RideScheduleInput{
		DepartureTime: "07:00", Weekdays: mon | sat, SeatCapacity: 30, PricePerSeat: 200_000, ValidFrom: "2026-11-20",
	})
	requireErr(t, err, http.StatusUnprocessableEntity, "overlapping_schedule")

	// No shared weekday, another time, or validity after the first: fine.
	e.schedule(t, p, l, listing.RideScheduleInput{DepartureTime: "07:00", Weekdays: sat | sun, SeatCapacity: 30, PricePerSeat: 200_000, ValidFrom: "2026-11-05"})
	e.schedule(t, p, l, listing.RideScheduleInput{DepartureTime: "07:30", Weekdays: mon, SeatCapacity: 30, PricePerSeat: 200_000, ValidFrom: "2026-11-05"})
	later := e.schedule(t, p, l, listing.RideScheduleInput{DepartureTime: "07:00", Weekdays: mon, SeatCapacity: 30, PricePerSeat: 200_000, ValidFrom: "2026-12-01"})

	// Extending the first one now overlaps the later one.
	_, err = e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, weekdays.ID, listing.RideSchedulePatch{ClearValidTo: true})
	requireErr(t, err, http.StatusUnprocessableEntity, "overlapping_schedule")
	// An inactive schedule does not count.
	_, err = e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, later.ID, listing.RideSchedulePatch{Active: ptr(false)})
	require.NoError(t, err)
	_, err = e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, weekdays.ID, listing.RideSchedulePatch{ClearValidTo: true})
	require.NoError(t, err)
	// And reactivating it would overlap again.
	_, err = e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, later.ID, listing.RideSchedulePatch{Active: ptr(true)})
	requireErr(t, err, http.StatusUnprocessableEntity, "overlapping_schedule")
}

func TestCreateSchedule_GeneratesSixtyDays(t *testing.T) {
	e, p := rideEnv(t)
	l := e.routed(t, p)
	s := e.schedule(t, p, l, daily("07:00"))
	require.Equal(t, "07:00", s.DepartureTime)
	require.Equal(t, "2026-11-05", s.ValidFrom)
	require.Nil(t, s.ValidTo)
	require.True(t, s.Active)

	trips := e.trips(t, l.ID)
	require.Len(t, trips, 59)
	require.True(t, trips[0].DepartureAt.Equal(vnAt(2026, 11, 6, 7, 0)), trips[0].DepartureAt)
	require.True(t, trips[58].DepartureAt.Equal(vnAt(2027, 1, 3, 7, 0)), trips[58].DepartureAt)
	for _, x := range trips {
		require.Equal(t, s.ID, *x.ScheduleID)
		require.Equal(t, "scheduled", x.Status)
		require.Equal(t, 30, x.SeatCapacity)
		require.Equal(t, int64(200_000), x.Price)
	}
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ride_schedules WHERE id = $1 AND generated_until = '2027-01-03'`, s.ID))

	got, err := e.Svc.GetRide(context.Background(), p.ID, l.ID)
	require.NoError(t, err)
	require.Len(t, got.Schedules, 1)
	require.Equal(t, s, got.Schedules[0])
}

func TestUpdateSchedule_PriceAppliesToUnbookedOnly(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	s := e.schedule(t, p, l, daily("07:00"))
	booked := e.tripAt(t, l.ID, vnAt(2026, 11, 7, 7, 0))
	e.reserve(t, booked.ID, 2)

	got, err := e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, s.ID, listing.RideSchedulePatch{
		PricePerSeat: ptr(int64(250_000)), SeatCapacity: ptr(40),
	})
	require.NoError(t, err)
	require.Equal(t, int64(250_000), got.PricePerSeat)
	require.Equal(t, 40, got.SeatCapacity)
	for _, x := range e.trips(t, l.ID) {
		if x.ID == booked.ID {
			require.Equal(t, int64(200_000), x.Price)
			require.Equal(t, 30, x.SeatCapacity)
			require.Equal(t, 2, x.SeatsReserved)
			continue
		}
		require.Equal(t, int64(250_000), x.Price)
		require.Equal(t, 40, x.SeatCapacity)
	}
	var found bool
	for _, a := range e.audits(t, l.ID) {
		if a.Action == "ride_schedule" && a.Changes["price_per_seat"].Old != nil {
			found = true
			require.EqualValues(t, 200_000, a.Changes["price_per_seat"].Old)
			require.EqualValues(t, 250_000, a.Changes["price_per_seat"].New)
		}
	}
	require.True(t, found)

	_, err = e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, s.ID, listing.RideSchedulePatch{SeatCapacity: ptr(81)})
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_ride")
	_, err = e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, uuid.Must(uuid.NewV7()), listing.RideSchedulePatch{SeatCapacity: ptr(10)})
	requireErr(t, err, http.StatusNotFound, "not_found")
}

func TestUpdateSchedule_ValidToShortenCancelsLaterTrips(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	s := e.schedule(t, p, l, daily("07:00"))
	booked := e.tripAt(t, l.ID, vnAt(2026, 11, 20, 7, 0))
	e.reserve(t, booked.ID, 1)

	_, err := e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, s.ID, listing.RideSchedulePatch{ValidTo: ptr("2026-11-10")})
	require.NoError(t, err)
	n := 0
	for _, x := range e.trips(t, l.ID) {
		switch {
		case x.ID == booked.ID, x.DepartureAt.Before(vnAt(2026, 11, 11, 0, 0)):
			require.Equal(t, "scheduled", x.Status, x.DepartureAt)
			n++
		default:
			require.Equal(t, "cancelled", x.Status, x.DepartureAt)
			require.Equal(t, "schedule", *x.CancelSource)
		}
	}
	require.Equal(t, 6, n) // 11-06..11-10 and the booked 11-20.
	require.Equal(t, 59-6, e.count(t, `SELECT count(*) FROM ride_trips WHERE cancelled_reason = 'Lịch chạy đã thay đổi' AND cancelled_by = $1`, p.OwnerUserID))

	// Extending again reopens them.
	_, err = e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, s.ID, listing.RideSchedulePatch{ClearValidTo: true})
	require.NoError(t, err)
	require.Zero(t, e.count(t, `SELECT count(*) FROM ride_trips WHERE status <> 'scheduled'`))
	require.Len(t, e.trips(t, l.ID), 59)
}

func TestUpdateSchedule_DeactivateThenReactivateReopens(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	s := e.schedule(t, p, l, daily("07:00"))
	booked := e.tripAt(t, l.ID, vnAt(2026, 11, 7, 7, 0))
	e.reserve(t, booked.ID, 2)
	byPartner := e.tripAt(t, l.ID, vnAt(2026, 11, 8, 7, 0))
	_, err := e.Pool.Exec(ctx, `UPDATE ride_trips SET status = 'cancelled', cancelled_reason = 'Nghỉ', cancelled_at = $2,
		cancel_source = 'partner' WHERE id = $1`, byPartner.ID, rideNow)
	require.NoError(t, err)

	got, err := e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, s.ID, listing.RideSchedulePatch{Active: ptr(false)})
	require.NoError(t, err)
	require.False(t, got.Active)
	for _, x := range e.trips(t, l.ID) {
		switch x.ID {
		case booked.ID:
			require.Equal(t, "scheduled", x.Status)
		case byPartner.ID:
			require.Equal(t, "partner", *x.CancelSource)
		default:
			require.Equal(t, "cancelled", x.Status)
			require.Equal(t, "schedule", *x.CancelSource)
		}
	}
	// The job leaves an inactive schedule alone.
	n, err := e.Svc.GenerateTrips(ctx)
	require.NoError(t, err)
	require.Zero(t, n)

	_, err = e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, s.ID, listing.RideSchedulePatch{Active: ptr(true)})
	require.NoError(t, err)
	for _, x := range e.trips(t, l.ID) {
		if x.ID == byPartner.ID {
			require.Equal(t, "cancelled", x.Status)
			require.Equal(t, "partner", *x.CancelSource)
			continue
		}
		require.Equal(t, "scheduled", x.Status, x.DepartureAt)
		require.Nil(t, x.CancelSource)
	}
	require.Len(t, e.trips(t, l.ID), 59)
}

func TestUpdateSchedule_ImmutableFields422(t *testing.T) {
	e, p := rideEnv(t)
	l := e.routed(t, p)
	s := e.schedule(t, p, l, listing.RideScheduleInput{
		DepartureTime: "07:00", Weekdays: 127, SeatCapacity: 30, PricePerSeat: 200_000, ValidFrom: "2026-11-05", ValidTo: ptr("2026-12-31"),
	})
	tok := e.token(t, p.OwnerUserID)
	path := "/v1/partner/listings/" + l.ID.String() + "/ride-schedules/" + s.ID.String()
	for _, body := range []string{`{"departure_time":"08:00"}`, `{"weekdays":1}`, `{"valid_from":"2026-12-01"}`, `{"weekdays":null}`} {
		requireCode(t, e.do(t, "PATCH", path, tok, body), http.StatusUnprocessableEntity, "invalid_ride")
	}
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ride_schedules WHERE departure_time = '07:00' AND weekdays = 127`))

	// valid_to null clears it; a missing valid_to keeps it.
	rec := e.do(t, "PATCH", path, tok, `{"price_per_seat":210000}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"valid_to":"2026-12-31"`)
	rec = e.do(t, "PATCH", path, tok, `{"valid_to":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"valid_to":null`)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ride_schedules WHERE valid_to IS NULL AND price_per_seat = 210000`))
}

func TestSubmit_RideCompleteness(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.rideListing(t, p, "Sài Gòn - Đà Lạt")
	_, _, err := e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{e.upload(t, p.OwnerUserID, "listing_image")}, nil)
	require.NoError(t, err)

	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_incomplete")
	require.Equal(t, "listing_incomplete: Listing chưa đủ thông tin để gửi duyệt, còn thiếu: tuyến (điểm đi, điểm đến); ít nhất 1 lịch chạy đang hoạt động", err.Error())

	_, err = e.Svc.SetRideRoute(ctx, p.ID, p.OwnerUserID, l.ID, e.hcmToDalat(t))
	require.NoError(t, err)
	s := e.schedule(t, p, l, daily("07:00"))
	_, err = e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, s.ID, listing.RideSchedulePatch{Active: ptr(false)})
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.Equal(t, "listing_incomplete: Listing chưa đủ thông tin để gửi duyệt, còn thiếu: ít nhất 1 lịch chạy đang hoạt động", err.Error())

	_, err = e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, s.ID, listing.RideSchedulePatch{Active: ptr(true)})
	require.NoError(t, err)
	got, err := e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	require.Equal(t, listing.ReviewPendingReview, got.ReviewStatus)

	// The admin sees the route and schedules; approval checks them too.
	d, err := e.Svc.AdminGet(ctx, l.ID)
	require.NoError(t, err)
	require.NotNil(t, d.Ride)
	require.NotNil(t, d.Ride.Route)
	require.Len(t, d.Ride.Schedules, 1)
	_, err = e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	pub, err := e.Svc.PublicDetail(ctx, l.Slug)
	require.NoError(t, err)
	require.NotNil(t, pub.Ride)
	require.Equal(t, "Bến xe Đà Lạt", pub.Ride.Route.Dest.Name)
}

func TestHTTP_RideRoutes_MalformedID404(t *testing.T) {
	e, p := rideEnv(t)
	tok := e.token(t, p.OwnerUserID)
	l := e.routed(t, p)
	unknown := uuid.Must(uuid.NewV7()).String()
	route := map[string]any{
		"origin":           map[string]any{"province_code": "79", "ward_code": e.wardIn(t, "79"), "name": "A", "address": "B"},
		"dest":             map[string]any{"province_code": "68", "ward_code": e.wardIn(t, "68"), "name": "C", "address": "D"},
		"duration_minutes": 300, "vehicle_description": "Xe 16 chỗ",
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/partner/listings/abc/ride"},
		{"GET", "/v1/partner/listings/" + unknown + "/ride"},
		{"PUT", "/v1/partner/listings/abc/ride-route"},
		{"PUT", "/v1/partner/listings/" + unknown + "/ride-route"},
		{"POST", "/v1/partner/listings/abc/ride-schedules"},
		{"POST", "/v1/partner/listings/" + unknown + "/ride-schedules"},
		{"PATCH", "/v1/partner/listings/" + l.ID.String() + "/ride-schedules/abc"},
		{"PATCH", "/v1/partner/listings/" + l.ID.String() + "/ride-schedules/" + unknown},
		{"PATCH", "/v1/partner/listings/abc/ride-schedules/" + unknown},
	} {
		var body any
		switch c.method {
		case "PUT":
			body = route
		case "POST":
			body = map[string]any{"departure_time": "07:00", "weekdays": 127, "seat_capacity": 30, "price_per_seat": 200000, "valid_from": "2026-11-05"}
		case "PATCH":
			body = map[string]any{"price_per_seat": 210000}
		}
		requireCode(t, e.do(t, c.method, c.path, tok, body), http.StatusNotFound, "not_found")
	}

	// The happy paths through the mux.
	rec := e.do(t, "PUT", "/v1/partner/listings/"+l.ID.String()+"/ride-route", tok, route)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"vehicle_description":"Xe 16 chỗ"`)
	rec = e.do(t, "POST", "/v1/partner/listings/"+l.ID.String()+"/ride-schedules", tok,
		map[string]any{"departure_time": "07:00", "weekdays": 127, "seat_capacity": 30, "price_per_seat": 200000, "valid_from": "2026-11-05"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.do(t, "GET", "/v1/partner/listings/"+l.ID.String()+"/ride", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"schedules":[{`)

	// An attraction is the wrong type.
	a := e.create(t, p, "Vé A")
	requireCode(t, e.do(t, "GET", "/v1/partner/listings/"+a.ID.String()+"/ride", tok, nil), http.StatusUnprocessableEntity, "wrong_service_type")
}

func TestHTTP_RideSchedule_UnknownField400(t *testing.T) {
	e, p := rideEnv(t)
	tok := e.token(t, p.OwnerUserID)
	l := e.routed(t, p)
	s := e.schedule(t, p, l, daily("07:00"))
	requireCode(t, e.do(t, "POST", "/v1/partner/listings/"+l.ID.String()+"/ride-schedules", tok, map[string]any{
		"departure_time": "08:00", "weekdays": 127, "seat_capacity": 30, "price_per_seat": 200000, "valid_from": "2026-11-05", "active": false,
	}), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "PATCH", "/v1/partner/listings/"+l.ID.String()+"/ride-schedules/"+s.ID.String(), tok,
		`{"price_per_seat":210000,"listing_id":"x"}`), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "PATCH", "/v1/partner/listings/"+l.ID.String()+"/ride-schedules/"+s.ID.String(), tok,
		`{"clear_valid_to":true}`), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "PUT", "/v1/partner/listings/"+l.ID.String()+"/ride-route", tok, `{"origin":{"lat":1}}`),
		http.StatusBadRequest, "invalid_request")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ride_schedules`))
}
