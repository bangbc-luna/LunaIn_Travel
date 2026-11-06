package listing_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/maps"
)

// Da Nang airport -> Hoi An: 30,240 m and 2,700 s by the fake route.
var (
	airport = listing.CharterPlace{Address: "Sân bay Đà Nẵng", Lat: 16.0544, Lng: 108.2022, ProvinceCode: "48"}
	hoiAn   = listing.CharterPlace{Address: "Phố cổ Hội An", Lat: 15.8801, Lng: 108.3380}

	airportPt = maps.Point{Lat: 16.0544, Lng: 108.2022}
	hoiAnPt   = maps.Point{Lat: 15.8801, Lng: 108.3380}

	// wed0800 is Wednesday 2026-11-11 08:00 Vietnam time; sat0800 is Saturday.
	wed0800 = time.Date(2026, 11, 11, 8, 0, 0, 0, clock.VN)
	sat0800 = time.Date(2026, 11, 14, 8, 0, 0, 0, clock.VN)
)

// pricing7 is "Xe 7 chỗ": 200,000 + 12,000/km, night 100,000 (22:00-05:00),
// weekend 10%, holiday 30%, round-trip waiting 150,000.
func pricing7() listing.CharterPricing {
	return listing.CharterPricing{
		BaseFare: 200_000, PricePerKm: 12_000,
		Surcharges: []listing.Surcharge{
			{Kind: listing.SurchargeNight, Calc: listing.CalcFixed, Amount: 100_000, Active: true},
			{Kind: listing.SurchargeWeekend, Calc: listing.CalcPercent, Amount: 1000, Active: true},
			{Kind: listing.SurchargeHoliday, Calc: listing.CalcPercent, Amount: 3000, Active: true},
			{Kind: listing.SurchargeRoundTripWaiting, Calc: listing.CalcFixed, Amount: 150_000, Active: true},
		},
		NightStart: ptr("22:00"), NightEnd: ptr("05:00"),
	}
}

func surchargeSet7() []listing.SurchargeInput {
	return pricing7().Surcharges
}

func kinds(q listing.CharterQuote) []string {
	out := []string{}
	for _, s := range q.Surcharges {
		out = append(out, s.Kind)
	}
	return out
}

// charterPublished is the approved "Xe bao chuyến Đà Nẵng" (areas 46, 48,
// max 300 km, night 22:00-05:00) with Xe 7 chỗ (all surcharges) and Xe 16
// chỗ (none), and the fake airport -> Hoi An route.
func (e *env) charterPublished(t *testing.T, p partner.Partner) (listing.Listing, listing.CharterClass, listing.CharterClass) {
	t.Helper()
	ctx := context.Background()
	l := e.charterShop(t, p)
	c16 := e.charterClass(t, p, l, class16())
	c7 := e.charterClass(t, p, l, class7())
	_, err := e.Svc.SetCharterSurcharges(ctx, p.ID, p.OwnerUserID, l.ID, c7.ID, surchargeSet7())
	require.NoError(t, err)
	_, _, err = e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{e.upload(t, p.OwnerUserID, "listing_image")}, nil)
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	l, err = e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	e.Maps.SetRoute(airportPt, hoiAnPt, maps.Route{DistanceMeters: 30_240, DurationSeconds: 2_700})
	return l, c7, c16
}

func TestCharterRoute_RoundsUp(t *testing.T) {
	e, _ := charterEnv(t)
	ctx := context.Background()
	for _, c := range []struct {
		m, s    int64
		tenths  int64
		minutes int
	}{
		{30_240, 2_700, 303, 45},
		{30_200, 2_700, 302, 45},
		{30_201, 2_701, 303, 46},
		{100, 1, 1, 1},
	} {
		e.Maps.SetRoute(airportPt, hoiAnPt, maps.Route{DistanceMeters: c.m, DurationSeconds: c.s})
		r, err := e.Svc.CharterRoute(ctx, airport, hoiAn)
		require.NoError(t, err)
		require.Equal(t, "fake", r.Provider)
		require.Equal(t, c.m, r.DistanceMeters)
		require.Equal(t, c.s, r.DurationSeconds)
		require.Equal(t, c.tenths, r.DistanceTenths, "%d m", c.m)
		require.Equal(t, c.minutes, r.DurationMinutes, "%d s", c.s)
	}
}

// Review Focus 4: provider failures map to 503 / 422, and bad coordinates
// never reach the provider.
func TestCharterRoute_ProviderErrors(t *testing.T) {
	e, _ := charterEnv(t)
	ctx := context.Background()
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{maps.ErrUnavailable, http.StatusServiceUnavailable, "map_unavailable"},
		{errors.Join(maps.ErrUnavailable, context.DeadlineExceeded), http.StatusServiceUnavailable, "map_unavailable"},
		{errors.New("unexpected"), http.StatusServiceUnavailable, "map_unavailable"},
		{maps.ErrNoRoute, http.StatusUnprocessableEntity, "no_route"},
	} {
		e.Maps.SetErr(c.err)
		_, err := e.Svc.CharterRoute(ctx, airport, hoiAn)
		requireErr(t, err, c.status, c.code)
	}
	e.Maps.SetErr(nil)
	e.Maps.SetRoute(airportPt, hoiAnPt, maps.Route{DistanceMeters: 0, DurationSeconds: 0})
	_, err := e.Svc.CharterRoute(ctx, airport, hoiAn)
	requireErr(t, err, http.StatusUnprocessableEntity, "no_route")

	calls := e.Maps.Calls()
	for _, bad := range []listing.CharterPlace{
		{Address: "A", Lat: 1.0, Lng: 100.0},
		{Address: "A", Lat: 7.99, Lng: 105},
		{Address: "A", Lat: 16, Lng: 110.01},
	} {
		_, err := e.Svc.CharterRoute(ctx, airport, bad)
		requireErr(t, err, http.StatusUnprocessableEntity, "invalid_location")
	}
	require.Equal(t, calls, e.Maps.Calls(), "invalid coordinates never reach the provider")
}

func TestPriceCharter_OneWayWeekday(t *testing.T) {
	q := listing.PriceCharter(pricing7(), listing.PricingInput{OneWayTenths: 303, PickupAt: wed0800})
	require.Equal(t, int64(303), q.DistanceTenths)
	require.Equal(t, int64(303), q.BilledTenths)
	require.False(t, q.IsRoundTrip)
	require.Equal(t, int64(200_000), q.BaseFare)
	require.Equal(t, int64(12_000), q.PricePerKm)
	require.Equal(t, int64(363_600), q.DistanceTotal)
	require.Equal(t, int64(563_600), q.Fare)
	require.Empty(t, q.Surcharges)
	require.Equal(t, int64(563_600), q.Total)

	q16 := listing.PriceCharter(listing.CharterPricing{BaseFare: 400_000, PricePerKm: 18_000}, listing.PricingInput{OneWayTenths: 303, PickupAt: wed0800})
	require.Equal(t, int64(945_400), q16.Total)
}

func TestPriceCharter_RoundTrip(t *testing.T) {
	q := listing.PriceCharter(pricing7(), listing.PricingInput{OneWayTenths: 303, RoundTrip: true, PickupAt: wed0800})
	require.True(t, q.IsRoundTrip)
	require.Equal(t, int64(303), q.DistanceTenths)
	require.Equal(t, int64(606), q.BilledTenths)
	require.Equal(t, int64(727_200), q.DistanceTotal)
	require.Equal(t, int64(927_200), q.Fare)
	require.Equal(t, []listing.SurchargeLine{{Kind: "round_trip_waiting", Calc: "fixed", Rate: 150_000, Amount: 150_000}}, q.Surcharges)
	require.Equal(t, int64(1_077_200), q.Total)
}

// Review Focus 2.
func TestPriceCharter_NightWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 11, h, m, 0, 0, clock.VN) }
	price := func(p listing.CharterPricing, t time.Time) listing.CharterQuote {
		return listing.PriceCharter(p, listing.PricingInput{OneWayTenths: 303, PickupAt: t})
	}
	night := []listing.SurchargeLine{{Kind: "night", Calc: "fixed", Rate: 100_000, Amount: 100_000}}

	q := price(pricing7(), at(22, 0))
	require.Equal(t, night, q.Surcharges)
	require.Equal(t, int64(663_600), q.Total)
	require.Equal(t, int64(663_600), price(pricing7(), at(4, 59)).Total)
	require.Equal(t, int64(663_600), price(pricing7(), at(23, 59)).Total)
	require.Equal(t, int64(663_600), price(pricing7(), at(0, 0)).Total)
	require.Equal(t, int64(563_600), price(pricing7(), at(5, 0)).Total)
	require.Equal(t, int64(563_600), price(pricing7(), at(21, 59)).Total)
	// 15:00 UTC is 22:00 in Vietnam.
	utc := price(pricing7(), time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC))
	require.Equal(t, night, utc.Surcharges)
	require.Equal(t, int64(663_600), utc.Total)
	require.Equal(t, int64(563_600), price(pricing7(), time.Date(2026, 11, 11, 14, 59, 0, 0, time.UTC)).Total)

	// A window that does not cross midnight.
	early := pricing7()
	early.NightStart, early.NightEnd = ptr("00:00"), ptr("05:00")
	require.Equal(t, int64(663_600), price(early, at(0, 0)).Total)
	require.Equal(t, int64(663_600), price(early, at(4, 59)).Total)
	require.Equal(t, int64(563_600), price(early, at(5, 0)).Total)
	require.Equal(t, int64(563_600), price(early, at(22, 0)).Total)

	// No night window: the night surcharge never applies.
	none := pricing7()
	none.NightStart, none.NightEnd = nil, nil
	require.Empty(t, price(none, at(23, 0)).Surcharges)
	require.Equal(t, int64(563_600), price(none, at(23, 0)).Total)

	// Round trip: night is charged once, on the pickup time.
	rt := listing.PriceCharter(pricing7(), listing.PricingInput{OneWayTenths: 303, RoundTrip: true, PickupAt: at(22, 0)})
	require.Equal(t, []string{"night", "round_trip_waiting"}, kinds(rt))
	require.Equal(t, int64(1_177_200), rt.Total)
}

func TestInNightWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 11, h, m, 0, 0, clock.VN) }
	for _, c := range []struct {
		start, end string
		t          time.Time
		want       bool
	}{
		{"22:00", "05:00", at(21, 59), false},
		{"22:00", "05:00", at(22, 0), true},
		{"22:00", "05:00", at(23, 59), true},
		{"22:00", "05:00", at(0, 0), true},
		{"22:00", "05:00", at(4, 59), true},
		{"22:00", "05:00", at(5, 0), false},
		{"22:00", "05:00", at(12, 0), false},
		{"00:00", "05:00", at(0, 0), true},
		{"00:00", "05:00", at(4, 59), true},
		{"00:00", "05:00", at(5, 0), false},
		{"00:00", "05:00", at(23, 59), false},
		{"20:30", "23:45", at(20, 29), false},
		{"20:30", "23:45", at(20, 30), true},
		{"20:30", "23:45", at(23, 44), true},
		{"20:30", "23:45", at(23, 45), false},
		{"22:00", "05:00", time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC), true},
		{"22:00", "05:00", time.Date(2026, 11, 11, 22, 0, 0, 0, time.UTC), false}, // 05:00 VN
		{"bad", "05:00", at(0, 0), false},
		{"22:00", "22:00", at(22, 0), false},
	} {
		require.Equal(t, c.want, listing.InNightWindow(c.start, c.end, c.t), "%s-%s at %s", c.start, c.end, c.t)
	}
}

// Review Focus 3.
func TestPriceCharter_WeekendHolidayTakeLarger(t *testing.T) {
	q := listing.PriceCharter(pricing7(), listing.PricingInput{OneWayTenths: 303, PickupAt: sat0800})
	require.Equal(t, []listing.SurchargeLine{{Kind: "weekend", Calc: "percent", Rate: 1000, Amount: 56_360}}, q.Surcharges)
	require.Equal(t, int64(619_960), q.Total)

	// Saturday is also a holiday: only the larger (holiday 30%), never both.
	q = listing.PriceCharter(pricing7(), listing.PricingInput{OneWayTenths: 303, PickupAt: sat0800, Holiday: true})
	require.Equal(t, []listing.SurchargeLine{{Kind: "holiday", Calc: "percent", Rate: 3000, Amount: 169_080}}, q.Surcharges)
	require.Equal(t, int64(732_680), q.Total)
	require.NotEqual(t, int64(788_960), q.Total, "56,360 + 169,080 must not stack")

	// Holiday off (0): the weekend still applies on a holiday Saturday.
	p := pricing7()
	p.Surcharges[2].Amount = 0
	q = listing.PriceCharter(p, listing.PricingInput{OneWayTenths: 303, PickupAt: sat0800, Holiday: true})
	require.Equal(t, []string{"weekend"}, kinds(q))
	require.Equal(t, int64(619_960), q.Total)

	// Weekend larger than holiday: weekend wins.
	p = pricing7()
	p.Surcharges[1].Amount, p.Surcharges[2].Amount = 3000, 1000
	q = listing.PriceCharter(p, listing.PricingInput{OneWayTenths: 303, PickupAt: sat0800, Holiday: true})
	require.Equal(t, []string{"weekend"}, kinds(q))
	require.Equal(t, int64(732_680), q.Total)

	// Equal amounts: recorded as holiday.
	p = pricing7()
	p.Surcharges[1].Amount, p.Surcharges[2].Amount = 3000, 3000
	q = listing.PriceCharter(p, listing.PricingInput{OneWayTenths: 303, PickupAt: sat0800, Holiday: true})
	require.Equal(t, []string{"holiday"}, kinds(q))
	require.Equal(t, int64(732_680), q.Total)

	// Fixed weekend 200,000 against holiday 30% (169,080): weekend is larger.
	p = pricing7()
	p.Surcharges[1] = listing.Surcharge{Kind: listing.SurchargeWeekend, Calc: listing.CalcFixed, Amount: 200_000, Active: true}
	q = listing.PriceCharter(p, listing.PricingInput{OneWayTenths: 303, PickupAt: sat0800, Holiday: true})
	require.Equal(t, []listing.SurchargeLine{{Kind: "weekend", Calc: "fixed", Rate: 200_000, Amount: 200_000}}, q.Surcharges)
	require.Equal(t, int64(763_600), q.Total)

	// A Wednesday holiday.
	q = listing.PriceCharter(pricing7(), listing.PricingInput{OneWayTenths: 303, PickupAt: wed0800, Holiday: true})
	require.Equal(t, []string{"holiday"}, kinds(q))
	require.Equal(t, int64(732_680), q.Total)

	// Sunday is a weekend day too.
	q = listing.PriceCharter(pricing7(), listing.PricingInput{OneWayTenths: 303, PickupAt: sat0800.AddDate(0, 0, 1)})
	require.Equal(t, int64(619_960), q.Total)
}

func TestPriceCharter_PercentOnFareIncludingBothLegs(t *testing.T) {
	q := listing.PriceCharter(pricing7(), listing.PricingInput{OneWayTenths: 303, RoundTrip: true, PickupAt: sat0800})
	require.Equal(t, int64(927_200), q.Fare)
	require.Equal(t, []listing.SurchargeLine{
		{Kind: "weekend", Calc: "percent", Rate: 1000, Amount: 92_720},
		{Kind: "round_trip_waiting", Calc: "fixed", Rate: 150_000, Amount: 150_000},
	}, q.Surcharges)
	require.Equal(t, int64(1_169_920), q.Total)
}

func TestPriceCharter_ZeroSurchargesNoLines(t *testing.T) {
	p := pricing7()
	for i := range p.Surcharges {
		if i%2 == 0 {
			p.Surcharges[i].Active = false
		} else {
			p.Surcharges[i].Amount = 0
		}
	}
	q := listing.PriceCharter(p, listing.PricingInput{OneWayTenths: 303, RoundTrip: true, PickupAt: time.Date(2026, 11, 14, 23, 0, 0, 0, clock.VN), Holiday: true})
	require.Empty(t, q.Surcharges)
	require.Equal(t, int64(927_200), q.Total)

	// No surcharge rows at all.
	q = listing.PriceCharter(listing.CharterPricing{BaseFare: 0, PricePerKm: 12_000, NightStart: ptr("22:00"), NightEnd: ptr("05:00")},
		listing.PricingInput{OneWayTenths: 303, RoundTrip: true, PickupAt: time.Date(2026, 11, 14, 23, 0, 0, 0, clock.VN), Holiday: true})
	require.Empty(t, q.Surcharges)
	require.Equal(t, int64(727_200), q.Total)
}

// Review Focus 3: the weekday and the holiday are read in Vietnam time.
func TestCharterQuote_VNDateForWeekendAndHoliday(t *testing.T) {
	friUTC := time.Date(2026, 11, 13, 17, 30, 0, 0, time.UTC) // 00:30 Saturday in Vietnam
	q := listing.PriceCharter(pricing7(), listing.PricingInput{OneWayTenths: 303, PickupAt: friUTC})
	require.Equal(t, []listing.SurchargeLine{
		{Kind: "night", Calc: "fixed", Rate: 100_000, Amount: 100_000},
		{Kind: "weekend", Calc: "percent", Rate: 1000, Amount: 56_360},
	}, q.Surcharges)
	require.Equal(t, int64(719_960), q.Total)

	// Through the database: Saturday 14/11 is a holiday, read on the Vietnam date.
	e, p := charterEnv(t)
	ctx := context.Background()
	l, c7, _ := e.charterPublished(t, p)
	require.NoError(t, e.Settings.AddHoliday(ctx, e.Admin, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), "Ngày lễ thử"))
	route, err := e.Svc.CharterRoute(ctx, airport, hoiAn)
	require.NoError(t, err)
	quote := func(at time.Time) listing.CharterQuote {
		var out []listing.CharterQuote
		inTx(t, e, func(tx pgx.Tx) error {
			var err error
			out, err = e.Svc.CharterQuoteTx(ctx, tx, l.ID, &c7.ID, listing.CharterTrip{Pickup: airport, Dropoff: hoiAn, PickupAt: at}, route)
			return err
		})
		require.Len(t, out, 1)
		return out[0]
	}
	q = quote(friUTC)
	require.Equal(t, []string{"night", "holiday"}, kinds(q))
	require.Equal(t, int64(832_680), q.Total)
	// 16:30 UTC is 23:30 Friday in Vietnam: night only.
	q = quote(time.Date(2026, 11, 13, 16, 30, 0, 0, time.UTC))
	require.Equal(t, []string{"night"}, kinds(q))
	require.Equal(t, int64(663_600), q.Total)
}

func TestCheckCharterTime(t *testing.T) {
	now := charterNow // Tuesday 10/11 09:00; 2 h payment + 1 h answer
	at := func(h, m int) time.Time { return time.Date(2026, 11, 10, h, m, 0, 0, clock.VN) }
	check := func(pickup time.Time, roundTrip bool, ret *time.Time) (time.Time, error) {
		return listing.CheckCharterTime(pickup, roundTrip, ret, 45, now, 2)
	}

	_, err := check(at(11, 59), false, nil)
	requireErr(t, err, http.StatusUnprocessableEntity, "pickup_too_soon")
	require.Contains(t, err.Error(), "Giờ đón quá gần")
	_, err = check(now.Add(-time.Hour), false, nil)
	requireErr(t, err, http.StatusUnprocessableEntity, "pickup_too_soon")
	end, err := check(at(12, 0), false, nil)
	require.NoError(t, err)
	require.Equal(t, at(12, 45), end)

	_, err = check(at(12, 0).Add(30*time.Second), false, nil)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_charter_time")
	_, err = check(time.Time{}, false, nil)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_charter_time")
	far := now.AddDate(0, 0, 180)
	_, err = check(far, false, nil)
	require.NoError(t, err)
	_, err = check(far.Add(time.Minute), false, nil)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_charter_time")

	pickup := at(15, 0)
	ret := pickup.Add(5 * time.Hour)
	_, err = check(pickup, false, &ret)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_charter_time")
	_, err = check(pickup, true, nil)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_charter_time")
	for _, c := range []struct {
		ret time.Time
		ok  bool
	}{
		{pickup.Add(44 * time.Minute), false},
		{pickup.Add(45 * time.Minute), true},
		{pickup.Add(7 * 24 * time.Hour), true},
		{pickup.Add(7*24*time.Hour + time.Minute), false},
		{pickup.Add(5*time.Hour + time.Second), false},
	} {
		end, err := check(pickup, true, &c.ret)
		if c.ok {
			require.NoError(t, err)
			require.Equal(t, c.ret, end, "round trip ends at return_at")
		} else {
			requireErr(t, err, http.StatusUnprocessableEntity, "invalid_charter_time")
		}
	}
}

func charterQuoteTx(t *testing.T, e *env, listingID uuid.UUID, classID *uuid.UUID, trip listing.CharterTrip, route listing.CharterRoute) ([]listing.CharterQuote, error) {
	t.Helper()
	var out []listing.CharterQuote
	var callErr error
	inTx(t, e, func(tx pgx.Tx) error {
		out, callErr = e.Svc.CharterQuoteTx(context.Background(), tx, listingID, classID, trip, route)
		return nil
	})
	return out, callErr
}

func TestCharterQuoteTx_AreaAndMaxKm(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l, c7, c16 := e.charterPublished(t, p)
	route := func(m int64) listing.CharterRoute {
		e.Maps.SetRoute(airportPt, hoiAnPt, maps.Route{DistanceMeters: m, DurationSeconds: 2_700})
		r, err := e.Svc.CharterRoute(ctx, airport, hoiAn)
		require.NoError(t, err)
		return r
	}
	trip := listing.CharterTrip{Pickup: airport, Dropoff: hoiAn, PickupAt: wed0800}

	qs, err := charterQuoteTx(t, e, l.ID, nil, trip, route(30_240))
	require.NoError(t, err)
	require.Len(t, qs, 2)
	require.Equal(t, c7.ID, qs[0].VehicleClassID, "sorted by seats")
	require.Equal(t, "Xe 7 chỗ", qs[0].ClassName)
	require.Equal(t, 7, qs[0].Seats)
	require.Equal(t, "fake", qs[0].Provider)
	require.Equal(t, int64(30_240), qs[0].DistanceMeters)
	require.Equal(t, int64(2_700), qs[0].DurationSeconds)
	require.Equal(t, 45, qs[0].DurationMinutes)
	require.Equal(t, int64(563_600), qs[0].Total)
	require.Equal(t, c16.ID, qs[1].VehicleClassID)
	require.Equal(t, int64(945_400), qs[1].Total)

	// The pickup province must be served; the dropoff province is free.
	out := trip
	out.Pickup.ProvinceCode = "79"
	_, err = charterQuoteTx(t, e, l.ID, nil, out, route(30_240))
	requireErr(t, err, http.StatusUnprocessableEntity, "outside_service_area")
	require.Contains(t, err.Error(), "khu vực phục vụ")
	in46 := trip
	in46.Pickup.ProvinceCode = "46"
	in46.Dropoff.ProvinceCode = "79"
	_, err = charterQuoteTx(t, e, l.ID, nil, in46, route(30_240))
	require.NoError(t, err)

	// max 300.0 km against the one-way distance.
	qs, err = charterQuoteTx(t, e, l.ID, &c7.ID, trip, route(300_000))
	require.NoError(t, err)
	require.Len(t, qs, 1)
	require.Equal(t, int64(3000), qs[0].DistanceTenths)
	require.Equal(t, int64(200_000+12_000*300), qs[0].Total)
	_, err = charterQuoteTx(t, e, l.ID, &c7.ID, trip, route(300_001))
	requireErr(t, err, http.StatusUnprocessableEntity, "distance_exceeds_max")
	rt := trip
	rt.RoundTrip = true
	ret := wed0800.Add(5 * time.Hour)
	rt.ReturnAt = &ret
	qs, err = charterQuoteTx(t, e, l.ID, &c7.ID, rt, route(200_000))
	require.NoError(t, err, "round trip 400 km billed, 200 km one way")
	require.Equal(t, int64(4000), qs[0].BilledTenths)
	require.Equal(t, int64(200_000+12_000*400+150_000), qs[0].Total)

	// A disabled class, an unknown class or another listing's class is 404.
	_, err = e.Svc.UpdateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, c16.ID, listing.CharterClassPatch{Active: ptr(false)})
	require.NoError(t, err)
	_, err = charterQuoteTx(t, e, l.ID, &c16.ID, trip, route(30_240))
	requireErr(t, err, http.StatusNotFound, "not_found")
	unknown := uuid.Must(uuid.NewV7())
	_, err = charterQuoteTx(t, e, l.ID, &unknown, trip, route(30_240))
	requireErr(t, err, http.StatusNotFound, "not_found")
	qs, err = charterQuoteTx(t, e, l.ID, nil, trip, route(30_240))
	require.NoError(t, err)
	require.Len(t, qs, 1, "inactive classes are not quoted")

	other := e.charterListing(t, p, "Xe bao chuyến khác")
	_, err = charterQuoteTx(t, e, other.ID, &c7.ID, trip, route(30_240))
	requireErr(t, err, http.StatusNotFound, "not_found")
}

func TestQuoteCharter_AllActiveClasses(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l, c7, c16 := e.charterPublished(t, p)
	in := listing.CharterQuoteInput{ListingID: l.ID, Pickup: airport, Dropoff: hoiAn, PickupAt: wed0800}

	res, err := e.Svc.QuoteCharter(ctx, in)
	require.NoError(t, err)
	require.InDelta(t, 30.3, res.DistanceKm, 1e-9)
	require.Equal(t, 45, res.DurationMinutes)
	require.Len(t, res.Quotes, 2)
	require.Equal(t, c7.ID, res.Quotes[0].VehicleClassID)
	require.Equal(t, int64(563_600), res.Quotes[0].Total)
	require.Equal(t, c16.ID, res.Quotes[1].VehicleClassID)
	require.Equal(t, int64(945_400), res.Quotes[1].Total)
	require.Equal(t, 1, e.Maps.Calls(), "one map call for every class")

	// Round trip on Saturday.
	rt := in
	rt.PickupAt, rt.IsRoundTrip = sat0800, true
	ret := sat0800.Add(6 * time.Hour)
	rt.ReturnAt = &ret
	res, err = e.Svc.QuoteCharter(ctx, rt)
	require.NoError(t, err)
	require.Equal(t, int64(1_169_920), res.Quotes[0].Total)

	// Times are checked against the route duration.
	short := pickupPlus(rt, 44*time.Minute)
	_, err = e.Svc.QuoteCharter(ctx, short)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_charter_time")
	soon := in
	soon.PickupAt = charterNow.Add(2*time.Hour + 59*time.Minute)
	_, err = e.Svc.QuoteCharter(ctx, soon)
	requireErr(t, err, http.StatusUnprocessableEntity, "pickup_too_soon")

	// Coordinates outside Vietnam never reach the map.
	calls := e.Maps.Calls()
	bad := in
	bad.Dropoff = listing.CharterPlace{Address: "Singapore", Lat: 1.0, Lng: 100.0}
	_, err = e.Svc.QuoteCharter(ctx, bad)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_location")
	require.Contains(t, err.Error(), "lãnh thổ Việt Nam")
	noProvince := in
	noProvince.Pickup.ProvinceCode = " "
	_, err = e.Svc.QuoteCharter(ctx, noProvince)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_location")
	require.Equal(t, calls, e.Maps.Calls())

	// The map is down: 503, and the error is not cached.
	e.Maps.SetErr(maps.ErrUnavailable)
	_, err = e.Svc.QuoteCharter(ctx, in)
	requireErr(t, err, http.StatusServiceUnavailable, "map_unavailable")
	require.Contains(t, err.Error(), "Chưa tính được quãng đường")
	e.Maps.SetErr(maps.ErrNoRoute)
	_, err = e.Svc.QuoteCharter(ctx, in)
	requireErr(t, err, http.StatusUnprocessableEntity, "no_route")
	e.Maps.SetErr(nil)

	// Hidden, missing or not a charter: 404, without calling the map.
	calls = e.Maps.Calls()
	att := e.approved(t, p, "Vé tham quan")
	missing := in
	missing.ListingID = uuid.Must(uuid.NewV7())
	notCharter := in
	notCharter.ListingID = att.ID
	_, err = e.Svc.Hide(ctx, e.Admin, l.ID, "Vi phạm")
	require.NoError(t, err)
	for _, x := range []listing.CharterQuoteInput{in, missing, notCharter} {
		_, err = e.Svc.QuoteCharter(ctx, x)
		requireErr(t, err, http.StatusNotFound, "not_found")
	}
	require.Equal(t, calls, e.Maps.Calls())
}

func pickupPlus(in listing.CharterQuoteInput, d time.Duration) listing.CharterQuoteInput {
	r := in.PickupAt.Add(d)
	in.ReturnAt = &r
	return in
}

// probeMaps records whether a pool connection is held while the map is
// called: a quote never keeps a transaction open across the provider call.
type probeMaps struct {
	*maps.Fake
	pool     *pgxpool.Pool
	acquired atomic.Int32
}

func (p *probeMaps) Route(ctx context.Context, from, to maps.Point) (maps.Route, error) {
	p.acquired.Store(p.pool.Stat().AcquiredConns())
	return p.Fake.Route(ctx, from, to)
}

func TestQuoteCharter_MapCalledOutsideTx(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l, _, _ := e.charterPublished(t, p)
	probe := &probeMaps{Fake: e.Maps, pool: e.Pool}
	probe.acquired.Store(-1)
	svc := listing.NewService(listing.Deps{
		Pool: e.Pool, Clock: e.Clock, Partners: e.Partner, Catalog: catalog.NewService(e.Pool, e.Clock),
		Geo: geo.NewService(e.Pool), Files: e.Files, BookingGuard: booking.Guard{}, Maps: probe, Holidays: e.Settings,
	})
	res, err := svc.QuoteCharter(ctx, listing.CharterQuoteInput{ListingID: l.ID, Pickup: airport, Dropoff: hoiAn, PickupAt: wed0800})
	require.NoError(t, err)
	require.Len(t, res.Quotes, 2)
	require.Equal(t, int32(0), probe.acquired.Load(), "no connection (and so no transaction) is held during the map call")
}

func TestHTTP_CharterQuote(t *testing.T) {
	e, p := charterEnv(t)
	l, c7, _ := e.charterPublished(t, p)
	body := func() map[string]any {
		return map[string]any{
			"listing_id": l.ID,
			"pickup":     map[string]any{"address": "Sân bay Đà Nẵng", "lat": 16.0544, "lng": 108.2022, "province_code": "48"},
			"dropoff":    map[string]any{"address": "Phố cổ Hội An", "lat": 15.8801, "lng": 108.3380},
			"pickup_at":  "2026-11-11T08:00:00+07:00",
		}
	}

	rec := e.do(t, "POST", "/v1/charter-quotes", "", body())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var res struct {
		DistanceKm      float64 `json:"distance_km"`
		DurationMinutes int     `json:"duration_minutes"`
		Quotes          []struct {
			VehicleClassID  uuid.UUID       `json:"vehicle_class_id"`
			ClassName       string          `json:"class_name"`
			Seats           int             `json:"seats"`
			DistanceKm      float64         `json:"distance_km"`
			BilledKm        float64         `json:"billed_km"`
			DurationMinutes int             `json:"duration_minutes"`
			IsRoundTrip     bool            `json:"is_round_trip"`
			DistanceTotal   int64           `json:"distance_total"`
			Fare            int64           `json:"fare"`
			Surcharges      json.RawMessage `json:"surcharges"`
			Total           int64           `json:"total"`
		} `json:"quotes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	require.Equal(t, 30.3, res.DistanceKm)
	require.Equal(t, 45, res.DurationMinutes)
	require.Len(t, res.Quotes, 2)
	q := res.Quotes[0]
	require.Equal(t, c7.ID, q.VehicleClassID)
	require.Equal(t, "Xe 7 chỗ", q.ClassName)
	require.Equal(t, 30.3, q.DistanceKm)
	require.Equal(t, 30.3, q.BilledKm)
	require.Equal(t, int64(363_600), q.DistanceTotal)
	require.Equal(t, int64(563_600), q.Total)
	require.JSONEq(t, `[]`, string(q.Surcharges))

	// A quote round-trips through JSON (it is stored as the booking snapshot).
	var full struct {
		Quotes []listing.CharterQuote `json:"quotes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &full))
	require.Equal(t, int64(303), full.Quotes[0].DistanceTenths)
	require.Equal(t, int64(303), full.Quotes[0].BilledTenths)
	require.Equal(t, int64(563_600), full.Quotes[0].Total)

	unknown := body()
	unknown["coupon"] = "X"
	requireCode(t, e.do(t, "POST", "/v1/charter-quotes", "", unknown), http.StatusBadRequest, "invalid_request")
	noAddr := body()
	noAddr["dropoff"] = map[string]any{"lat": 15.8801, "lng": 108.3380}
	requireCode(t, e.do(t, "POST", "/v1/charter-quotes", "", noAddr), http.StatusUnprocessableEntity, "invalid_location")
	requireCode(t, e.do(t, "POST", "/v1/charter-quotes", "", `{"listing_id": "x"`), http.StatusBadRequest, "invalid_request")

	// 20 per minute per IP: 4 sent so far.
	for i := 0; i < 16; i++ {
		require.Equal(t, http.StatusOK, e.do(t, "POST", "/v1/charter-quotes", "", body()).Code)
	}
	requireCode(t, e.do(t, "POST", "/v1/charter-quotes", "", body()), http.StatusTooManyRequests, "too_many_requests")
}

func TestSearch_CharterByServiceArea(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l, c7, c16 := e.charterPublished(t, p)

	cards := e.search(t, listing.SearchFilter{ProvinceCode: ptr("46")})
	require.Equal(t, []string{l.Slug}, slugs(cards), "served province, not the operator's")
	require.NotNil(t, cards[0].PriceFrom)
	require.Equal(t, int64(212_000), *cards[0].PriceFrom, "Xe 7 chỗ 200,000 + 12,000 beats Xe 16 chỗ 418,000")
	require.Equal(t, []string{l.Slug}, slugs(e.search(t, listing.SearchFilter{ProvinceCode: ptr("48"), ServiceType: ptr(charter)})))
	require.Empty(t, e.search(t, listing.SearchFilter{ProvinceCode: ptr("79")}))

	// An attraction is still matched on its own province only.
	att := e.approved(t, p, "Vé tham quan")
	var attProvince string
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT province_code FROM listings WHERE id = $1`, att.ID).Scan(&attProvince))
	require.Contains(t, slugs(e.search(t, listing.SearchFilter{ProvinceCode: &attProvince})), att.Slug)
	if attProvince != "46" {
		require.NotContains(t, slugs(e.search(t, listing.SearchFilter{ProvinceCode: ptr("46")})), att.Slug)
	}

	// No active class: the listing cannot be quoted, so it is hidden.
	for _, c := range []listing.CharterClass{c7, c16} {
		_, err := e.Svc.UpdateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, listing.CharterClassPatch{Active: ptr(false)})
		require.NoError(t, err)
	}
	require.Empty(t, e.search(t, listing.SearchFilter{ProvinceCode: ptr("46")}))
	require.NotContains(t, slugs(e.search(t, listing.SearchFilter{})), l.Slug)
}
