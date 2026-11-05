package listing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

// oneOff creates a one-off trip of listing l departing at.
func (e *env) oneOff(t *testing.T, p partner.Partner, l listing.Listing, at time.Time, seats int) listing.Trip {
	t.Helper()
	tr, err := e.Svc.CreateTrip(context.Background(), p.ID, p.OwnerUserID, l.ID, listing.TripInput{
		DepartureAt: at, SeatCapacity: seats, PricePerSeat: 250_000,
	})
	require.NoError(t, err)
	return tr
}

func requireApp(t *testing.T, err error, status int, code string) {
	t.Helper()
	st, c := appCode(t, err)
	require.Equal(t, status, st, err.Error())
	require.Equal(t, code, c)
}

func TestCreateTrip_OneOff(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	at := vnAt(2026, 11, 7, 6, 30)
	tr := e.oneOff(t, p, l, at, 16)
	require.Equal(t, l.ID, tr.ListingID)
	require.Nil(t, tr.ScheduleID)
	require.True(t, tr.DepartureAt.Equal(at))
	require.Equal(t, 16, tr.SeatCapacity)
	require.Equal(t, 0, tr.SeatsReserved)
	require.Equal(t, 16, tr.SeatsLeft())
	require.Equal(t, int64(250_000), tr.PricePerSeat)
	require.Equal(t, "scheduled", tr.Status)

	for _, in := range []listing.TripInput{
		{DepartureAt: rideNow, SeatCapacity: 10, PricePerSeat: 200_000},                     // not in the future
		{DepartureAt: rideNow.AddDate(0, 0, 366), SeatCapacity: 10, PricePerSeat: 200_000}, // beyond 365 days
		{DepartureAt: at.Add(time.Hour), SeatCapacity: 0, PricePerSeat: 200_000},
		{DepartureAt: at.Add(time.Hour), SeatCapacity: 81, PricePerSeat: 200_000},
		{DepartureAt: at.Add(time.Hour), SeatCapacity: 10, PricePerSeat: 999},
	} {
		_, err := e.Svc.CreateTrip(ctx, p.ID, p.OwnerUserID, l.ID, in)
		requireApp(t, err, http.StatusUnprocessableEntity, "invalid_ride")
	}

	// No route yet.
	bare := e.rideListing(t, p, "Chưa có tuyến")
	_, err := e.Svc.CreateTrip(ctx, p.ID, p.OwnerUserID, bare.ID, listing.TripInput{DepartureAt: at, SeatCapacity: 10, PricePerSeat: 200_000})
	requireApp(t, err, http.StatusUnprocessableEntity, "route_required")

	// Not a ride listing.
	att := e.create(t, p, "Vé tham quan")
	_, err = e.Svc.CreateTrip(ctx, p.ID, p.OwnerUserID, att.ID, listing.TripInput{DepartureAt: at, SeatCapacity: 10, PricePerSeat: 200_000})
	requireApp(t, err, http.StatusUnprocessableEntity, "wrong_service_type")
}

func TestCreateTrip_TimeTaken409(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	e.schedule(t, p, l, daily("07:00"))
	_, err := e.Svc.CreateTrip(ctx, p.ID, p.OwnerUserID, l.ID, listing.TripInput{
		DepartureAt: vnAt(2026, 11, 6, 7, 0), SeatCapacity: 10, PricePerSeat: 200_000,
	})
	requireApp(t, err, http.StatusConflict, "trip_time_taken")

	at := vnAt(2026, 11, 6, 13, 0)
	tr := e.oneOff(t, p, l, at, 10)
	_, err = e.Svc.CreateTrip(ctx, p.ID, p.OwnerUserID, l.ID, listing.TripInput{DepartureAt: at, SeatCapacity: 5, PricePerSeat: 100_000})
	requireApp(t, err, http.StatusConflict, "trip_time_taken")

	// Moving another trip onto a taken time is also 409.
	other := e.oneOff(t, p, l, at.Add(time.Hour), 10)
	_, err = e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, other.ID, listing.TripPatch{DepartureAt: &tr.DepartureAt})
	requireApp(t, err, http.StatusConflict, "trip_time_taken")
}

func TestUpdateTrip_ShrinkBelowReserved409(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	tr := e.oneOff(t, p, l, vnAt(2026, 11, 8, 8, 0), 10)
	e.reserve(t, tr.ID, 4)

	_, err := e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, listing.TripPatch{SeatCapacity: ptr(3)})
	requireApp(t, err, http.StatusConflict, "capacity_below_reserved")

	got, err := e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, listing.TripPatch{SeatCapacity: ptr(4)})
	require.NoError(t, err)
	require.Equal(t, 4, got.SeatCapacity)
	require.Equal(t, 4, got.SeatsReserved)
	require.Equal(t, 0, got.SeatsLeft())

	// Growing it back is fine too.
	got, err = e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, listing.TripPatch{SeatCapacity: ptr(12)})
	require.NoError(t, err)
	require.Equal(t, 8, got.SeatsLeft())

	au := e.audits(t, l.ID)
	last := au[len(au)-1]
	require.Equal(t, "ride_trip", last.Action)
	require.Equal(t, p.OwnerUserID, last.Actor)
	require.EqualValues(t, 4, last.Changes["seat_capacity"].Old)
	require.EqualValues(t, 12, last.Changes["seat_capacity"].New)
	require.Equal(t, tr.ID.String(), last.Changes["id"].New)
}

func TestUpdateTrip_TimeOrPriceWithBookings409(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	tr := e.oneOff(t, p, l, vnAt(2026, 11, 8, 8, 0), 10)
	e.reserve(t, tr.ID, 1)

	newAt := vnAt(2026, 11, 8, 9, 0)
	_, err := e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, listing.TripPatch{DepartureAt: &newAt})
	requireApp(t, err, http.StatusConflict, "trip_has_bookings")
	_, err = e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, listing.TripPatch{PricePerSeat: ptr(int64(300_000))})
	requireApp(t, err, http.StatusConflict, "trip_has_bookings")

	// Same values are not a change.
	same := tr.DepartureAt
	_, err = e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, listing.TripPatch{DepartureAt: &same, PricePerSeat: ptr(tr.PricePerSeat)})
	require.NoError(t, err)

	x := e.tripAt(t, l.ID, tr.DepartureAt)
	require.Equal(t, int64(250_000), x.Price)
}

func TestUpdateTrip_NoBookingsOK(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	e.schedule(t, p, l, daily("07:00"))
	sched := e.tripAt(t, l.ID, vnAt(2026, 11, 6, 7, 0))

	newAt := vnAt(2026, 11, 6, 7, 30)
	got, err := e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, sched.ID, listing.TripPatch{
		DepartureAt: &newAt, PricePerSeat: ptr(int64(180_000)), SeatCapacity: ptr(20),
	})
	require.NoError(t, err)
	require.True(t, got.DepartureAt.Equal(newAt))
	require.Equal(t, int64(180_000), got.PricePerSeat)
	require.Equal(t, 20, got.SeatCapacity)
	// A moved trip leaves its schedule: it is now a one-off.
	require.Nil(t, got.ScheduleID)

	// Validation still applies.
	past := rideNow.Add(-time.Minute)
	_, err = e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, sched.ID, listing.TripPatch{DepartureAt: &past})
	requireApp(t, err, http.StatusUnprocessableEntity, "invalid_ride")
	_, err = e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, sched.ID, listing.TripPatch{SeatCapacity: ptr(0)})
	requireApp(t, err, http.StatusUnprocessableEntity, "invalid_ride")

	// A cancelled trip cannot be edited.
	_, err = e.Svc.CancelTripByPartner(ctx, p.ID, p.OwnerUserID, l.ID, sched.ID, "Xe hỏng")
	require.NoError(t, err)
	_, err = e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, sched.ID, listing.TripPatch{SeatCapacity: ptr(10)})
	requireApp(t, err, http.StatusConflict, "trip_not_scheduled")

	// Unknown trip id.
	_, err = e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, uuid.Must(uuid.NewV7()), listing.TripPatch{SeatCapacity: ptr(10)})
	requireApp(t, err, http.StatusNotFound, "not_found")
}

func TestCancelTripByPartner_WithSeats409(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	tr := e.oneOff(t, p, l, vnAt(2026, 11, 8, 8, 0), 10)
	e.reserve(t, tr.ID, 2)
	_, err := e.Svc.CancelTripByPartner(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, "Xe hỏng")
	requireApp(t, err, http.StatusConflict, "trip_has_bookings")
	require.Equal(t, "scheduled", e.tripAt(t, l.ID, tr.DepartureAt).Status)
}

func TestCancelTripByPartner_EmptyOK(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	tr := e.oneOff(t, p, l, vnAt(2026, 11, 8, 8, 0), 10)

	for _, reason := range []string{"", "   ", strings.Repeat("a", 501)} {
		_, err := e.Svc.CancelTripByPartner(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, reason)
		requireApp(t, err, http.StatusUnprocessableEntity, "invalid_ride")
	}

	got, err := e.Svc.CancelTripByPartner(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, "  Xe hỏng  ")
	require.NoError(t, err)
	require.Equal(t, "cancelled", got.Status)
	require.Equal(t, "Xe hỏng", *got.CancelledReason)
	x := e.tripAt(t, l.ID, tr.DepartureAt)
	require.Equal(t, "partner", *x.CancelSource)
	var by uuid.UUID
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT cancelled_by FROM ride_trips WHERE id = $1`, tr.ID).Scan(&by))
	require.Equal(t, p.OwnerUserID, by)
	require.Equal(t, "ride_trip", e.audits(t, l.ID)[len(e.audits(t, l.ID))-1].Action)

	_, err = e.Svc.CancelTripByPartner(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, "Lần nữa")
	requireApp(t, err, http.StatusConflict, "trip_not_scheduled")
}

func TestTripOtherPartner404(t *testing.T) {
	e, a := rideEnv(t)
	ctx := context.Background()
	b := e.partner(t, "Beta", ride)
	la := e.routed(t, a)
	lb := e.routed(t, b)
	tr := e.oneOff(t, a, la, vnAt(2026, 11, 8, 8, 0), 10)

	// B names A's listing.
	_, err := e.Svc.UpdateTrip(ctx, b.ID, b.OwnerUserID, la.ID, tr.ID, listing.TripPatch{SeatCapacity: ptr(5)})
	requireApp(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.CancelTripByPartner(ctx, b.ID, b.OwnerUserID, la.ID, tr.ID, "x")
	requireApp(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.ListTrips(ctx, b.ID, la.ID, rideNow, rideNow.AddDate(0, 0, 7))
	requireApp(t, err, http.StatusNotFound, "not_found")
	// B names its own listing with A's trip.
	_, err = e.Svc.UpdateTrip(ctx, b.ID, b.OwnerUserID, lb.ID, tr.ID, listing.TripPatch{SeatCapacity: ptr(5)})
	requireApp(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.CancelTripByPartner(ctx, b.ID, b.OwnerUserID, lb.ID, tr.ID, "x")
	requireApp(t, err, http.StatusNotFound, "not_found")

	// Over HTTP, malformed ids are 404 too.
	tokB := e.token(t, b.OwnerUserID)
	base := "/v1/partner/listings/" + la.ID.String() + "/ride-trips/"
	requireCode(t, e.do(t, http.MethodPatch, base+tr.ID.String(), tokB, map[string]any{"seat_capacity": 5}), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, http.MethodPost, base+tr.ID.String()+"/cancel", tokB, map[string]any{"reason": "x"}), http.StatusNotFound, "not_found")
	tokA := e.token(t, a.OwnerUserID)
	requireCode(t, e.do(t, http.MethodPatch, base+"not-a-uuid", tokA, map[string]any{"seat_capacity": 5}), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, http.MethodPost, base+"not-a-uuid/cancel", tokA, map[string]any{"reason": "x"}), http.StatusNotFound, "not_found")
	require.Equal(t, 10, e.tripAt(t, la.ID, tr.DepartureAt).SeatCapacity)
}

func TestSuspendedPartnerCannotEditTrips(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	tr := e.oneOff(t, p, l, vnAt(2026, 11, 8, 8, 0), 10)
	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, p.ID, "Vi phạm"))

	_, err := e.Svc.CreateTrip(ctx, p.ID, p.OwnerUserID, l.ID, listing.TripInput{DepartureAt: vnAt(2026, 11, 9, 8, 0), SeatCapacity: 10, PricePerSeat: 200_000})
	requireApp(t, err, http.StatusForbidden, "partner_suspended")
	_, err = e.Svc.UpdateTrip(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, listing.TripPatch{SeatCapacity: ptr(5)})
	requireApp(t, err, http.StatusForbidden, "partner_suspended")
	_, err = e.Svc.CancelTripByPartner(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, "Xe hỏng")
	requireApp(t, err, http.StatusForbidden, "partner_suspended")

	tok := e.token(t, p.OwnerUserID)
	base := "/v1/partner/listings/" + l.ID.String() + "/ride-trips"
	requireCode(t, e.do(t, http.MethodPost, base, tok, map[string]any{
		"departure_at": "2026-11-09T08:00:00+07:00", "seat_capacity": 10, "price_per_seat": 200000,
	}), http.StatusForbidden, "partner_suspended")
	requireCode(t, e.do(t, http.MethodPatch, base+"/"+tr.ID.String(), tok, map[string]any{"seat_capacity": 5}), http.StatusForbidden, "partner_suspended")
	requireCode(t, e.do(t, http.MethodPost, base+"/"+tr.ID.String()+"/cancel", tok, map[string]any{"reason": "x"}), http.StatusForbidden, "partner_suspended")
	x := e.tripAt(t, l.ID, tr.DepartureAt)
	require.Equal(t, 10, x.SeatCapacity)
	require.Equal(t, "scheduled", x.Status)
}

func TestTrips_HTTP(t *testing.T) {
	e, p := rideEnv(t)
	l := e.routed(t, p)
	e.schedule(t, p, l, daily("07:00"))
	tok := e.token(t, p.OwnerUserID)
	base := "/v1/partner/listings/" + l.ID.String() + "/ride-trips"

	rec := e.do(t, http.MethodPost, base, tok, map[string]any{
		"departure_at": "2026-11-06T13:00:00+07:00", "seat_capacity": 12, "price_per_seat": 150000,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		ID        uuid.UUID `json:"id"`
		SeatsLeft int       `json:"seats_left"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, 12, created.SeatsLeft)

	requireCode(t, e.do(t, http.MethodPost, base, tok, map[string]any{
		"departure_at": "2026-11-06T13:00:00+07:00", "seat_capacity": 12, "price_per_seat": 150000, "extra": 1,
	}), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, http.MethodPost, base, tok, map[string]any{
		"departure_at": "2026-11-06T14:00:00+07:00", "seat_capacity": 0, "price_per_seat": 150000,
	}), http.StatusUnprocessableEntity, "invalid_ride")
	requireCode(t, e.do(t, http.MethodPatch, base+"/"+created.ID.String(), tok, map[string]any{"bogus": 1}),
		http.StatusBadRequest, "invalid_request")

	rec = e.do(t, http.MethodPatch, base+"/"+created.ID.String(), tok, map[string]any{"price_per_seat": 160000})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// List: 2026-11-06 (date form, whole VN day) has the 07:00 and 13:00 trips.
	rec = e.do(t, http.MethodGet, base+"?from=2026-11-06&to=2026-11-06", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var trips []struct {
		ID           uuid.UUID `json:"id"`
		DepartureAt  time.Time `json:"departure_at"`
		PricePerSeat int64     `json:"price_per_seat"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trips))
	require.Len(t, trips, 2)
	require.True(t, trips[0].DepartureAt.Equal(vnAt(2026, 11, 6, 7, 0)))
	require.Equal(t, created.ID, trips[1].ID)
	require.Equal(t, int64(160000), trips[1].PricePerSeat)

	rec = e.do(t, http.MethodGet, base+"?from=2026-11-06T00:00:00%2B07:00&to=2026-11-08T00:00:00%2B07:00", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trips))
	require.Len(t, trips, 3)

	requireCode(t, e.do(t, http.MethodGet, base+"?from=2026-11-06&to=2027-02-01", tok, nil), http.StatusUnprocessableEntity, "invalid_ride")
	requireCode(t, e.do(t, http.MethodGet, base+"?from=nope&to=2026-11-08", tok, nil), http.StatusUnprocessableEntity, "invalid_ride")
	requireCode(t, e.do(t, http.MethodGet, base+"?from=2026-11-08&to=2026-11-06", tok, nil), http.StatusUnprocessableEntity, "invalid_ride")

	rec = e.do(t, http.MethodPost, base+"/"+created.ID.String()+"/cancel", tok, map[string]any{"reason": "Đổi xe"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireCode(t, e.do(t, http.MethodPost, base+"/"+created.ID.String()+"/cancel", tok, map[string]any{"reason": "Đổi xe", "x": 1}),
		http.StatusBadRequest, "invalid_request")
}

func TestAdminListTrips(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	e.schedule(t, p, l, daily("07:00"))

	trips, err := e.Svc.AdminListTrips(ctx, l.ID, vnAt(2026, 11, 6, 0, 0), vnAt(2026, 11, 9, 0, 0))
	require.NoError(t, err)
	require.Len(t, trips, 3)
	_, err = e.Svc.AdminListTrips(ctx, uuid.Must(uuid.NewV7()), rideNow, rideNow.AddDate(0, 0, 1))
	requireApp(t, err, http.StatusNotFound, "not_found")

	adminTok, _, err := e.Issuer.Issue(auth.Principal{UserID: e.Admin, Role: auth.RoleSuperAdmin})
	require.NoError(t, err)
	path := "/v1/admin/listings/" + l.ID.String() + "/ride-trips?from=2026-11-06&to=2026-11-07"
	rec := e.do(t, http.MethodGet, path, adminTok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2)
	requireCode(t, e.do(t, http.MethodGet, path, e.token(t, p.OwnerUserID), nil), http.StatusForbidden, "forbidden")
	requireCode(t, e.do(t, http.MethodGet, "/v1/admin/listings/nope/ride-trips", adminTok, nil), http.StatusNotFound, "not_found")

	d, err := e.Svc.AdminGet(ctx, l.ID)
	require.NoError(t, err)
	require.NotNil(t, d.Ride)
}

// inTx runs f in its own transaction on the test pool.
func (e *env) inTx(t *testing.T, f func(tx pgx.Tx) error) error {
	t.Helper()
	return db.InTx(context.Background(), e.Pool, f)
}

func TestReserveSeats_Conditional(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	tr := e.oneOff(t, p, l, vnAt(2026, 11, 8, 8, 0), 3)
	reserve := func(id uuid.UUID, n int, now time.Time) bool {
		var ok bool
		require.NoError(t, e.inTx(t, func(tx pgx.Tx) error {
			var err error
			ok, err = e.Svc.ReserveSeats(ctx, tx, id, n, now)
			return err
		}))
		return ok
	}
	require.True(t, reserve(tr.ID, 2, rideNow))
	require.False(t, reserve(tr.ID, 2, rideNow))
	require.True(t, reserve(tr.ID, 1, rideNow))
	require.Equal(t, 3, e.tripAt(t, l.ID, tr.DepartureAt).SeatsReserved)

	// Departed (departure_at <= now).
	tr2 := e.oneOff(t, p, l, vnAt(2026, 11, 9, 8, 0), 3)
	require.False(t, reserve(tr2.ID, 1, tr2.DepartureAt))
	require.False(t, reserve(tr2.ID, 1, tr2.DepartureAt.Add(time.Minute)))
	require.True(t, reserve(tr2.ID, 1, tr2.DepartureAt.Add(-time.Minute)))

	// Cancelled.
	tr3 := e.oneOff(t, p, l, vnAt(2026, 11, 10, 8, 0), 3)
	_, err := e.Svc.CancelTripByPartner(ctx, p.ID, p.OwnerUserID, l.ID, tr3.ID, "Nghỉ")
	require.NoError(t, err)
	require.False(t, reserve(tr3.ID, 1, rideNow))
	require.Equal(t, 0, e.tripAt(t, l.ID, tr3.DepartureAt).SeatsReserved)
}

func TestReserveSeats_ConcurrentLastSeatsNeverOversell(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)

	// Two transactions race for the last 2 seats: exactly one wins.
	tr := e.oneOff(t, p, l, vnAt(2026, 11, 8, 8, 0), 3)
	e.reserve(t, tr.ID, 1)
	txA, err := e.Pool.Begin(ctx)
	require.NoError(t, err)
	okA, err := e.Svc.ReserveSeats(ctx, txA, tr.ID, 2, rideNow)
	require.NoError(t, err)
	require.True(t, okA)
	done := make(chan bool, 1)
	go func() {
		var ok bool
		_ = db.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
			var err error
			ok, err = e.Svc.ReserveSeats(ctx, tx, tr.ID, 2, rideNow) // blocks on A's row lock
			return err
		})
		done <- ok
	}()
	select {
	case <-done:
		t.Fatal("second reservation did not wait for the first")
	case <-time.After(200 * time.Millisecond):
	}
	require.NoError(t, txA.Commit(ctx))
	require.False(t, <-done)
	require.Equal(t, 3, e.tripAt(t, l.ID, tr.DepartureAt).SeatsReserved)

	// Many concurrent single-seat reservations fill the trip exactly.
	tr2 := e.oneOff(t, p, l, vnAt(2026, 11, 9, 8, 0), 3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var ok bool
			err := db.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
				var err error
				ok, err = e.Svc.ReserveSeats(ctx, tx, tr2.ID, 1, rideNow)
				return err
			})
			if err == nil && ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 3, wins)
	require.Equal(t, 3, e.tripAt(t, l.ID, tr2.DepartureAt).SeatsReserved)
}

func TestReleaseSeats_BelowZeroFails(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	tr := e.oneOff(t, p, l, vnAt(2026, 11, 8, 8, 0), 5)
	e.reserve(t, tr.ID, 2)

	require.Error(t, e.inTx(t, func(tx pgx.Tx) error { return e.Svc.ReleaseSeats(ctx, tx, tr.ID, 3) }))
	require.Equal(t, 2, e.tripAt(t, l.ID, tr.DepartureAt).SeatsReserved)

	require.NoError(t, e.inTx(t, func(tx pgx.Tx) error { return e.Svc.ReleaseSeats(ctx, tx, tr.ID, 1) }))
	require.Equal(t, 1, e.tripAt(t, l.ID, tr.DepartureAt).SeatsReserved)

	// Seats are released on a cancelled trip too.
	require.NoError(t, e.inTx(t, func(tx pgx.Tx) error {
		return e.Svc.CancelTripByAdmin(ctx, tx, tr.ID, e.Admin, "Bão", rideNow)
	}))
	require.NoError(t, e.inTx(t, func(tx pgx.Tx) error { return e.Svc.ReleaseSeats(ctx, tx, tr.ID, 1) }))
	require.Equal(t, 0, e.tripAt(t, l.ID, tr.DepartureAt).SeatsReserved)

	err := e.inTx(t, func(tx pgx.Tx) error { return e.Svc.ReleaseSeats(ctx, tx, uuid.Must(uuid.NewV7()), 1) })
	requireApp(t, err, http.StatusNotFound, "not_found")
}

func TestLockTrips_OrderedAndDeduped(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	a := e.oneOff(t, p, l, vnAt(2026, 11, 8, 8, 0), 5)
	b := e.oneOff(t, p, l, vnAt(2026, 11, 9, 8, 0), 5)
	c := e.oneOff(t, p, l, vnAt(2026, 11, 10, 8, 0), 5)

	var got []listing.Trip
	require.NoError(t, e.inTx(t, func(tx pgx.Tx) error {
		var err error
		got, err = e.Svc.LockTrips(ctx, tx, []uuid.UUID{c.ID, a.ID, c.ID, b.ID, a.ID})
		return err
	}))
	require.Len(t, got, 3)
	ids := []uuid.UUID{got[0].ID, got[1].ID, got[2].ID}
	require.Equal(t, []uuid.UUID{a.ID, b.ID, c.ID}, ids) // v7 ids: creation order = id order
	require.Equal(t, 5, got[0].SeatsLeft())

	// The rows really are locked: another tx cannot lock one NOWAIT.
	tx, err := e.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = e.Svc.LockTrips(ctx, tx, []uuid.UUID{b.ID})
	require.NoError(t, err)
	_, err = e.Pool.Exec(ctx, `SELECT 1 FROM ride_trips WHERE id = $1 FOR UPDATE NOWAIT`, b.ID)
	require.Error(t, err)
	require.NoError(t, tx.Rollback(ctx))

	err = e.inTx(t, func(tx pgx.Tx) error {
		_, err := e.Svc.LockTrips(ctx, tx, []uuid.UUID{a.ID, uuid.Must(uuid.NewV7())})
		return err
	})
	requireApp(t, err, http.StatusNotFound, "not_found")
}

func TestTripListingIDsAndRideRoute(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l1 := e.routed(t, p)
	l2 := e.routed(t, p)
	a := e.oneOff(t, p, l1, vnAt(2026, 11, 8, 8, 0), 5)
	b := e.oneOff(t, p, l2, vnAt(2026, 11, 8, 8, 0), 5)
	require.NoError(t, e.inTx(t, func(tx pgx.Tx) error {
		m, err := e.Svc.TripListingIDs(ctx, tx, []uuid.UUID{a.ID, b.ID, a.ID})
		require.NoError(t, err)
		require.Equal(t, map[uuid.UUID]uuid.UUID{a.ID: l1.ID, b.ID: l2.ID}, m)
		_, err = e.Svc.TripListingIDs(ctx, tx, []uuid.UUID{a.ID, uuid.Must(uuid.NewV7())})
		requireApp(t, err, http.StatusNotFound, "not_found")

		r, err := e.Svc.RideRoute(ctx, tx, l1.ID)
		require.NoError(t, err)
		require.Equal(t, 420, r.DurationMinutes)
		require.Equal(t, "Bến xe Đà Lạt", r.Dest.Name)
		_, err = e.Svc.RideRoute(ctx, tx, uuid.Must(uuid.NewV7()))
		requireApp(t, err, http.StatusNotFound, "not_found")
		return nil
	}))
}

func TestCancelTripByAdmin(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	tr := e.oneOff(t, p, l, vnAt(2026, 11, 8, 8, 0), 5)
	e.reserve(t, tr.ID, 3)

	require.NoError(t, e.inTx(t, func(tx pgx.Tx) error {
		if _, err := e.Svc.LockTrips(ctx, tx, []uuid.UUID{tr.ID}); err != nil {
			return err
		}
		return e.Svc.CancelTripByAdmin(ctx, tx, tr.ID, e.Admin, "Bão lớn", rideNow)
	}))
	x := e.tripAt(t, l.ID, tr.DepartureAt)
	require.Equal(t, "cancelled", x.Status)
	require.Equal(t, "admin", *x.CancelSource)
	require.Equal(t, 3, x.SeatsReserved) // seats are released by booking, booking by booking

	err := e.inTx(t, func(tx pgx.Tx) error { return e.Svc.CancelTripByAdmin(ctx, tx, tr.ID, e.Admin, "Lần nữa", rideNow) })
	requireApp(t, err, http.StatusConflict, "trip_not_scheduled")
	err = e.inTx(t, func(tx pgx.Tx) error {
		return e.Svc.CancelTripByAdmin(ctx, tx, uuid.Must(uuid.NewV7()), e.Admin, "x", rideNow)
	})
	requireApp(t, err, http.StatusNotFound, "not_found")
}
