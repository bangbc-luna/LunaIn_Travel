package listing_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

const rental = "self_drive_rental"

// rentalNow is Thursday 2026-11-05 09:00 Vietnam time.
var rentalNow = time.Date(2026, 11, 5, 9, 0, 0, 0, clock.VN)

// rentalEnv is newEnv with the clock at rentalNow and a partner selling rentals.
func rentalEnv(t *testing.T) (*env, partner.Partner) {
	t.Helper()
	e := newEnv(t)
	e.Clock.T = rentalNow
	return e, e.partner(t, "Alpha", rental, ride, attraction)
}

func (e *env) rentalListing(t *testing.T, p partner.Partner, title string) listing.Listing {
	t.Helper()
	l, err := e.Svc.Create(context.Background(), p.ID, p.OwnerUserID, listing.CreateInput{
		ServiceType: rental, Title: title, Description: ptr("Xe tự lái"),
	})
	require.NoError(t, err)
	return l
}

// dalatLocation puts the listing (the store address) in Lâm Đồng (68).
func (e *env) dalatLocation(t *testing.T, p partner.Partner, l listing.Listing) listing.Listing {
	t.Helper()
	ward := e.wardIn(t, "68")
	got, err := e.Svc.Update(context.Background(), p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{
		ProvinceCode: ptr("68"), WardCode: &ward, Address: ptr("1 Hoàng Văn Thụ"),
	})
	require.NoError(t, err)
	return got
}

func storeIn(open, close string) listing.RentalStoreInput {
	return listing.RentalStoreInput{HandoverOpen: open, HandoverClose: close, DeliveryAvailable: true, DepositTerms: ptr("Cọc 5.000.000đ")}
}

// rentalShop is a rental listing "Thuê xe Đà Lạt" in Lâm Đồng with a store open 06:00-22:00.
func (e *env) rentalShop(t *testing.T, p partner.Partner) listing.Listing {
	t.Helper()
	l := e.dalatLocation(t, p, e.rentalListing(t, p, "Thuê xe Đà Lạt"))
	_, err := e.Svc.SetRentalStore(context.Background(), p.ID, p.OwnerUserID, l.ID, storeIn("06:00", "22:00"))
	require.NoError(t, err)
	return l
}

func (e *env) pickupPoint(t *testing.T, p partner.Partner, l listing.Listing, name string, fee int64) listing.PickupPoint {
	t.Helper()
	pp, err := e.Svc.CreatePickupPoint(context.Background(), p.ID, p.OwnerUserID, l.ID, e.pointInput(t, name, fee))
	require.NoError(t, err)
	return pp
}

func (e *env) pointInput(t *testing.T, name string, fee int64) listing.PickupPointInput {
	t.Helper()
	return listing.PickupPointInput{Name: name, Address: "2 Trần Phú", ProvinceCode: "68", WardCode: e.wardIn(t, "68"), DeliveryFee: fee}
}

func TestSetRentalStore_UpsertAndAudit(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	l := e.dalatLocation(t, p, e.rentalListing(t, p, "Thuê xe Đà Lạt"))

	st, err := e.Svc.SetRentalStore(ctx, p.ID, p.OwnerUserID, l.ID, storeIn("06:00", "22:00"))
	require.NoError(t, err)
	require.Equal(t, l.ID, st.ListingID)
	require.Equal(t, "22:00", st.HandoverClose)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM rental_stores WHERE listing_id = $1`, l.ID))

	in := storeIn("06:00", "23:00")
	in.DepositTerms = ptr("   ")
	in.LateFeeTerms = ptr(" 100.000đ/giờ ")
	st, err = e.Svc.SetRentalStore(ctx, p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
	require.Equal(t, "23:00", st.HandoverClose)
	require.Nil(t, st.DepositTerms)
	require.Equal(t, "100.000đ/giờ", *st.LateFeeTerms)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM rental_stores WHERE listing_id = $1`, l.ID))

	var audits []auditRow
	for _, a := range e.audits(t, l.ID) {
		if a.Action == "rental_store" {
			audits = append(audits, a)
		}
	}
	require.Len(t, audits, 2)
	require.Equal(t, p.OwnerUserID, audits[1].Actor)
	require.Contains(t, audits[1].Changes, "handover_close")
	require.Equal(t, "22:00", audits[1].Changes["handover_close"].Old)
	require.Equal(t, "23:00", audits[1].Changes["handover_close"].New)
	require.NotContains(t, audits[1].Changes, "handover_open")

	// Identical input again writes no audit row.
	_, err = e.Svc.SetRentalStore(ctx, p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM listing_audit_log WHERE listing_id = $1 AND action = 'rental_store'`, l.ID))

	got, err := e.Svc.GetRental(ctx, p.ID, l.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Store)
	require.Equal(t, "23:00", got.Store.HandoverClose)
	require.Empty(t, got.PickupPoints)
}

func TestSetRentalStore_BadHours422(t *testing.T) {
	e, p := rentalEnv(t)
	l := e.rentalListing(t, p, "Thuê xe Đà Lạt")
	long := strings.Repeat("a", 2001)
	for name, in := range map[string]listing.RentalStoreInput{
		"hour 25":       storeIn("06:00", "25:00"),
		"bad format":    storeIn("6:00", "22:00"),
		"close = open":  storeIn("06:00", "06:00"),
		"close < open":  storeIn("22:00", "06:00"),
		"term too long": {HandoverOpen: "06:00", HandoverClose: "22:00", OtherTerms: &long},
	} {
		_, err := e.Svc.SetRentalStore(context.Background(), p.ID, p.OwnerUserID, l.ID, in)
		requireErr(t, err, http.StatusUnprocessableEntity, "invalid_rental")
		require.NotEmpty(t, name)
	}
	require.Zero(t, e.count(t, `SELECT count(*) FROM rental_stores`))
}

func TestSetRentalStore_OnRide422(t *testing.T) {
	e, p := rentalEnv(t)
	l := e.rideListing(t, p, "Sài Gòn - Đà Lạt")
	_, err := e.Svc.SetRentalStore(context.Background(), p.ID, p.OwnerUserID, l.ID, storeIn("06:00", "22:00"))
	requireErr(t, err, http.StatusUnprocessableEntity, "wrong_service_type")
	require.Contains(t, err.Error(), "thuê xe tự lái")
}

func TestSetRentalStore_SuspendedPartner403(t *testing.T) {
	e, p := rentalEnv(t)
	l := e.rentalListing(t, p, "Thuê xe Đà Lạt")
	require.NoError(t, e.Partner.Suspend(context.Background(), e.Admin, p.ID, "Vi phạm"))
	_, err := e.Svc.SetRentalStore(context.Background(), p.ID, p.OwnerUserID, l.ID, storeIn("06:00", "22:00"))
	requireErr(t, err, http.StatusForbidden, "partner_suspended")
}

func TestRentalStore_CoversHandover(t *testing.T) {
	st := listing.RentalStore{RentalStoreInput: storeIn("06:00", "22:00")}
	for name, c := range map[string]struct {
		at   time.Time
		want bool
	}{
		"22:00 VN as UTC":       {time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC), true},
		"22:01 VN":              {time.Date(2026, 11, 7, 15, 1, 0, 0, time.UTC), false},
		"06:00 VN":              {time.Date(2026, 11, 6, 6, 0, 0, 0, clock.VN), true},
		"05:59 VN":              {time.Date(2026, 11, 6, 5, 59, 0, 0, clock.VN), false},
		"noon":                  {time.Date(2026, 11, 6, 12, 0, 0, 0, clock.VN), true},
		"22:00 UTC is 05:00 VN": {time.Date(2026, 11, 6, 22, 0, 0, 0, time.UTC), false},
	} {
		require.Equal(t, c.want, st.CoversHandover(c.at), name)
	}
}

func TestCreatePickupPoint_NeedsStore422(t *testing.T) {
	e, p := rentalEnv(t)
	l := e.dalatLocation(t, p, e.rentalListing(t, p, "Thuê xe Đà Lạt"))
	_, err := e.Svc.CreatePickupPoint(context.Background(), p.ID, p.OwnerUserID, l.ID, e.pointInput(t, "Sân bay", 0))
	requireErr(t, err, http.StatusUnprocessableEntity, "rental_store_required")
	require.Contains(t, err.Error(), "Cần khai báo thông tin cửa hàng trước")
}

func TestCreatePickupPoint_WardNotInProvince422(t *testing.T) {
	e, p := rentalEnv(t)
	l := e.rentalShop(t, p)
	in := e.pointInput(t, "Sân bay", 0)
	in.WardCode = e.wardIn(t, "79")
	_, err := e.Svc.CreatePickupPoint(context.Background(), p.ID, p.OwnerUserID, l.ID, in)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_location")
	require.Zero(t, e.count(t, `SELECT count(*) FROM rental_pickup_points`))
}

func TestCreatePickupPoint_CreateAndValidate(t *testing.T) {
	e, p := rentalEnv(t)
	l := e.rentalShop(t, p)
	pp := e.pickupPoint(t, p, l, "  Sân bay Liên Khương ", 150_000)
	require.Equal(t, "Sân bay Liên Khương", pp.Name)
	require.Equal(t, int64(150_000), pp.DeliveryFee)
	require.True(t, pp.Active)
	require.Equal(t, l.ID, pp.ListingID)
	created := 0
	for _, a := range e.audits(t, l.ID) {
		if a.Action == "rental_pickup_point" {
			created++
			require.Equal(t, pp.ID.String(), a.Changes["id"].New)
		}
	}
	require.Equal(t, 1, created)

	for name, mut := range map[string]func(*listing.PickupPointInput){
		"empty name":    func(in *listing.PickupPointInput) { in.Name = "  " },
		"long name":     func(in *listing.PickupPointInput) { in.Name = strings.Repeat("a", 201) },
		"empty address": func(in *listing.PickupPointInput) { in.Address = "" },
		"long address":  func(in *listing.PickupPointInput) { in.Address = strings.Repeat("a", 501) },
	} {
		in := e.pointInput(t, "Điểm", 0)
		mut(&in)
		_, err := e.Svc.CreatePickupPoint(context.Background(), p.ID, p.OwnerUserID, l.ID, in)
		requireErr(t, err, http.StatusUnprocessableEntity, "invalid_rental")
		require.NotEmpty(t, name)
	}
}

func TestCreatePickupPoint_FeeBounds(t *testing.T) {
	e, p := rentalEnv(t)
	l := e.rentalShop(t, p)
	for _, fee := range []int64{-1, 5_000_001} {
		_, err := e.Svc.CreatePickupPoint(context.Background(), p.ID, p.OwnerUserID, l.ID, e.pointInput(t, "Sân bay", fee))
		requireErr(t, err, http.StatusUnprocessableEntity, "invalid_rental")
	}
	for _, fee := range []int64{0, 5_000_000} {
		e.pickupPoint(t, p, l, "Điểm", fee)
	}
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM rental_pickup_points`))
}

func TestUpdatePickupPoint_LocationImmutable422(t *testing.T) {
	e, p := rentalEnv(t)
	l := e.rentalShop(t, p)
	pp := e.pickupPoint(t, p, l, "Sân bay", 100_000)
	tok := e.token(t, p.OwnerUserID)
	path := "/v1/partner/listings/" + l.ID.String() + "/rental-pickup-points/" + pp.ID.String()
	requireCode(t, e.do(t, "PATCH", path, tok, map[string]any{"province_code": "79"}), http.StatusUnprocessableEntity, "invalid_rental")
	requireCode(t, e.do(t, "PATCH", path, tok, map[string]any{"ward_code": "x", "name": "Mới"}), http.StatusUnprocessableEntity, "invalid_rental")
	var name, prov string
	require.NoError(t, e.Pool.QueryRow(context.Background(), `SELECT name, province_code FROM rental_pickup_points WHERE id = $1`, pp.ID).Scan(&name, &prov))
	require.Equal(t, "Sân bay", name)
	require.Equal(t, "68", prov)
}

func TestUpdatePickupPoint_DeactivateOK(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	l := e.rentalShop(t, p)
	pp := e.pickupPoint(t, p, l, "Sân bay", 100_000)

	got, err := e.Svc.UpdatePickupPoint(ctx, p.ID, p.OwnerUserID, l.ID, pp.ID, listing.PickupPointPatch{
		Name: ptr("Sân bay Liên Khương"), DeliveryFee: ptr(int64(120_000)), Active: ptr(false),
	})
	require.NoError(t, err)
	require.Equal(t, "Sân bay Liên Khương", got.Name)
	require.Equal(t, int64(120_000), got.DeliveryFee)
	require.False(t, got.Active)
	require.Equal(t, "2 Trần Phú", got.Address)

	audits := e.audits(t, l.ID)
	a := audits[len(audits)-1]
	require.Equal(t, "rental_pickup_point", a.Action)
	require.Contains(t, a.Changes, "active")
	require.Contains(t, a.Changes, "delivery_fee")

	d, err := e.Svc.GetRental(ctx, p.ID, l.ID)
	require.NoError(t, err)
	require.Len(t, d.PickupPoints, 1) // the partner still sees inactive points

	_, err = e.Svc.UpdatePickupPoint(ctx, p.ID, p.OwnerUserID, l.ID, pp.ID, listing.PickupPointPatch{DeliveryFee: ptr(int64(-5))})
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_rental")
	_, err = e.Svc.UpdatePickupPoint(ctx, p.ID, p.OwnerUserID, l.ID, uuid.Must(uuid.NewV7()), listing.PickupPointPatch{Active: ptr(true)})
	requireErr(t, err, http.StatusNotFound, "not_found")
}

func TestPickupPointForBooking_InactiveOrOtherListing404(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	l := e.rentalShop(t, p)
	other := e.dalatLocation(t, p, e.rentalListing(t, p, "Thuê xe Nha Trang"))
	_, err := e.Svc.SetRentalStore(ctx, p.ID, p.OwnerUserID, other.ID, storeIn("07:00", "20:00"))
	require.NoError(t, err)
	pp := e.pickupPoint(t, p, l, "Sân bay", 100_000)

	get := func(listingID, pointID uuid.UUID) (listing.PickupPoint, error) {
		tx, err := e.Pool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx)
		return e.Svc.PickupPointForBooking(ctx, tx, listingID, pointID)
	}
	got, err := get(l.ID, pp.ID)
	require.NoError(t, err)
	require.Equal(t, pp.ID, got.ID)
	require.Equal(t, int64(100_000), got.DeliveryFee)

	_, err = get(other.ID, pp.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = get(l.ID, uuid.Must(uuid.NewV7()))
	requireErr(t, err, http.StatusNotFound, "not_found")

	_, err = e.Svc.UpdatePickupPoint(ctx, p.ID, p.OwnerUserID, l.ID, pp.ID, listing.PickupPointPatch{Active: ptr(false)})
	require.NoError(t, err)
	_, err = get(l.ID, pp.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")

	// RentalStoreOf for the booking.
	tx, err := e.Pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	st, err := e.Svc.RentalStoreOf(ctx, tx, l.ID)
	require.NoError(t, err)
	require.Equal(t, "22:00", st.HandoverClose)
	_, err = e.Svc.RentalStoreOf(ctx, tx, uuid.Must(uuid.NewV7()))
	requireErr(t, err, http.StatusUnprocessableEntity, "rental_store_required")
}

func TestSubmit_RentalCompleteness(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	l := e.rentalListing(t, p, "Thuê xe Đà Lạt")
	const head = "listing_incomplete: Listing chưa đủ thông tin để gửi duyệt, còn thiếu: "

	// Nothing yet: the common items and the three rental items.
	_, err := e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_incomplete")
	require.Equal(t, head+"tỉnh/thành và phường/xã; địa chỉ; ít nhất 1 ảnh; thông tin cửa hàng (giờ giao nhận); ít nhất 1 mẫu xe đang cho thuê; ít nhất 1 điểm nhận xe đang hoạt động", err.Error())

	l = e.dalatLocation(t, p, l)
	_, _, err = e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{e.upload(t, p.OwnerUserID, "listing_image")}, nil)
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.Equal(t, head+"thông tin cửa hàng (giờ giao nhận); ít nhất 1 mẫu xe đang cho thuê; ít nhất 1 điểm nhận xe đang hoạt động", err.Error())

	_, err = e.Svc.SetRentalStore(ctx, p.ID, p.OwnerUserID, l.ID, storeIn("06:00", "22:00"))
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.Equal(t, head+"ít nhất 1 mẫu xe đang cho thuê; ít nhất 1 điểm nhận xe đang hoạt động", err.Error())

	// A model with quantity 0 does not count.
	modelID := uuid.Must(uuid.NewV7())
	_, err = e.Pool.Exec(ctx, `INSERT INTO rental_vehicle_models (id, listing_id, vehicle_kind, name, model_year, seats, price_per_day, quantity)
		VALUES ($1, $2, 'car', 'Vios', 2023, 5, 800000, 0)`, modelID, l.ID)
	require.NoError(t, err)
	pp := e.pickupPoint(t, p, l, "Sân bay", 0)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.Equal(t, head+"ít nhất 1 mẫu xe đang cho thuê", err.Error())

	// An inactive model does not count either.
	_, err = e.Pool.Exec(ctx, `UPDATE rental_vehicle_models SET quantity = 2, active = false WHERE id = $1`, modelID)
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.Equal(t, head+"ít nhất 1 mẫu xe đang cho thuê", err.Error())
	_, err = e.Pool.Exec(ctx, `UPDATE rental_vehicle_models SET active = true WHERE id = $1`, modelID)
	require.NoError(t, err)

	// Deactivating the only pickup point brings that item back.
	_, err = e.Svc.UpdatePickupPoint(ctx, p.ID, p.OwnerUserID, l.ID, pp.ID, listing.PickupPointPatch{Active: ptr(false)})
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.Equal(t, head+"ít nhất 1 điểm nhận xe đang hoạt động", err.Error())
	_, err = e.Svc.UpdatePickupPoint(ctx, p.ID, p.OwnerUserID, l.ID, pp.ID, listing.PickupPointPatch{Active: ptr(true)})
	require.NoError(t, err)

	got, err := e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	require.Equal(t, listing.ReviewPendingReview, got.ReviewStatus)

	// The admin sees the store and points; approval re-checks.
	d, err := e.Svc.AdminGet(ctx, l.ID)
	require.NoError(t, err)
	require.NotNil(t, d.Rental)
	require.NotNil(t, d.Rental.Store)
	require.Len(t, d.Rental.PickupPoints, 1)
	_, err = e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	pub, err := e.Svc.PublicDetail(ctx, l.Slug)
	require.NoError(t, err)
	require.NotNil(t, pub.Rental)
	require.Equal(t, "22:00", pub.Rental.Store.HandoverClose)
	require.Len(t, pub.Rental.PickupPoints, 1)
}

func TestHTTP_RentalRoutes_MalformedID404(t *testing.T) {
	e, p := rentalEnv(t)
	tok := e.token(t, p.OwnerUserID)
	l := e.rentalShop(t, p)
	unknown := uuid.Must(uuid.NewV7()).String()
	store := map[string]any{"handover_open": "06:00", "handover_close": "22:00", "delivery_available": false}
	point := map[string]any{"name": "Sân bay", "address": "1 A", "province_code": "68", "ward_code": e.wardIn(t, "68"), "delivery_fee": 0}
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/partner/listings/abc/rental"},
		{"GET", "/v1/partner/listings/" + unknown + "/rental"},
		{"PUT", "/v1/partner/listings/abc/rental-store"},
		{"PUT", "/v1/partner/listings/" + unknown + "/rental-store"},
		{"POST", "/v1/partner/listings/abc/rental-pickup-points"},
		{"POST", "/v1/partner/listings/" + unknown + "/rental-pickup-points"},
		{"PATCH", "/v1/partner/listings/" + l.ID.String() + "/rental-pickup-points/abc"},
		{"PATCH", "/v1/partner/listings/" + l.ID.String() + "/rental-pickup-points/" + unknown},
		{"PATCH", "/v1/partner/listings/abc/rental-pickup-points/" + unknown},
	} {
		var body any
		switch c.method {
		case "PUT":
			body = store
		case "POST":
			body = point
		case "PATCH":
			body = map[string]any{"delivery_fee": 1000}
		}
		requireCode(t, e.do(t, c.method, c.path, tok, body), http.StatusNotFound, "not_found")
	}

	base := "/v1/partner/listings/" + l.ID.String()
	rec := e.do(t, "PUT", base+"/rental-store", tok, store)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"handover_close":"22:00"`)
	rec = e.do(t, "POST", base+"/rental-pickup-points", tok, point)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	pp := decode[listing.PickupPoint](t, rec.Body.Bytes())
	rec = e.do(t, "PATCH", base+"/rental-pickup-points/"+pp.ID.String(), tok, map[string]any{"delivery_fee": 30000, "active": false})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"delivery_fee":30000`)
	rec = e.do(t, "GET", base+"/rental", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"pickup_points":[{`)
	require.Contains(t, rec.Body.String(), `"store":{`)

	// An attraction is the wrong type.
	a := e.create(t, p, "Vé A")
	requireCode(t, e.do(t, "GET", "/v1/partner/listings/"+a.ID.String()+"/rental", tok, nil), http.StatusUnprocessableEntity, "wrong_service_type")
}

func TestHTTP_RentalStore_UnknownField400(t *testing.T) {
	e, p := rentalEnv(t)
	tok := e.token(t, p.OwnerUserID)
	l := e.rentalShop(t, p)
	base := "/v1/partner/listings/" + l.ID.String()
	requireCode(t, e.do(t, "PUT", base+"/rental-store", tok,
		`{"handover_open":"06:00","handover_close":"22:00","delivery_available":true,"listing_id":"x"}`), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "POST", base+"/rental-pickup-points", tok,
		`{"name":"A","address":"B","province_code":"68","ward_code":"x","delivery_fee":0,"active":false}`), http.StatusBadRequest, "invalid_request")
	pp := e.pickupPoint(t, p, l, "Sân bay", 0)
	requireCode(t, e.do(t, "PATCH", base+"/rental-pickup-points/"+pp.ID.String(), tok, `{"colour":"red"}`), http.StatusBadRequest, "invalid_request")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM rental_pickup_points`))
}

func TestHTTP_PickupPoint_OtherPartner404(t *testing.T) {
	e, p := rentalEnv(t)
	l := e.rentalShop(t, p)
	pp := e.pickupPoint(t, p, l, "Sân bay", 0)
	q := e.partner(t, "Beta", rental)
	tok := e.token(t, q.OwnerUserID)
	base := "/v1/partner/listings/" + l.ID.String()
	requireCode(t, e.do(t, "GET", base+"/rental", tok, nil), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "PUT", base+"/rental-store", tok, map[string]any{"handover_open": "06:00", "handover_close": "22:00"}), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "POST", base+"/rental-pickup-points", tok,
		map[string]any{"name": "A", "address": "B", "province_code": "68", "ward_code": e.wardIn(t, "68"), "delivery_fee": 0}), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "PATCH", base+"/rental-pickup-points/"+pp.ID.String(), tok, map[string]any{"active": false}), http.StatusNotFound, "not_found")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM rental_pickup_points WHERE active`))
}
