package listing_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
)

func TestRentalDays(t *testing.T) {
	p := vn(11, 7, 8)
	for name, c := range map[string]struct {
		d    time.Duration
		days int
	}{
		"24h":   {24 * time.Hour, 1},
		"24h01": {24*time.Hour + time.Minute, 2},
		"1h":    {time.Hour, 1},
		"48h":   {48 * time.Hour, 2},
		"50h":   {50 * time.Hour, 3},
	} {
		require.Equal(t, c.days, listing.RentalDays(p, p.Add(c.d)), name)
	}
}

func TestCheckRentalPeriod(t *testing.T) {
	now := rentalNow
	check := func(pickup, ret time.Time) (int, error) { return listing.CheckRentalPeriod(pickup, ret, now, 2) }

	_, err := check(vn(11, 5, 12).Add(-time.Minute), vn(11, 6, 12))
	requireErr(t, err, http.StatusUnprocessableEntity, "pickup_too_soon")
	days, err := check(vn(11, 5, 12), vn(11, 6, 12))
	require.NoError(t, err)
	require.Equal(t, 1, days)

	pickup := vn(11, 6, 8)
	days, err = check(pickup, pickup.Add(30*24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 30, days)
	_, err = check(pickup, pickup.Add(30*24*time.Hour+time.Minute))
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_rental_period")

	_, err = check(pickup.Add(30*time.Second), pickup.Add(48*time.Hour))
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_rental_period")
	_, err = check(pickup, pickup.Add(48*time.Hour+time.Second))
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_rental_period")
	_, err = check(pickup, pickup)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_rental_period")
	_, err = check(pickup, pickup.Add(-time.Hour))
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_rental_period")

	// A return centuries away must not saturate into 1 day.
	far := time.Date(9999, 1, 1, 8, 0, 0, 0, time.UTC)
	_, err = check(pickup, far)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_rental_period")
	require.Greater(t, listing.RentalDays(pickup, far), 30)

	limit := now.AddDate(0, 0, 180)
	_, err = check(limit, limit.Add(24*time.Hour))
	require.NoError(t, err)
	_, err = check(limit.Add(time.Minute), limit.Add(24*time.Hour))
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_rental_period")
}

func viosInput() listing.RentalModelInput {
	return listing.RentalModelInput{
		VehicleKind: "car", Name: "Toyota Vios", ModelYear: 2023, Seats: 5,
		Tags: []string{"Số tự động"}, PricePerDay: 800_000, Quantity: 1,
	}
}

// publishRental submits and approves a rental shop that already has its
// store, models and pickup point.
func (e *env) publishRental(t *testing.T, p partner.Partner, l listing.Listing) listing.Listing {
	t.Helper()
	ctx := context.Background()
	_, _, err := e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{e.upload(t, p.OwnerUserID, "listing_image")}, nil)
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	out, err := e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	return out
}

type rentalFixture struct {
	P            partner.Partner
	L            listing.Listing
	Vios, Vision listing.RentalModel
	Point        listing.PickupPoint
}

// approvedRentalShop is a visible shop (open 06:00-22:00, delivery fee 50.000)
// with a Vios (1 car) and a Vision (2 motorbikes).
func (e *env) approvedRentalShop(t *testing.T, p partner.Partner, title string) rentalFixture {
	t.Helper()
	l := e.dalatLocation(t, p, e.rentalListing(t, p, title))
	_, err := e.Svc.SetRentalStore(context.Background(), p.ID, p.OwnerUserID, l.ID, storeIn("06:00", "22:00"))
	require.NoError(t, err)
	f := rentalFixture{P: p}
	f.Point = e.pickupPoint(t, p, l, "Cửa hàng chính", 50_000)
	f.Vios = e.rentalModel(t, p, l, viosInput())
	f.Vision = e.rentalModel(t, p, l, visionInput())
	f.L = e.publishRental(t, p, l)
	return f
}

func (e *env) searchRental(t *testing.T, f listing.RentalSearchFilter) []listing.RentalVehicleCard {
	t.Helper()
	cards, err := e.Svc.SearchRentalVehicles(context.Background(), f)
	require.NoError(t, err)
	return cards
}

func dalatSearch(pickup, ret time.Time) listing.RentalSearchFilter {
	return listing.RentalSearchFilter{ProvinceCode: "68", PickupAt: pickup, ReturnAt: ret}
}

func modelIDs(cards []listing.RentalVehicleCard) []uuid.UUID {
	out := []uuid.UUID{}
	for _, c := range cards {
		out = append(out, c.ModelID)
	}
	return out
}

func TestSearchRental_UnitsLeftByOverlap(t *testing.T) {
	e, p := rentalEnv(t)
	f := e.approvedRentalShop(t, p, "Thuê xe Đà Lạt")
	pickup, ret := vn(11, 7, 8), vn(11, 8, 8)
	q := dalatSearch(pickup, ret)

	// Back-to-back holds ([.., pickup) and [return, ..)) take nothing.
	for _, m := range []listing.RentalModel{f.Vios, f.Vision} {
		e.hold(t, f.L, m, "confirmed", vn(11, 6, 8), pickup, nil)
		e.hold(t, f.L, m, "requested", ret, vn(11, 9, 8), nil)
	}
	cards := e.searchRental(t, q)
	require.Equal(t, []uuid.UUID{f.Vision.ID, f.Vios.ID}, modelIDs(cards))
	require.Equal(t, 2, cards[0].UnitsLeft)
	require.Equal(t, 1, cards[1].UnitsLeft)

	// One overlapping hold: Vision has one left, the single Vios is gone.
	e.hold(t, f.L, f.Vision, "requested", vn(11, 7, 10), vn(11, 7, 12), nil)
	e.hold(t, f.L, f.Vios, "awaiting_payment", vn(11, 7, 10), vn(11, 7, 12), nil)
	cards = e.searchRental(t, q)
	require.Equal(t, []uuid.UUID{f.Vision.ID}, modelIDs(cards))
	require.Equal(t, 1, cards[0].UnitsLeft)
}

// Two sequential holds inside the window never overlap, so a quantity-2 model
// has one vehicle left (peak 1), not zero (raw count 2).
func TestSearchRental_PeakNotRawCount(t *testing.T) {
	e, p := rentalEnv(t)
	f := e.approvedRentalShop(t, p, "Thuê xe Đà Lạt")
	e.hold(t, f.L, f.Vision, "confirmed", vn(11, 9, 8), vn(11, 10, 8), nil)  // Mon-Tue
	e.hold(t, f.L, f.Vision, "confirmed", vn(11, 11, 8), vn(11, 12, 8), nil) // Wed-Thu
	cards := e.searchRental(t, dalatSearch(vn(11, 9, 8), vn(11, 12, 8)))
	require.Equal(t, []uuid.UUID{f.Vision.ID, f.Vios.ID}, modelIDs(cards))
	require.Equal(t, 1, cards[0].UnitsLeft)
}

// A pending hold whose hold_until has passed is not counted.
func TestSearchRental_ExpiredHoldIgnored(t *testing.T) {
	e, p := rentalEnv(t)
	f := e.approvedRentalShop(t, p, "Thuê xe Đà Lạt")
	past := e.Clock.Now().Add(-time.Minute)
	e.hold(t, f.L, f.Vios, "requested", vn(11, 7, 9), vn(11, 7, 12), &past)
	cards := e.searchRental(t, dalatSearch(vn(11, 7, 8), vn(11, 8, 8)))
	require.Equal(t, []uuid.UUID{f.Vision.ID, f.Vios.ID}, modelIDs(cards))
	live := e.Clock.Now().Add(time.Hour)
	e.hold(t, f.L, f.Vios, "requested", vn(11, 7, 9), vn(11, 7, 12), &live)
	require.Equal(t, []uuid.UUID{f.Vision.ID}, modelIDs(e.searchRental(t, dalatSearch(vn(11, 7, 8), vn(11, 8, 8)))))
}

func TestSearchRental_PriceAndDays(t *testing.T) {
	e, p := rentalEnv(t)
	f := e.approvedRentalShop(t, p, "Thuê xe Đà Lạt")
	cards := e.searchRental(t, dalatSearch(vn(11, 7, 8), vn(11, 9, 10)))
	require.Equal(t, []uuid.UUID{f.Vision.ID, f.Vios.ID}, modelIDs(cards))
	vision, vios := cards[0], cards[1]
	require.Equal(t, 3, vios.Days)
	require.Equal(t, int64(800_000), vios.PricePerDay)
	require.Equal(t, int64(2_400_000), vios.RentalTotal)
	require.Equal(t, int64(450_000), vision.RentalTotal)
	require.Equal(t, 3, vision.Days)
	require.Equal(t, f.L.ID, vios.Listing.ID)
	require.Equal(t, f.L.Slug, vios.Listing.Slug)
	require.Equal(t, "Thuê xe Đà Lạt", vios.Listing.Title)
	require.Equal(t, "car", vios.VehicleKind)
	require.Equal(t, "Toyota Vios", vios.Name)
	require.Equal(t, 2023, vios.ModelYear)
	require.Equal(t, 5, vios.Seats)
	require.Equal(t, []string{"Số tự động"}, vios.Tags)
	require.Equal(t, "1 Hoàng Văn Thụ", vios.StoreAddress)
	require.Equal(t, "06:00", vios.HandoverOpen)
	require.Equal(t, "22:00", vios.HandoverClose)
}

// Review Focus 5: UTC input is compared in Vietnam time.
func TestSearchRental_HandoverHours(t *testing.T) {
	e, p := rentalEnv(t)
	e.approvedRentalShop(t, p, "Thuê xe Đà Lạt")
	ret := time.Date(2026, 11, 8, 3, 0, 0, 0, time.UTC) // 10:00 VN

	at2200 := time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC)
	require.Len(t, e.searchRental(t, dalatSearch(at2200, ret.Add(24*time.Hour))), 2)
	at2230 := time.Date(2026, 11, 7, 15, 30, 0, 0, time.UTC)
	require.Empty(t, e.searchRental(t, dalatSearch(at2230, ret.Add(24*time.Hour))))
	// The return time is checked too.
	require.Empty(t, e.searchRental(t, dalatSearch(vn(11, 7, 8), at2230.Add(24*time.Hour))))
	require.Empty(t, e.searchRental(t, dalatSearch(time.Date(2026, 11, 6, 22, 59, 0, 0, time.UTC), ret.Add(24*time.Hour))), "05:59 VN is before opening")
	require.Len(t, e.searchRental(t, dalatSearch(time.Date(2026, 11, 6, 23, 0, 0, 0, time.UTC), ret.Add(24*time.Hour))), 2, "06:00 VN")
}

func TestSearchRental_HidesInvisibleAndInactive(t *testing.T) {
	e, alpha := rentalEnv(t)
	ctx := context.Background()
	q := dalatSearch(vn(11, 7, 8), vn(11, 8, 8))

	ok := e.approvedRentalShop(t, alpha, "Cửa hàng tốt")
	suspended := e.approvedRentalShop(t, e.partner(t, "Beta", rental), "Cửa hàng đình chỉ")
	disabled := e.approvedRentalShop(t, e.partner(t, "Gamma", rental), "Cửa hàng tắt loại")
	hidden := e.approvedRentalShop(t, e.partner(t, "Delta", rental), "Cửa hàng bị ẩn")
	require.Len(t, e.searchRental(t, q), 8)

	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, suspended.P.ID, "Vi phạm"))
	require.NoError(t, e.Partner.DisableServiceType(ctx, e.Admin, disabled.P.ID, rental))
	_, err := e.Svc.Hide(ctx, e.Admin, hidden.L.ID, "Vi phạm")
	require.NoError(t, err)
	require.ElementsMatch(t, []uuid.UUID{ok.Vios.ID, ok.Vision.ID}, modelIDs(e.searchRental(t, q)))

	// An inactive model or one with quantity 0 is not offered.
	_, err = e.Svc.UpdateRentalModel(ctx, ok.P.ID, ok.P.OwnerUserID, ok.L.ID, ok.Vios.ID, listing.RentalModelPatch{Active: ptr(false)})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{ok.Vision.ID}, modelIDs(e.searchRental(t, q)))
	_, err = e.Svc.UpdateRentalModel(ctx, ok.P.ID, ok.P.OwnerUserID, ok.L.ID, ok.Vision.ID, listing.RentalModelPatch{Quantity: ptr(0)})
	require.NoError(t, err)
	require.Empty(t, e.searchRental(t, q))
}

// A listing whose pickup points are all inactive cannot be booked, so it is
// neither found by the vehicle search nor shown with a price.
func TestSearchRental_AllPickupPointsInactiveHidden(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	f := e.approvedRentalShop(t, p, "Thuê xe Đà Lạt")
	q := dalatSearch(vn(11, 7, 8), vn(11, 8, 8))
	require.Len(t, e.searchRental(t, q), 2)
	require.Len(t, e.search(t, listing.SearchFilter{ServiceType: ptr(rental)}), 1)

	_, err := e.Svc.UpdatePickupPoint(ctx, p.ID, p.OwnerUserID, f.L.ID, f.Point.ID, listing.PickupPointPatch{Active: ptr(false)})
	require.NoError(t, err)
	require.Empty(t, e.searchRental(t, q))
	require.Empty(t, e.search(t, listing.SearchFilter{ServiceType: ptr(rental)}))
	d, err := e.Svc.PublicDetail(ctx, f.L.Slug)
	require.NoError(t, err)
	require.Nil(t, d.PriceFrom)
	require.Empty(t, d.Rental.PickupPoints)

	_, err = e.Svc.UpdatePickupPoint(ctx, p.ID, p.OwnerUserID, f.L.ID, f.Point.ID, listing.PickupPointPatch{Active: ptr(true)})
	require.NoError(t, err)
	require.Len(t, e.searchRental(t, q), 2)
}

func TestSearchRental_ProvinceFilterAndPaging(t *testing.T) {
	e, p := rentalEnv(t)
	e.approvedRentalShop(t, p, "Thuê xe Đà Lạt")
	q := dalatSearch(vn(11, 7, 8), vn(11, 8, 8))
	other := q
	other.ProvinceCode = "79"
	require.Empty(t, e.searchRental(t, other))
	q.Limit = 1
	require.Len(t, e.searchRental(t, q), 1)
	q.Offset = 1
	cards := e.searchRental(t, q)
	require.Len(t, cards, 1)
	require.Equal(t, "Toyota Vios", cards[0].Name)
	q.Offset = 2
	require.Empty(t, e.searchRental(t, q))
}

func TestSearchRental_KindAndSeatsFilter(t *testing.T) {
	e, p := rentalEnv(t)
	f := e.approvedRentalShop(t, p, "Thuê xe Đà Lạt")
	q := dalatSearch(vn(11, 7, 8), vn(11, 8, 8))
	q.VehicleKind = ptr("car")
	require.Equal(t, []uuid.UUID{f.Vios.ID}, modelIDs(e.searchRental(t, q)))
	q.VehicleKind = ptr("motorbike")
	require.Equal(t, []uuid.UUID{f.Vision.ID}, modelIDs(e.searchRental(t, q)))
	q.VehicleKind = nil
	q.MinSeats = 3
	require.Equal(t, []uuid.UUID{f.Vios.ID}, modelIDs(e.searchRental(t, q)))
	q.MinSeats = 50
	require.Empty(t, e.searchRental(t, q))
}

func TestSearchRental_BadParams400(t *testing.T) {
	e, p := rentalEnv(t)
	e.approvedRentalShop(t, p, "Thuê xe Đà Lạt")
	pickup, ret := vn(11, 7, 8), vn(11, 8, 8)
	bad := map[string]func(*listing.RentalSearchFilter){
		"no province":     func(f *listing.RentalSearchFilter) { f.ProvinceCode = " " },
		"return = pickup": func(f *listing.RentalSearchFilter) { f.ReturnAt = f.PickupAt },
		"return < pickup": func(f *listing.RentalSearchFilter) { f.ReturnAt = f.PickupAt.Add(-time.Hour) },
		"truck":           func(f *listing.RentalSearchFilter) { f.VehicleKind = ptr("truck") },
		"seats -1":        func(f *listing.RentalSearchFilter) { f.MinSeats = -1 },
		"seats 51":        func(f *listing.RentalSearchFilter) { f.MinSeats = 51 },
		"limit -1":        func(f *listing.RentalSearchFilter) { f.Limit = -1 },
		"offset 1001":     func(f *listing.RentalSearchFilter) { f.Offset = 1001 },
		"year 9999":       func(f *listing.RentalSearchFilter) { f.ReturnAt = time.Date(9999, 1, 1, 8, 0, 0, 0, time.UTC) },
		"31 days":         func(f *listing.RentalSearchFilter) { f.ReturnAt = f.PickupAt.Add(31 * 24 * time.Hour) },
		"seconds":         func(f *listing.RentalSearchFilter) { f.PickupAt = f.PickupAt.Add(time.Second) },
		"pickup too soon": func(f *listing.RentalSearchFilter) {
			f.PickupAt = rentalNow.Add(time.Hour)
			f.ReturnAt = f.PickupAt.Add(24 * time.Hour)
		},
		"pickup > 180 days": func(f *listing.RentalSearchFilter) {
			f.PickupAt = rentalNow.AddDate(0, 0, 181)
			f.ReturnAt = f.PickupAt.Add(24 * time.Hour)
		},
	}
	for name, mut := range bad {
		f := dalatSearch(pickup, ret)
		mut(&f)
		_, err := e.Svc.SearchRentalVehicles(context.Background(), f)
		require.Error(t, err, name)
		requireErr(t, err, http.StatusBadRequest, "invalid_request")
	}
}

func TestHTTP_RentalVehicles(t *testing.T) {
	e, p := rentalEnv(t)
	f := e.approvedRentalShop(t, p, "Thuê xe Đà Lạt")
	qs := func(kv ...string) string {
		v := url.Values{}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return "/v1/rental-vehicles?" + v.Encode()
	}
	rec := e.do(t, "GET", qs("province", "68", "pickup_at", "2026-11-07T08:00:00+07:00", "return_at", "2026-11-09T10:00:00+07:00", "vehicle_kind", "car", "min_seats", "4", "limit", "5"), "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	page := decode[struct {
		Items []map[string]any `json:"items"`
	}](t, rec.Body.Bytes())
	require.Len(t, page.Items, 1)
	it := page.Items[0]
	require.Equal(t, f.Vios.ID.String(), it["model_id"])
	require.Equal(t, float64(2_400_000), it["rental_total"])
	require.Equal(t, float64(3), it["days"])
	require.Equal(t, float64(1), it["units_left"])
	require.Equal(t, float64(800_000), it["price_per_day"])

	for name, path := range map[string]string{
		"no province": qs("pickup_at", "2026-11-07T08:00:00Z", "return_at", "2026-11-08T08:00:00Z"),
		"bad time":    qs("province", "68", "pickup_at", "07/11/2026", "return_at", "2026-11-08T08:00:00Z"),
		"no return":   qs("province", "68", "pickup_at", "2026-11-07T08:00:00Z"),
		"bad kind":    qs("province", "68", "pickup_at", "2026-11-07T08:00:00Z", "return_at", "2026-11-08T08:00:00Z", "vehicle_kind", "truck"),
		"bad seats":   qs("province", "68", "pickup_at", "2026-11-07T08:00:00Z", "return_at", "2026-11-08T08:00:00Z", "min_seats", "x"),
	} {
		rec := e.do(t, "GET", path, "", nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, name)
	}
}

func TestPublicDetail_Rental(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	f := e.approvedRentalShop(t, p, "Thuê xe Đà Lạt")
	off := e.pickupPoint(t, p, f.L, "Điểm đã đóng", 0)
	_, err := e.Svc.UpdatePickupPoint(ctx, p.ID, p.OwnerUserID, f.L.ID, off.ID, listing.PickupPointPatch{Active: ptr(false)})
	require.NoError(t, err)
	in := viosInput()
	in.Name = "Xe ngừng cho thuê"
	gone := e.rentalModel(t, p, f.L, in)
	_, err = e.Svc.UpdateRentalModel(ctx, p.ID, p.OwnerUserID, f.L.ID, gone.ID, listing.RentalModelPatch{Active: ptr(false)})
	require.NoError(t, err)

	d, err := e.Svc.PublicDetail(ctx, f.L.Slug)
	require.NoError(t, err)
	require.NotNil(t, d.Rental)
	require.NotNil(t, d.PriceFrom)
	require.Equal(t, int64(150_000), *d.PriceFrom)
	require.Equal(t, "22:00", d.Rental.Store.HandoverClose)
	require.Equal(t, "Cọc 5.000.000đ", *d.Rental.Store.DepositTerms)
	require.Len(t, d.Rental.Models, 2)
	require.Len(t, d.Rental.PickupPoints, 1)
	require.Equal(t, int64(50_000), d.Rental.PickupPoints[0].DeliveryFee)
	require.Nil(t, d.Attraction)
	require.Nil(t, d.Ride)

	rec := e.do(t, "GET", "/v1/listings/"+f.L.Slug, "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestSearch_RentalPriceFrom(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	f := e.approvedRentalShop(t, p, "Thuê xe Đà Lạt")
	// A cheaper model that is sold out or inactive does not count.
	in := visionInput()
	in.Name, in.PricePerDay, in.Quantity = "Xe hết", 50_000, 0
	e.rentalModel(t, p, f.L, in)
	in.Name, in.PricePerDay, in.Quantity = "Xe tắt", 40_000, 1
	off := e.rentalModel(t, p, f.L, in)
	_, err := e.Svc.UpdateRentalModel(ctx, p.ID, p.OwnerUserID, f.L.ID, off.ID, listing.RentalModelPatch{Active: ptr(false)})
	require.NoError(t, err)

	cards := e.search(t, listing.SearchFilter{ServiceType: ptr(rental)})
	require.Len(t, cards, 1)
	require.NotNil(t, cards[0].PriceFrom)
	require.Equal(t, int64(150_000), *cards[0].PriceFrom)
}
