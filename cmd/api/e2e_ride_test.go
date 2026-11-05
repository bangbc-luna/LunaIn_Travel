package main

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

const e2eRideOwnerEmail, e2eRideOwnerPass = "owner@xetuyen.vn", "matkhau123"

// rideListing builds an approved scheduled ride listing through the API:
// route origin → dest (180 minutes), one daily schedule at departure with
// 10 seats at price, an image; submitted, then approved by the admin.
func (a *e2eApp) rideListing(t *testing.T, pt, admin, title string, origin, dest listing.RidePoint, departure string, price int64) listing.Listing {
	t.Helper()
	var l listing.Listing
	a.call(t, "POST", "/v1/partner/listings", pt, map[string]any{
		"service_type": "scheduled_ride", "title": title, "description": "Xe giường nằm, có nước uống",
	}, http.StatusCreated, &l)
	lp := "/v1/partner/listings/" + l.ID.String()
	a.call(t, "PUT", lp+"/ride-route", pt, map[string]any{
		"origin": origin, "dest": dest, "duration_minutes": 180, "vehicle_description": "Xe giường nằm 34 chỗ",
	}, http.StatusOK, nil)
	a.call(t, "POST", lp+"/ride-schedules", pt, map[string]any{
		"departure_time": departure, "weekdays": 0b1111111, "seat_capacity": 10,
		"price_per_seat": price, "valid_from": "2026-11-05",
	}, http.StatusCreated, nil)
	img := a.uploadPNG(t, pt, "listing_image")
	a.call(t, "PUT", lp+"/images", pt, map[string]any{"file_ids": []uuid.UUID{img}}, http.StatusOK, nil)
	a.call(t, "POST", lp+"/submit", pt, nil, http.StatusOK, nil)
	a.call(t, "POST", "/v1/admin/listings/"+l.ID.String()+"/approve", admin, nil, http.StatusOK, nil)
	return l
}

// searchTrip searches the public trips from → to on date for 2 passengers
// and returns the one of listing l.
func (a *e2eApp) searchTrip(t *testing.T, from, to, date string, l listing.Listing) listing.TripCard {
	t.Helper()
	q := url.Values{"origin_province": {from}, "dest_province": {to}, "date": {date}, "passengers": {"2"}}
	var res struct {
		Items []listing.TripCard `json:"items"`
	}
	a.call(t, "GET", "/v1/ride-trips?"+q.Encode(), "", nil, http.StatusOK, &res)
	for _, c := range res.Items {
		if c.Listing.ID == l.ID {
			return c
		}
	}
	require.FailNow(t, "trip not found", "listing %s on %s: %+v", l.ID, date, res.Items)
	return listing.TripCard{}
}

// TestE2E_RideRoundTrip runs a scheduled ride round trip through the real mux
// with payment.Fake: two reverse listings with daily schedules, search, book
// 2 seats both ways, pay once, check in the outbound leg (completed by the
// job), the return leg becomes no_show, and both settlements are released.
func TestE2E_RideRoundTrip(t *testing.T) {
	// Thursday 2026-11-05 09:00 in Vietnam.
	a := newE2EApp(t, time.Date(2026, 11, 5, 9, 0, 0, 0, clock.VN))
	ctx := context.Background()

	// 1. The admin sets the scheduled_ride commission (10%) and creates a
	// partner offering scheduled rides.
	admin := a.login(t, e2eAdminEmail, e2eAdminPass)
	a.call(t, "POST", "/v1/admin/commission-rates", admin,
		map[string]any{"service_type": "scheduled_ride", "rate_bps": 1000}, http.StatusCreated, nil)
	var p partner.Partner
	a.call(t, "POST", "/v1/admin/partners", admin, map[string]any{
		"legal_name": "Công ty Xe Tuyến", "display_name": "Xe Tuyến",
		"owner_email": e2eRideOwnerEmail, "owner_password": e2eRideOwnerPass,
		"service_types": []string{"scheduled_ride"},
	}, http.StatusCreated, &p)
	pt := a.login(t, e2eRideOwnerEmail, e2eRideOwnerPass)

	// 2. Listing A goes 79 → 68 at 07:00 (250.000), listing B 68 → 79 at
	// 13:00 (220.000); both approved, trips generated on schedule creation.
	ward := func(province string) string {
		var w string
		require.NoError(t, a.pool.QueryRow(ctx, `SELECT code FROM wards WHERE province_code = $1 ORDER BY code LIMIT 1`, province).Scan(&w))
		return w
	}
	hcm := listing.RidePoint{ProvinceCode: "79", WardCode: ward("79"), Name: "Bến xe Miền Đông", Address: "292 Đinh Bộ Lĩnh"}
	dalat := listing.RidePoint{ProvinceCode: "68", WardCode: ward("68"), Name: "Bến xe Đà Lạt", Address: "1 Tô Hiến Thành"}
	la := a.rideListing(t, pt, admin, "Sài Gòn - Đà Lạt", hcm, dalat, "07:00", 250_000)
	lb := a.rideListing(t, pt, admin, "Đà Lạt - Sài Gòn", dalat, hcm, "13:00", 220_000)

	// 3. The customer searches both directions on Saturday and books a round
	// trip for 2 passengers.
	a.call(t, "POST", "/v1/auth/register", "",
		map[string]string{"email": e2eCustomerEmail, "password": e2eCustomerPass}, http.StatusAccepted, nil)
	var mail string
	require.NoError(t, a.pool.QueryRow(ctx, `SELECT args->>'text' FROM river_job
		WHERE kind = 'send_email' AND args->>'to' = $1 ORDER BY id DESC LIMIT 1`, e2eCustomerEmail).Scan(&mail))
	code := sixDigits.FindString(mail)
	require.NotEmpty(t, code, mail)
	var tk identity.Tokens
	a.call(t, "POST", "/v1/auth/verify-email", "",
		map[string]string{"email": e2eCustomerEmail, "code": code, "password": e2eCustomerPass}, http.StatusOK, &tk)
	ct := tk.AccessToken

	out := a.searchTrip(t, "79", "68", "2026-11-07", la)
	ret := a.searchTrip(t, "68", "79", "2026-11-07", lb)
	departA := time.Date(2026, 11, 7, 7, 0, 0, 0, clock.VN)
	departB := time.Date(2026, 11, 7, 13, 0, 0, 0, clock.VN)
	require.True(t, out.DepartureAt.Equal(departA), out.DepartureAt)
	require.True(t, ret.DepartureAt.Equal(departB), ret.DepartureAt)
	require.Equal(t, 10, out.SeatsLeft)
	require.Equal(t, "Xe giường nằm 34 chỗ", out.VehicleDescription)

	const gross, net = int64(940_000), int64(846_000) // 2×250.000 + 2×220.000, minus 10%
	var co booking.CheckoutView
	a.call(t, "POST", "/v1/bookings/ride", ct, map[string]any{
		"outbound_trip_id": out.TripID, "return_trip_id": ret.TripID, "seats": 2,
		"contact_name": "Nguyễn Văn An", "contact_phone": "0912345678", "contact_email": e2eCustomerEmail,
	}, http.StatusCreated, &co, "Idempotency-Key", "e2e-ride-1")
	require.Equal(t, gross, co.TotalAmount)
	require.Len(t, co.Bookings, 2)
	outbound, back := co.Bookings[0], co.Bookings[1]
	require.Equal(t, "outbound", outbound.Ride.Direction)
	require.Equal(t, "return", back.Ride.Direction)

	// 4. One Fake payment confirms both legs; the partner's pending is the
	// net of both.
	a.call(t, "POST", "/v1/dev/payments/"+orderCode(t, outbound)+"/simulate-paid", "", map[string]any{}, http.StatusOK, nil)
	for _, b := range []*booking.BookingView{&outbound, &back} {
		a.call(t, "GET", "/v1/bookings/"+b.ID.String(), ct, nil, http.StatusOK, b)
		require.Equal(t, booking.StatusConfirmed, b.Status)
		require.NotNil(t, b.QRToken)
	}
	require.Equal(t, ledger.Balance{Pending: net}, a.balance(t, pt))

	// 5. At A's departure the partner checks the outbound leg in by QR;
	// CompleteRides completes it once the 180-minute ride is over.
	a.clock.T = departA
	pt = a.login(t, e2eRideOwnerEmail, e2eRideOwnerPass)
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	var res booking.CheckInResult
	a.call(t, "POST", "/v1/partner/check-ins", pt, map[string]string{"qr_token": *outbound.QRToken}, http.StatusOK, &res)
	require.Equal(t, booking.StatusCheckedIn, res.Booking.Status)
	a.clock.T = departA.Add(180 * time.Minute)
	require.NoError(t, a.complete.Work(ctx, nil))
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	a.call(t, "GET", "/v1/bookings/"+outbound.ID.String(), ct, nil, http.StatusOK, &outbound)
	require.Equal(t, booking.StatusCompleted, outbound.Status)

	// 6. Nobody checks the return leg in: 61 minutes after B's departure it
	// becomes no_show.
	a.clock.T = departB.Add(61 * time.Minute)
	require.NoError(t, a.noShow.Work(ctx, nil))
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	a.call(t, "GET", "/v1/bookings/"+back.ID.String(), ct, nil, http.StatusOK, &back)
	require.Equal(t, booking.StatusNoShow, back.Status)

	// 7. A day later (release_delay_days = 1) both legs are released; the
	// ledger balances and both trips still hold their 2 seats.
	a.clock.T = a.clock.T.Add(24 * time.Hour)
	require.NoError(t, a.release.Work(ctx, nil))
	pt = a.login(t, e2eRideOwnerEmail, e2eRideOwnerPass)
	require.Equal(t, ledger.Balance{Available: net}, a.balance(t, pt))
	require.NoError(t, a.ledger.Verify(ctx, p.ID))
	for _, id := range []uuid.UUID{out.TripID, ret.TripID} {
		var seats int
		require.NoError(t, a.pool.QueryRow(ctx, `SELECT seats_reserved FROM ride_trips WHERE id = $1`, id).Scan(&seats))
		require.Equal(t, 2, seats)
	}
	var platform int64
	require.NoError(t, a.pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::bigint FROM platform_commission_entries`).Scan(&platform))
	require.Equal(t, gross-net, platform)
}
