package listing_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
)

// approvedRide is a visible HCM -> Đà Lạt listing. Its one schedule starts
// far in the future so only the test's one-off trips are on sale.
func (e *env) approvedRide(t *testing.T, p partner.Partner, title string) listing.Listing {
	t.Helper()
	ctx := context.Background()
	l := e.rideListing(t, p, title)
	_, _, err := e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{e.upload(t, p.OwnerUserID, "listing_image")}, nil)
	require.NoError(t, err)
	_, err = e.Svc.SetRideRoute(ctx, p.ID, p.OwnerUserID, l.ID, e.hcmToDalat(t))
	require.NoError(t, err)
	in := daily("07:00")
	in.ValidFrom = "2027-06-01"
	e.schedule(t, p, l, in)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	out, err := e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	return out
}

func (e *env) searchTrips(t *testing.T, f listing.TripSearchFilter) []listing.TripCard {
	t.Helper()
	cards, err := e.Svc.SearchTrips(context.Background(), f)
	require.NoError(t, err)
	return cards
}

func hcmDalat(date string) listing.TripSearchFilter {
	return listing.TripSearchFilter{OriginProvince: "79", DestProvince: "68", Date: date}
}

func tripIDs(cards []listing.TripCard) []uuid.UUID {
	out := []uuid.UUID{}
	for _, c := range cards {
		out = append(out, c.TripID)
	}
	return out
}

func TestSearchTrips_MatchesProvincesAndSeatsLeft(t *testing.T) {
	e, p := rideEnv(t)
	l := e.approvedRide(t, p, "Sài Gòn - Đà Lạt")
	full := e.oneOff(t, p, l, vnAt(2026, 11, 6, 8, 0), 10)
	last := e.oneOff(t, p, l, vnAt(2026, 11, 6, 9, 0), 10)
	two := e.oneOff(t, p, l, vnAt(2026, 11, 6, 10, 0), 10)
	e.reserve(t, full.ID, 10)
	e.reserve(t, last.ID, 9)
	e.reserve(t, two.ID, 8)

	f := hcmDalat("2026-11-06")
	cards := e.searchTrips(t, f) // Seats 0 means the default, 1 passenger
	require.Equal(t, []uuid.UUID{last.ID, two.ID}, tripIDs(cards))
	require.Equal(t, 1, cards[0].SeatsLeft)
	require.Equal(t, 2, cards[1].SeatsLeft)
	c := cards[0]
	require.Equal(t, l.ID, c.Listing.ID)
	require.Equal(t, l.Slug, c.Listing.Slug)
	require.Equal(t, "Sài Gòn - Đà Lạt", c.Listing.Title)
	require.Equal(t, "Bến xe Miền Đông", c.Origin.Name)
	require.Equal(t, "Bến xe Đà Lạt", c.Dest.Name)
	require.Equal(t, 420, c.DurationMinutes)
	require.Equal(t, "Xe giường nằm 34 chỗ", c.VehicleDescription)
	require.Equal(t, int64(250_000), c.PricePerSeat)
	require.True(t, c.DepartureAt.Equal(vnAt(2026, 11, 6, 9, 0)))
	require.True(t, c.ArriveAt.Equal(vnAt(2026, 11, 6, 16, 0)))

	f.Seats = 2
	require.Equal(t, []uuid.UUID{two.ID}, tripIDs(e.searchTrips(t, f)))
	f.Seats = 3
	require.Empty(t, e.searchTrips(t, f))

	// The reverse direction does not match.
	require.Empty(t, e.searchTrips(t, listing.TripSearchFilter{OriginProvince: "68", DestProvince: "79", Date: "2026-11-06"}))
}

// A trip with 2 seats left is hidden for 3 passengers and shown for 2.
func TestSearchTrips_PassengersFilter(t *testing.T) {
	e, p := rideEnv(t)
	l := e.approvedRide(t, p, "Sài Gòn - Đà Lạt")
	tr := e.oneOff(t, p, l, vnAt(2026, 11, 6, 8, 0), 5)
	e.reserve(t, tr.ID, 3)
	f := hcmDalat("2026-11-06")
	f.Seats = 3
	require.Empty(t, e.searchTrips(t, f))
	f.Seats = 2
	require.Len(t, e.searchTrips(t, f), 1)
}

func TestSearchTrips_DateIsVNDepartureDate(t *testing.T) {
	e, p := rideEnv(t)
	l := e.approvedRide(t, p, "Sài Gòn - Đà Lạt")
	late := e.oneOff(t, p, l, vnAt(2026, 11, 9, 23, 45), 10) // Monday
	early := e.oneOff(t, p, l, vnAt(2026, 11, 10, 0, 15), 10)
	require.Equal(t, []uuid.UUID{late.ID}, tripIDs(e.searchTrips(t, hcmDalat("2026-11-09"))))
	require.Equal(t, []uuid.UUID{early.ID}, tripIDs(e.searchTrips(t, hcmDalat("2026-11-10"))))
}

func TestSearchTrips_BookingCutoff(t *testing.T) {
	e, p := rideEnv(t) // now 09:00, default cutoff 60 minutes
	l := e.approvedRide(t, p, "Sài Gòn - Đà Lạt")
	soon := e.oneOff(t, p, l, vnAt(2026, 11, 5, 9, 59), 10)
	edge := e.oneOff(t, p, l, vnAt(2026, 11, 5, 10, 0), 10)
	ok := e.oneOff(t, p, l, vnAt(2026, 11, 5, 10, 1), 10)
	f := hcmDalat("2026-11-05")
	require.Equal(t, []uuid.UUID{ok.ID}, tripIDs(e.searchTrips(t, f)), "now+59m and now+60m hidden, now+61m shown")

	// Cutoff 0: hold 15 + grace 5 = 20 minutes still applies.
	_, err := e.Pool.Exec(context.Background(), `UPDATE platform_settings SET ride_booking_cutoff_minutes = 0`)
	require.NoError(t, err)
	a := e.oneOff(t, p, l, vnAt(2026, 11, 5, 9, 19), 10)
	b := e.oneOff(t, p, l, vnAt(2026, 11, 5, 9, 20), 10)
	c := e.oneOff(t, p, l, vnAt(2026, 11, 5, 9, 21), 10)
	got := tripIDs(e.searchTrips(t, f))
	require.NotContains(t, got, a.ID)
	require.NotContains(t, got, b.ID)
	require.Equal(t, []uuid.UUID{c.ID, soon.ID, edge.ID, ok.ID}, got)
}

func TestSearchTrips_HidesInvisibleListings(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.approvedRide(t, p, "Sài Gòn - Đà Lạt")
	tr := e.oneOff(t, p, l, vnAt(2026, 11, 6, 8, 0), 10)
	f := hcmDalat("2026-11-06")
	require.Equal(t, []uuid.UUID{tr.ID}, tripIDs(e.searchTrips(t, f)))

	_, err := e.Svc.Hide(ctx, e.Admin, l.ID, "Vi phạm")
	require.NoError(t, err)
	require.Empty(t, e.searchTrips(t, f))
	_, err = e.Svc.Unhide(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	require.Len(t, e.searchTrips(t, f), 1)

	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, p.ID, "Vi phạm"))
	require.Empty(t, e.searchTrips(t, f))
	require.NoError(t, e.Partner.Unsuspend(ctx, e.Admin, p.ID))
	require.Len(t, e.searchTrips(t, f), 1)

	require.NoError(t, e.Partner.DisableServiceType(ctx, e.Admin, p.ID, ride))
	require.Empty(t, e.searchTrips(t, f))
	require.NoError(t, e.Partner.EnableServiceType(ctx, e.Admin, p.ID, ride))
	require.Len(t, e.searchTrips(t, f), 1)

	// Cancelled trips never show.
	_, err = e.Svc.CancelTripByPartner(ctx, p.ID, p.OwnerUserID, l.ID, tr.ID, "Hỏng xe")
	require.NoError(t, err)
	require.Empty(t, e.searchTrips(t, f))
}

func TestSearchTrips_WardFilter(t *testing.T) {
	e, p := rideEnv(t)
	l := e.approvedRide(t, p, "Sài Gòn - Đà Lạt")
	e.oneOff(t, p, l, vnAt(2026, 11, 6, 8, 0), 10)
	route := e.hcmToDalat(t)
	other := "00000"
	f := hcmDalat("2026-11-06")
	f.OriginWard = &route.Origin.WardCode
	f.DestWard = &route.Dest.WardCode
	require.Len(t, e.searchTrips(t, f), 1)
	f.DestWard = &other
	require.Empty(t, e.searchTrips(t, f))
	f.DestWard = nil
	f.OriginWard = &other
	require.Empty(t, e.searchTrips(t, f))
	blank := ""
	f.OriginWard = &blank
	require.Len(t, e.searchTrips(t, f), 1, "blank ward is no filter")
}

func TestSearchTrips_OrderAndPaging(t *testing.T) {
	e, p := rideEnv(t)
	a := e.approvedRide(t, p, "A")
	ta := e.oneOff(t, p, a, vnAt(2026, 11, 6, 9, 0), 10)
	b := e.approvedRide(t, p, "B")
	tb := e.oneOff(t, p, b, vnAt(2026, 11, 6, 8, 0), 10)
	f := hcmDalat("2026-11-06")
	require.Equal(t, []uuid.UUID{tb.ID, ta.ID}, tripIDs(e.searchTrips(t, f)))
	f.Limit, f.Offset = 1, 1
	require.Equal(t, []uuid.UUID{ta.ID}, tripIDs(e.searchTrips(t, f)))
}

func TestSearchTrips_BadParams400(t *testing.T) {
	e, _ := rideEnv(t)
	ctx := context.Background()
	bad := []listing.TripSearchFilter{
		{DestProvince: "68", Date: "2026-11-06"},
		{OriginProvince: "79", Date: "2026-11-06"},
		{OriginProvince: "79", DestProvince: "68"},
		{OriginProvince: "79", DestProvince: "68", Date: "06/11/2026"},
		{OriginProvince: "79", DestProvince: "68", Date: "2026-11-04"}, // yesterday
		{OriginProvince: "79", DestProvince: "68", Date: "2027-11-06"}, // beyond today + 365
		{OriginProvince: "79", DestProvince: "68", Date: "2026-11-06", Seats: -1},
		{OriginProvince: "79", DestProvince: "68", Date: "2026-11-06", Seats: 1001},
		{OriginProvince: "79", DestProvince: "68", Date: "2026-11-06", Limit: -1},
		{OriginProvince: "79", DestProvince: "68", Date: "2026-11-06", Offset: 1001},
	}
	for _, f := range bad {
		_, err := e.Svc.SearchTrips(ctx, f)
		requireErr(t, err, http.StatusBadRequest, "invalid_request")
	}
	// Today and today + 365 are allowed.
	_, err := e.Svc.SearchTrips(ctx, hcmDalat("2026-11-05"))
	require.NoError(t, err)
	_, err = e.Svc.SearchTrips(ctx, hcmDalat("2027-11-05"))
	require.NoError(t, err)
}

type tripPage struct {
	Items []map[string]any `json:"items"`
}

func TestHTTP_SearchTrips(t *testing.T) {
	e, p := rideEnv(t)
	l := e.approvedRide(t, p, "Sài Gòn - Đà Lạt")
	tr := e.oneOff(t, p, l, vnAt(2026, 11, 6, 8, 0), 10)
	e.reserve(t, tr.ID, 7)
	q := url.Values{"origin_province": {"79"}, "dest_province": {"68"}, "date": {"2026-11-06"}, "passengers": {"3"}, "limit": {"10"}, "offset": {"0"}}
	rec := e.do(t, "GET", "/v1/ride-trips?"+q.Encode(), "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))
	page := decode[tripPage](t, rec.Body.Bytes())
	require.Len(t, page.Items, 1)
	it := page.Items[0]
	require.Equal(t, tr.ID.String(), it["trip_id"])
	require.Equal(t, float64(3), it["seats_left"])
	require.Equal(t, "Xe giường nằm 34 chỗ", it["vehicle_description"])
	require.Equal(t, float64(250000), it["price_per_seat"])
	require.Equal(t, float64(420), it["duration_minutes"])
	require.Equal(t, l.Slug, it["listing"].(map[string]any)["slug"])
	require.Equal(t, "Bến xe Đà Lạt", it["dest"].(map[string]any)["name"])

	q.Set("passengers", "4")
	rec = e.do(t, "GET", "/v1/ride-trips?"+q.Encode(), "", nil)
	require.Empty(t, decode[tripPage](t, rec.Body.Bytes()).Items)

	for _, path := range []string{
		"/v1/ride-trips?origin_province=79&dest_province=68",
		"/v1/ride-trips?origin_province=79&dest_province=68&date=2026-11-06&passengers=x",
		"/v1/ride-trips?origin_province=79&dest_province=68&date=2026-11-06&passengers=0",
		"/v1/ride-trips?origin_province=79&dest_province=68&date=2026-11-06&limit=x",
		"/v1/ride-trips?origin_province=79&dest_province=68&date=2026-11-06&offset=x",
	} {
		rec := e.do(t, "GET", path, "", nil)
		requireCode(t, rec, http.StatusBadRequest, "invalid_request")
		require.Empty(t, rec.Header().Get("Cache-Control"), path)
	}
}

func TestPublicDetail_Ride(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.approvedRide(t, p, "Sài Gòn - Đà Lạt")
	inactive := daily("22:00")
	inactive.ValidFrom = "2027-06-01"
	inactive.SeatCapacity = 10
	s2 := e.schedule(t, p, l, inactive)
	_, err := e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, s2.ID, listing.RideSchedulePatch{Active: ptr(false)})
	require.NoError(t, err)

	d, err := e.Svc.PublicDetail(ctx, l.Slug)
	require.NoError(t, err)
	require.NotNil(t, d.Ride)
	require.Nil(t, d.Attraction)
	require.Equal(t, "Bến xe Miền Đông", d.Ride.Route.Origin.Name)
	require.Len(t, d.Ride.Schedules, 1, "active schedules only")
	require.Equal(t, "07:00", d.Ride.Schedules[0].DepartureTime)
	require.NotNil(t, d.PriceFrom)
	require.Equal(t, int64(200_000), *d.PriceFrom)
}

func TestSearch_RidePriceFrom(t *testing.T) {
	e, p := rideEnv(t)
	ctx := context.Background()
	l := e.approvedRide(t, p, "Sài Gòn - Đà Lạt") // one active schedule at 200k
	cheap := daily("22:00")
	cheap.ValidFrom = "2027-06-01"
	cheap.PricePerSeat = 150_000
	cheap.Weekdays = 1
	e.schedule(t, p, l, cheap)
	off := daily("23:00")
	off.ValidFrom = "2027-06-01"
	off.PricePerSeat = 50_000
	off.Weekdays = 2
	s3 := e.schedule(t, p, l, off)
	_, err := e.Svc.UpdateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, s3.ID, listing.RideSchedulePatch{Active: ptr(false)})
	require.NoError(t, err)

	cards := e.search(t, listing.SearchFilter{ServiceType: ptr(ride)})
	require.Len(t, cards, 1)
	require.NotNil(t, cards[0].PriceFrom)
	require.Equal(t, int64(150_000), *cards[0].PriceFrom, "cheapest active schedule")

	d, err := e.Svc.PublicDetail(ctx, l.Slug)
	require.NoError(t, err)
	require.Equal(t, int64(150_000), *d.PriceFrom)
}
