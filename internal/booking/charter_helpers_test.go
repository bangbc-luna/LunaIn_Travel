package booking_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/maps"
)

const charterType = "private_charter"

var (
	// charterNow is Tuesday 2026-11-10 09:00 Vietnam time.
	charterNow = vnAt(2026, 11, 10, 9, 0)

	// Da Nang airport -> Hoi An: 30,240 m (30.3 km billed) and 2,700 s (45
	// min) by the fake route.
	airport = listing.CharterPlace{Address: "Sân bay Đà Nẵng", Lat: 16.0544, Lng: 108.2022, ProvinceCode: "48"}
	hoiAn   = listing.CharterPlace{Address: "Phố cổ Hội An", Lat: 15.8801, Lng: 108.3380}

	airportPt = maps.Point{Lat: 16.0544, Lng: 108.2022}
	hoiAnPt   = maps.Point{Lat: 15.8801, Lng: 108.3380}
)

// charterEnv is rentalEnv (whose approved rental listing RL is used as a
// listing of the wrong type) with the clock moved to charterNow, commission
// 1000 bps on private_charter, and a charter partner with the approved "Xe
// bao chuyến Đà Nẵng": service areas 48 and 46, max 300 km, night window
// 22:00-05:00, policy [{48,10000},{24,5000}], classes Xe 7 chỗ (200.000 +
// 12.000/km; night 100.000, weekend 10%, holiday 30%, round-trip waiting
// 150.000) and Xe 16 chỗ (400.000 + 18.000/km, no surcharge), and the fake
// airport -> Hoi An route.
type charterEnv struct {
	*rentalEnv
	CP      partner.Partner
	CL      listing.Listing
	C7, C16 listing.CharterClass
}

func newCharterEnv(t *testing.T) *charterEnv {
	t.Helper()
	ctx := context.Background()
	r := newRentalEnv(t)
	r.Clock.T = charterNow
	c := &charterEnv{rentalEnv: r}
	_, err := r.Catalog.CreateCommissionRate(ctx, r.Admin, charterType, 1000, nil)
	require.NoError(t, err)
	c.CP, err = r.Partner.Create(ctx, r.Admin, partner.CreateInput{
		LegalName: "Công ty Xe Bao Chuyến Đà Nẵng", DisplayName: "Xe Bao Chuyến Đà Nẵng",
		OwnerEmail: "owner@xebaochuyen.vn", OwnerPassword: "matkhau123",
		ServiceTypes: []string{charterType},
	})
	require.NoError(t, err)
	p := c.CP
	l := c.charterListing(t, "Xe bao chuyến Đà Nẵng")
	_, err = r.Listing.SetCharterService(ctx, p.ID, p.OwnerUserID, l.ID, listing.CharterServiceInput{
		ProvinceCodes: []string{"48", "46"}, MaxDistanceKm: ptrTo(300.0),
		NightStart: ptrTo("22:00"), NightEnd: ptrTo("05:00"),
	})
	require.NoError(t, err)
	c.C16 = c.charterClass(t, l.ID, listing.CharterClassInput{Name: "Xe 16 chỗ", Seats: 16, BaseFare: 400_000, PricePerKm: 18_000})
	c.C7 = c.charterClass(t, l.ID, listing.CharterClassInput{Name: "Xe 7 chỗ", Seats: 7, BaseFare: 200_000, PricePerKm: 12_000})
	_, err = r.Listing.SetCharterSurcharges(ctx, p.ID, p.OwnerUserID, l.ID, c.C7.ID, []listing.SurchargeInput{
		{Kind: listing.SurchargeNight, Calc: listing.CalcFixed, Amount: 100_000, Active: true},
		{Kind: listing.SurchargeWeekend, Calc: listing.CalcPercent, Amount: 1000, Active: true},
		{Kind: listing.SurchargeHoliday, Calc: listing.CalcPercent, Amount: 3000, Active: true},
		{Kind: listing.SurchargeRoundTripWaiting, Calc: listing.CalcFixed, Amount: 150_000, Active: true},
	})
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	f, err := r.Files.Upload(ctx, auth.Principal{UserID: p.OwnerUserID, Role: auth.RolePartner}, "listing_image", &buf)
	require.NoError(t, err)
	_, _, err = r.Listing.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{f.ID}, nil)
	require.NoError(t, err)
	_, err = r.Listing.SetCancellationPolicy(ctx, p.ID, p.OwnerUserID, l.ID, []listing.CancellationTier{
		{HoursBefore: 48, RefundBps: 10000}, {HoursBefore: 24, RefundBps: 5000},
	})
	require.NoError(t, err)
	_, err = r.Listing.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	c.CL, err = r.Listing.Approve(ctx, r.Admin, l.ID)
	require.NoError(t, err)
	r.Maps.SetRoute(airportPt, hoiAnPt, maps.Route{DistanceMeters: 30_240, DurationSeconds: 2_700})
	return c
}

func ptrTo[T any](v T) *T { return &v }

// charterListing is a draft charter listing of CP placed in Đà Nẵng (48).
func (c *charterEnv) charterListing(t *testing.T, title string) listing.Listing {
	t.Helper()
	ctx := context.Background()
	p := c.CP
	l, err := c.Listing.Create(ctx, p.ID, p.OwnerUserID, listing.CreateInput{
		ServiceType: charterType, Title: title, Description: ptrTo("Xe bao chuyến đưa đón sân bay"),
	})
	require.NoError(t, err)
	ward := c.wardOf(t, "48")
	l, err = c.Listing.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{
		ProvinceCode: ptrTo("48"), WardCode: &ward, Address: ptrTo("1 Bạch Đằng"),
	})
	require.NoError(t, err)
	return l
}

func (c *charterEnv) charterClass(t *testing.T, listingID uuid.UUID, in listing.CharterClassInput) listing.CharterClass {
	t.Helper()
	cl, err := c.Listing.CreateCharterClass(context.Background(), c.CP.ID, c.CP.OwnerUserID, listingID, in)
	require.NoError(t, err)
	return cl
}

// setPricePerKm is the partner editing a class's price per km.
func (c *charterEnv) setPricePerKm(t *testing.T, class uuid.UUID, price int64) {
	t.Helper()
	_, err := c.Listing.UpdateCharterClass(context.Background(), c.CP.ID, c.CP.OwnerUserID, c.CL.ID, class,
		listing.CharterClassPatch{PricePerKm: &price})
	require.NoError(t, err)
}

// charterInput requests class on CL from the airport to Hoi An.
func (c *charterEnv) charterInput(class uuid.UUID, pickup time.Time, roundTrip bool, ret *time.Time, quoted int64) booking.CreateCharterInput {
	return booking.CreateCharterInput{
		ListingID: c.CL.ID, VehicleClassID: class, Pickup: airport, Dropoff: hoiAn,
		PickupAt: pickup, IsRoundTrip: roundTrip, ReturnAt: ret, QuotedTotal: quoted,
		ContactName: "Trần Thị Bình", ContactPhone: "0912 345 678", ContactEmail: "Binh@Gmail.com",
	}
}

func (c *charterEnv) createCharter(t *testing.T, customer uuid.UUID, key string, in booking.CreateCharterInput) booking.BookingView {
	t.Helper()
	v, err := c.Svc.CreateCharter(context.Background(), customer, key, in)
	require.NoError(t, err)
	return v
}

// charterRow is a booking_charter_details row read back with raw SQL.
type charterRow struct {
	ListingID, VehicleClassID          uuid.UUID
	PickupAt                           time.Time
	PickupAddress, DropoffAddress      string
	PickupLat, PickupLng               float64
	DropoffLat, DropoffLng, DistanceKm float64
	IsRoundTrip                        bool
	ReturnAt                           *time.Time
	Quote                              listing.CharterQuote
	PickupProvinceCode                 string
	DurationMinutes                    int
	ClassName                          string
	Seats                              int
}

func (c *charterEnv) charterDetail(t *testing.T, bookingID uuid.UUID) charterRow {
	t.Helper()
	var (
		r     charterRow
		quote []byte
	)
	require.NoError(t, c.Pool.QueryRow(context.Background(), `
		SELECT listing_id, vehicle_class_id, pickup_at, pickup_address, dropoff_address,
		       pickup_lat::float8, pickup_lng::float8, dropoff_lat::float8, dropoff_lng::float8, distance_km::float8,
		       is_round_trip, return_at, quote, pickup_province_code, duration_minutes, vehicle_class_name, seats
		FROM booking_charter_details WHERE booking_id = $1`, bookingID).Scan(
		&r.ListingID, &r.VehicleClassID, &r.PickupAt, &r.PickupAddress, &r.DropoffAddress,
		&r.PickupLat, &r.PickupLng, &r.DropoffLat, &r.DropoffLng, &r.DistanceKm,
		&r.IsRoundTrip, &r.ReturnAt, &quote, &r.PickupProvinceCode, &r.DurationMinutes, &r.ClassName, &r.Seats))
	require.NoError(t, json.Unmarshal(quote, &r.Quote))
	return r
}

// itemRow is a booking_items row read back with raw SQL, in insertion order.
type itemRow struct {
	Kind, Description string
	Unit              int64
	Quantity          float64
	Total             int64
}

func (c *charterEnv) items(t *testing.T, bookingID uuid.UUID) []itemRow {
	t.Helper()
	rows, err := c.Pool.Query(context.Background(), `
		SELECT line_kind, description, unit_price, quantity::float8, line_total
		FROM booking_items WHERE booking_id = $1 ORDER BY id`, bookingID)
	require.NoError(t, err)
	defer rows.Close()
	var out []itemRow
	for rows.Next() {
		var it itemRow
		require.NoError(t, rows.Scan(&it.Kind, &it.Description, &it.Unit, &it.Quantity, &it.Total))
		out = append(out, it)
	}
	require.NoError(t, rows.Err())
	return out
}
