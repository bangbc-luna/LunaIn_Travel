package listing_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
)

func TestGenerateTrips_Idempotent(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	s := e.schedule(t, p, l, daily("07:00"))
	require.Len(t, e.trips(t, l.ID), 59)

	// Up to date: nothing to do.
	n, err := e.Svc.GenerateTrips(ctx)
	require.NoError(t, err)
	require.Zero(t, n)

	// A day later the horizon moves by one day, once.
	e.Clock.T = rideNow.AddDate(0, 0, 1)
	n, err = e.Svc.GenerateTrips(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = e.Svc.GenerateTrips(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Len(t, e.trips(t, l.ID), 60)

	// Forgetting how far it got regenerates nothing new.
	_, err = e.Pool.Exec(ctx, `UPDATE ride_schedules SET generated_until = NULL WHERE id = $1`, s.ID)
	require.NoError(t, err)
	n, err = e.Svc.GenerateTrips(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Len(t, e.trips(t, l.ID), 60)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ride_schedules WHERE generated_until = '2027-01-04'`))
}

func TestGenerateTrips_AcrossMidnightVN(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	late := e.schedule(t, p, l, listing.RideScheduleInput{DepartureTime: "23:45", Weekdays: mon, SeatCapacity: 30, PricePerSeat: 200_000, ValidFrom: "2026-11-05"})
	early := e.schedule(t, p, l, listing.RideScheduleInput{DepartureTime: "00:15", Weekdays: 1 << 1, SeatCapacity: 30, PricePerSeat: 200_000, ValidFrom: "2026-11-05"})
	_, err := e.Svc.GenerateTrips(ctx)
	require.NoError(t, err)

	at := func(s uuid.UUID, ts time.Time) int {
		return e.count(t, `SELECT count(*) FROM ride_trips WHERE schedule_id = $1 AND departure_at = $2`, s, ts)
	}
	// Monday 2026-11-09 23:45 VN.
	require.Equal(t, 1, at(late.ID, time.Date(2026, 11, 9, 16, 45, 0, 0, time.UTC)))
	// Tuesday 2026-11-10 00:15 VN is still Monday in UTC.
	require.Equal(t, 1, at(early.ID, time.Date(2026, 11, 9, 17, 15, 0, 0, time.UTC)))
	// Nothing on Monday 2026-11-09 00:15 VN.
	require.Zero(t, at(early.ID, time.Date(2026, 11, 8, 17, 15, 0, 0, time.UTC)))

	// Every trip falls on its schedule's Vietnam weekday.
	require.Zero(t, e.count(t, `SELECT count(*) FROM ride_trips WHERE schedule_id = $1
		AND extract(isodow FROM departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh') <> 1`, late.ID))
	require.Zero(t, e.count(t, `SELECT count(*) FROM ride_trips WHERE schedule_id = $1
		AND extract(isodow FROM departure_at AT TIME ZONE 'Asia/Ho_Chi_Minh') <> 2`, early.ID))
	// 2026-11-05 .. 2027-01-03 has 8 Mondays and 8 Tuesdays.
	require.Equal(t, 8, e.count(t, `SELECT count(*) FROM ride_trips WHERE schedule_id = $1`, late.ID))
	require.Equal(t, 8, e.count(t, `SELECT count(*) FROM ride_trips WHERE schedule_id = $1`, early.ID))
}

func TestGenerateTrips_RespectsValidTo(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	in := daily("07:00")
	in.ValidTo = ptr("2026-11-10")
	e.schedule(t, p, l, in)
	trips := e.trips(t, l.ID)
	require.Len(t, trips, 5)
	require.True(t, trips[4].DepartureAt.Equal(vnAt(2026, 11, 10, 7, 0)))

	for _, d := range []int{0, 3, 30} {
		e.Clock.T = rideNow.AddDate(0, 0, d)
		n, err := e.Svc.GenerateTrips(ctx)
		require.NoError(t, err)
		require.Zero(t, n)
	}
	require.Len(t, e.trips(t, l.ID), 5)
}

func TestGenerateTrips_SkipsAdHocConflict(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.routed(t, p)
	adHoc := uuid.Must(uuid.NewV7())
	_, err := e.Pool.Exec(ctx, `INSERT INTO ride_trips (id, listing_id, departure_at, seat_capacity, price_per_seat)
		VALUES ($1, $2, $3, 16, 50000)`, adHoc, l.ID, vnAt(2026, 11, 7, 7, 0))
	require.NoError(t, err)

	e.schedule(t, p, l, daily("07:00"))
	trips := e.trips(t, l.ID)
	require.Len(t, trips, 59)
	x := e.tripAt(t, l.ID, vnAt(2026, 11, 7, 7, 0))
	require.Equal(t, adHoc, x.ID)
	require.Nil(t, x.ScheduleID)
	require.Equal(t, int64(50_000), x.Price)
	require.Equal(t, 16, x.SeatCapacity)

	_, err = e.Pool.Exec(ctx, `UPDATE ride_schedules SET generated_until = NULL`)
	require.NoError(t, err)
	n, err := e.Svc.GenerateTrips(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Nil(t, e.tripAt(t, l.ID, vnAt(2026, 11, 7, 7, 0)).ScheduleID)
}

// A schedule that fails does not stop the others.
func TestGenerateTrips_ContinuesAfterFailure(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	a := e.routed(t, p)
	b := e.routed(t, p)
	sa := e.schedule(t, p, a, daily("07:00"))
	e.schedule(t, p, b, daily("07:00"))
	_, err := e.Pool.Exec(ctx, `UPDATE ride_schedules SET generated_until = NULL`)
	require.NoError(t, err)
	// Per-test database: make every new trip of a fail to insert.
	_, err = e.Pool.Exec(ctx, `ALTER TABLE ride_trips ADD CONSTRAINT test_fail
		CHECK (schedule_id IS DISTINCT FROM '`+sa.ID.String()+`' OR departure_at < '2027-01-04') NOT VALID`)
	require.NoError(t, err)
	e.Clock.T = rideNow.AddDate(0, 0, 1)
	n, err := e.Svc.GenerateTrips(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n) // b's new day only
	require.Len(t, e.trips(t, b.ID), 60)
	require.Len(t, e.trips(t, a.ID), 59)
}
