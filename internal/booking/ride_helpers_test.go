package booking_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

const rideType = "scheduled_ride"

// rideEnv is env plus a ride partner with two approved listings, A
// (TP.HCM 79 -> Lâm Đồng 68) and B (68 -> 79), 180 minutes each, commission
// 1000 bps on scheduled_ride, policy [{48,10000},{24,5000}], and the trips
// A1 (2026-11-07 07:00, 250.000/seat, 10 seats) and B1 (2026-11-09 13:00,
// 220.000/seat, 10 seats).
type rideEnv struct {
	*env
	RP     partner.Partner
	A, B   listing.Listing
	A1, B1 listing.Trip
}

func newRideEnv(t *testing.T) *rideEnv {
	t.Helper()
	e := newEnv(t)
	_, err := e.Catalog.CreateCommissionRate(context.Background(), e.Admin, rideType, 1000, nil)
	require.NoError(t, err)
	r := &rideEnv{env: e}
	r.RP = r.ridePartner(t, "Phuong")
	r.A = r.rideListing(t, r.RP, "Sài Gòn - Đà Lạt", "79", "68")
	r.B = r.rideListing(t, r.RP, "Đà Lạt - Sài Gòn", "68", "79")
	r.A1 = r.trip(t, r.RP, r.A, vnAt(2026, 11, 7, 7, 0), 250_000, 10)
	r.B1 = r.trip(t, r.RP, r.B, vnAt(2026, 11, 9, 13, 0), 220_000, 10)
	return r
}

func vnAt(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, clock.VN)
}

func (e *rideEnv) ridePartner(t *testing.T, name string) partner.Partner {
	t.Helper()
	key := strings.ToLower(name)
	p, err := e.Partner.Create(context.Background(), e.Admin, partner.CreateInput{
		LegalName: "Nhà xe " + name, DisplayName: name,
		OwnerEmail: "owner@" + key + ".vn", OwnerPassword: "matkhau123",
		ServiceTypes: []string{rideType},
	})
	require.NoError(t, err)
	return p
}

// stationName names the station of a province in the fixture routes.
func stationName(province string) string {
	switch province {
	case "79":
		return "Bến xe Miền Đông"
	case "68":
		return "Bến xe Đà Lạt"
	default:
		return "Bến xe Giáp Bát"
	}
}

func (e *rideEnv) wardIn(t *testing.T, province string) string {
	t.Helper()
	var w string
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT code FROM wards WHERE province_code = $1 ORDER BY code LIMIT 1`, province).Scan(&w))
	return w
}

// rideListing creates an approved ride listing of p from origin to dest
// (180 minutes). Its only schedule starts in 2027-06, so trip generation
// adds no trip within the 60-day horizon: tests add one-off trips.
func (e *rideEnv) rideListing(t *testing.T, p partner.Partner, title, origin, dest string) listing.Listing {
	t.Helper()
	ctx := context.Background()
	desc := "Xe giường nằm"
	l, err := e.Listing.Create(ctx, p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: rideType, Title: title, Description: &desc})
	require.NoError(t, err)
	_, err = e.Listing.SetRideRoute(ctx, p.ID, p.OwnerUserID, l.ID, listing.RideRouteInput{
		Origin:             listing.RidePoint{ProvinceCode: origin, WardCode: e.wardIn(t, origin), Name: stationName(origin), Address: "1 Đường A"},
		Dest:               listing.RidePoint{ProvinceCode: dest, WardCode: e.wardIn(t, dest), Name: stationName(dest), Address: "2 Đường B"},
		DurationMinutes:    180,
		VehicleDescription: "Xe giường nằm 34 chỗ",
	})
	require.NoError(t, err)
	_, err = e.Listing.CreateRideSchedule(ctx, p.ID, p.OwnerUserID, l.ID, listing.RideScheduleInput{
		DepartureTime: "05:00", Weekdays: 127, SeatCapacity: 30, PricePerSeat: 200_000, ValidFrom: "2027-06-01",
	})
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	f, err := e.Files.Upload(ctx, auth.Principal{UserID: p.OwnerUserID, Role: auth.RolePartner}, "listing_image", &buf)
	require.NoError(t, err)
	_, _, err = e.Listing.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{f.ID}, nil)
	require.NoError(t, err)
	_, err = e.Listing.SetCancellationPolicy(ctx, p.ID, p.OwnerUserID, l.ID, []listing.CancellationTier{
		{HoursBefore: 48, RefundBps: 10000}, {HoursBefore: 24, RefundBps: 5000},
	})
	require.NoError(t, err)
	_, err = e.Listing.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	l, err = e.Listing.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	return l
}

// trip adds a one-off trip to l.
func (e *rideEnv) trip(t *testing.T, p partner.Partner, l listing.Listing, at time.Time, price int64, seats int) listing.Trip {
	t.Helper()
	tr, err := e.Listing.CreateTrip(context.Background(), p.ID, p.OwnerUserID, l.ID,
		listing.TripInput{DepartureAt: at, SeatCapacity: seats, PricePerSeat: price})
	require.NoError(t, err)
	return tr
}

// rideInput books seats on outbound (and ret when set).
func rideInput(outbound uuid.UUID, ret *uuid.UUID, seats int) booking.CreateRideInput {
	return booking.CreateRideInput{
		OutboundTripID: outbound, ReturnTripID: ret, Seats: seats,
		ContactName: "Trần Thị Bình", ContactPhone: "0987 654 321", ContactEmail: "Binh@Gmail.com",
	}
}

func (e *rideEnv) createRide(t *testing.T, customer uuid.UUID, key string, in booking.CreateRideInput) booking.CheckoutView {
	t.Helper()
	v, err := e.Svc.CreateRide(context.Background(), customer, key, in)
	require.NoError(t, err)
	return v
}

func (e *rideEnv) seatsReserved(t *testing.T, tripID uuid.UUID) int {
	t.Helper()
	return e.count(t, `SELECT seats_reserved FROM ride_trips WHERE id = $1`, tripID)
}

func (e *rideEnv) status(t *testing.T, table string, id uuid.UUID) string {
	t.Helper()
	var s string
	require.NoError(t, e.Pool.QueryRow(context.Background(), `SELECT status FROM `+table+` WHERE id = $1`, id).Scan(&s))
	return s
}

// checkoutPayment returns the id, status, amount and order code of the only
// payment of a checkout.
func (e *rideEnv) checkoutPayment(t *testing.T, checkoutID uuid.UUID) (uuid.UUID, string, int64, int64) {
	t.Helper()
	var (
		id         uuid.UUID
		st         string
		amount, oc int64
	)
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT id, status, amount, provider_order_code FROM payments WHERE checkout_id = $1`, checkoutID).Scan(&id, &st, &amount, &oc))
	return id, st, amount, oc
}

func ptrID(id uuid.UUID) *uuid.UUID { return &id }
