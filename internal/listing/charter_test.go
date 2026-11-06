package listing_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

const charter = "private_charter"

// charterNow is Tuesday 2026-11-10 09:00 Vietnam time.
var charterNow = time.Date(2026, 11, 10, 9, 0, 0, 0, clock.VN)

// charterEnv is newEnv with the clock at charterNow and a partner selling charters.
func charterEnv(t *testing.T) (*env, partner.Partner) {
	t.Helper()
	e := newEnv(t)
	e.Clock.T = charterNow
	return e, e.partner(t, "Alpha", charter, rental, ride, attraction)
}

// charterListing is a draft charter listing placed in Đà Nẵng (48): the
// operator's address, not a service area.
func (e *env) charterListing(t *testing.T, p partner.Partner, title string) listing.Listing {
	t.Helper()
	ctx := context.Background()
	l, err := e.Svc.Create(ctx, p.ID, p.OwnerUserID, listing.CreateInput{
		ServiceType: charter, Title: title, Description: ptr("Xe bao chuyến"),
	})
	require.NoError(t, err)
	ward := e.wardIn(t, "48")
	l, err = e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{
		ProvinceCode: ptr("48"), WardCode: &ward, Address: ptr("1 Bạch Đằng"),
	})
	require.NoError(t, err)
	return l
}

func serviceIn() listing.CharterServiceInput {
	return listing.CharterServiceInput{
		ProvinceCodes: []string{"48", "46"}, MaxDistanceKm: ptr(300.0),
		NightStart: ptr("22:00"), NightEnd: ptr("05:00"),
	}
}

func class7() listing.CharterClassInput {
	return listing.CharterClassInput{Name: "Xe 7 chỗ", Seats: 7, BaseFare: 200_000, PricePerKm: 12_000}
}

func class16() listing.CharterClassInput {
	return listing.CharterClassInput{Name: "Xe 16 chỗ", Seats: 16, BaseFare: 400_000, PricePerKm: 18_000}
}

// charterShop is "Xe bao chuyến Đà Nẵng" with its service set.
func (e *env) charterShop(t *testing.T, p partner.Partner) listing.Listing {
	t.Helper()
	l := e.charterListing(t, p, "Xe bao chuyến Đà Nẵng")
	_, err := e.Svc.SetCharterService(context.Background(), p.ID, p.OwnerUserID, l.ID, serviceIn())
	require.NoError(t, err)
	return l
}

func (e *env) charterClass(t *testing.T, p partner.Partner, l listing.Listing, in listing.CharterClassInput) listing.CharterClass {
	t.Helper()
	c, err := e.Svc.CreateCharterClass(context.Background(), p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
	return c
}

func TestSetCharterService_UpsertAreasAndAudit(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l := e.charterListing(t, p, "Xe bao chuyến Đà Nẵng")

	in := serviceIn()
	in.ProvinceCodes = []string{"48"}
	st, err := e.Svc.SetCharterService(ctx, p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
	require.Equal(t, l.ID, st.ListingID)
	require.Equal(t, []string{"48"}, st.ProvinceCodes)
	require.NotNil(t, st.MaxDistanceTenths)
	require.Equal(t, int64(3000), *st.MaxDistanceTenths)
	require.Equal(t, "22:00", *st.NightStart)
	require.Equal(t, "05:00", *st.NightEnd)

	// Second call replaces the areas (duplicates dropped) and the settings.
	in = listing.CharterServiceInput{ProvinceCodes: []string{"56", "48", "46", "48"}}
	st, err = e.Svc.SetCharterService(ctx, p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
	require.Equal(t, []string{"46", "48", "56"}, st.ProvinceCodes)
	require.Nil(t, st.MaxDistanceTenths)
	require.Nil(t, st.NightStart)
	require.Nil(t, st.NightEnd)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM charter_services WHERE listing_id = $1`, l.ID))
	require.Equal(t, 3, e.count(t, `SELECT count(*) FROM charter_service_areas WHERE listing_id = $1`, l.ID))
	require.Zero(t, e.count(t, `SELECT count(*) FROM charter_services WHERE max_distance_km IS NOT NULL OR night_start IS NOT NULL`))

	var audits []auditRow
	for _, a := range e.audits(t, l.ID) {
		if a.Action == "charter_service" {
			audits = append(audits, a)
		}
	}
	require.Len(t, audits, 2)
	require.Equal(t, p.OwnerUserID, audits[1].Actor)
	require.Equal(t, "48", audits[1].Changes["province_codes"].Old)
	require.Equal(t, "46,48,56", audits[1].Changes["province_codes"].New)
	require.Contains(t, audits[1].Changes, "night_start")

	// Identical input again writes no audit row.
	_, err = e.Svc.SetCharterService(ctx, p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM listing_audit_log WHERE listing_id = $1 AND action = 'charter_service'`, l.ID))

	got, err := e.Svc.GetCharter(ctx, p.ID, l.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Service)
	require.Equal(t, []string{"46", "48", "56"}, got.Service.ProvinceCodes)
	require.Empty(t, got.Classes)
}

func TestSetCharterService_Validation422(t *testing.T) {
	e, p := charterEnv(t)
	l := e.charterListing(t, p, "Xe bao chuyến Đà Nẵng")
	var tooMany []string
	rows, err := e.Pool.Query(context.Background(), `SELECT code FROM provinces ORDER BY code`)
	require.NoError(t, err)
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		tooMany = append(tooMany, c)
	}
	rows.Close()
	require.Len(t, tooMany, 34)

	for name, in := range map[string]listing.CharterServiceInput{
		"no province":      {},
		"unknown":          {ProvinceCodes: []string{"48", "99"}},
		"blank":            {ProvinceCodes: []string{" "}},
		"two decimals":     {ProvinceCodes: []string{"48"}, MaxDistanceKm: ptr(30.25)},
		"below 1 km":       {ProvinceCodes: []string{"48"}, MaxDistanceKm: ptr(0.9)},
		"above 2000 km":    {ProvinceCodes: []string{"48"}, MaxDistanceKm: ptr(2000.1)},
		"only night_start": {ProvinceCodes: []string{"48"}, NightStart: ptr("22:00")},
		"only night_end":   {ProvinceCodes: []string{"48"}, NightEnd: ptr("05:00")},
		"hour 25":          {ProvinceCodes: []string{"48"}, NightStart: ptr("25:00"), NightEnd: ptr("05:00")},
		"same night":       {ProvinceCodes: []string{"48"}, NightStart: ptr("22:00"), NightEnd: ptr("22:00")},
	} {
		_, err := e.Svc.SetCharterService(context.Background(), p.ID, p.OwnerUserID, l.ID, in)
		requireErr(t, err, http.StatusUnprocessableEntity, "invalid_charter")
		require.NotEmpty(t, name)
	}
	require.Zero(t, e.count(t, `SELECT count(*) FROM charter_services`))

	// 34 provinces and 1 decimal at both bounds are fine.
	_, err = e.Svc.SetCharterService(context.Background(), p.ID, p.OwnerUserID, l.ID,
		listing.CharterServiceInput{ProvinceCodes: tooMany, MaxDistanceKm: ptr(2000.0)})
	require.NoError(t, err)
	st, err := e.Svc.SetCharterService(context.Background(), p.ID, p.OwnerUserID, l.ID,
		listing.CharterServiceInput{ProvinceCodes: []string{"48"}, MaxDistanceKm: ptr(1.0), NightStart: ptr("00:00"), NightEnd: ptr("05:00")})
	require.NoError(t, err)
	require.Equal(t, int64(10), *st.MaxDistanceTenths)
}

func TestSetCharterService_OnRental422(t *testing.T) {
	e, p := charterEnv(t)
	l := e.rentalListing(t, p, "Thuê xe Đà Lạt")
	_, err := e.Svc.SetCharterService(context.Background(), p.ID, p.OwnerUserID, l.ID, serviceIn())
	requireErr(t, err, http.StatusUnprocessableEntity, "wrong_service_type")
	require.Contains(t, err.Error(), "xe bao chuyến")
	require.Zero(t, e.count(t, `SELECT count(*) FROM charter_services`))
}

func TestSuspendedPartnerCannotEditCharterService(t *testing.T) {
	e, p := charterEnv(t)
	l := e.charterListing(t, p, "Xe bao chuyến Đà Nẵng")
	require.NoError(t, e.Partner.Suspend(context.Background(), e.Admin, p.ID, "Vi phạm"))
	_, err := e.Svc.SetCharterService(context.Background(), p.ID, p.OwnerUserID, l.ID, serviceIn())
	requireErr(t, err, http.StatusForbidden, "partner_suspended")
}

func TestSubmit_CharterCompleteness(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l, err := e.Svc.Create(ctx, p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: charter, Title: "Xe bao chuyến Đà Nẵng", Description: ptr("Xe")})
	require.NoError(t, err)
	const head = "listing_incomplete: Listing chưa đủ thông tin để gửi duyệt, còn thiếu: "

	// Nothing yet: the common items and the two charter items.
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_incomplete")
	require.Equal(t, head+"tỉnh/thành và phường/xã; địa chỉ; ít nhất 1 ảnh; khu vực phục vụ (ít nhất 1 tỉnh); ít nhất 1 loại xe đang hoạt động", err.Error())

	ward := e.wardIn(t, "48")
	l, err = e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{ProvinceCode: ptr("48"), WardCode: &ward, Address: ptr("1 Bạch Đằng")})
	require.NoError(t, err)
	_, _, err = e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{e.upload(t, p.OwnerUserID, "listing_image")}, nil)
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.Equal(t, head+"khu vực phục vụ (ít nhất 1 tỉnh); ít nhất 1 loại xe đang hoạt động", err.Error())

	_, err = e.Svc.SetCharterService(ctx, p.ID, p.OwnerUserID, l.ID, serviceIn())
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.Equal(t, head+"ít nhất 1 loại xe đang hoạt động", err.Error())

	// An inactive class does not count.
	c := e.charterClass(t, p, l, class7())
	_, err = e.Svc.UpdateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, listing.CharterClassPatch{Active: ptr(false)})
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.Equal(t, head+"ít nhất 1 loại xe đang hoạt động", err.Error())

	_, err = e.Svc.UpdateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, listing.CharterClassPatch{Active: ptr(true)})
	require.NoError(t, err)
	got, err := e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	require.Equal(t, listing.ReviewPendingReview, got.ReviewStatus)

	d, err := e.Svc.AdminGet(ctx, l.ID)
	require.NoError(t, err)
	require.NotNil(t, d.Charter)
	require.NotNil(t, d.Charter.Service)
	require.Len(t, d.Charter.Classes, 1)
}

func TestPublicDetail_Charter(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l := e.charterShop(t, p)
	c7 := e.charterClass(t, p, l, class7())
	c16 := e.charterClass(t, p, l, class16())
	_, err := e.Svc.SetCharterSurcharges(ctx, p.ID, p.OwnerUserID, l.ID, c7.ID, []listing.SurchargeInput{
		{Kind: listing.SurchargeNight, Calc: listing.CalcFixed, Amount: 100_000, Active: true},
		{Kind: listing.SurchargeWeekend, Calc: listing.CalcPercent, Amount: 1000, Active: true},
		{Kind: listing.SurchargeHoliday, Calc: listing.CalcPercent, Amount: 3000, Active: false},
		{Kind: listing.SurchargeRoundTripWaiting, Calc: listing.CalcFixed, Amount: 0, Active: true},
	})
	require.NoError(t, err)

	// A disabled class never shows.
	off := e.charterClass(t, p, l, listing.CharterClassInput{Name: "Xe 29 chỗ", Seats: 29, BaseFare: 0, PricePerKm: 25_000})
	_, err = e.Svc.UpdateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, off.ID, listing.CharterClassPatch{Active: ptr(false)})
	require.NoError(t, err)

	_, _, err = e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{e.upload(t, p.OwnerUserID, "listing_image")}, nil)
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	_, err = e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)

	pub, err := e.Svc.PublicDetail(ctx, l.Slug)
	require.NoError(t, err)
	require.NotNil(t, pub.Charter)
	require.NotNil(t, pub.Charter.Service)
	require.Equal(t, []string{"46", "48"}, pub.Charter.Service.ProvinceCodes)
	require.Len(t, pub.Charter.Classes, 2)
	require.Equal(t, c7.ID, pub.Charter.Classes[0].ID)
	require.Equal(t, c16.ID, pub.Charter.Classes[1].ID)
	require.Empty(t, pub.Charter.Classes[1].Surcharges)
	var kinds []string
	for _, s := range pub.Charter.Classes[0].Surcharges {
		kinds = append(kinds, s.Kind)
	}
	require.Equal(t, []string{"night", "weekend"}, kinds, "inactive and zero surcharges are hidden")
	require.NotNil(t, pub.PriceFrom)
	require.Equal(t, int64(212_000), *pub.PriceFrom, "cheapest base_fare + price_per_km of the active classes")

	// The admin still sees everything.
	d, err := e.Svc.AdminGet(ctx, l.ID)
	require.NoError(t, err)
	require.Len(t, d.Charter.Classes, 3)
	require.Len(t, d.Charter.Classes[0].Surcharges, 4)
}

func TestHTTP_CharterRoutes_MalformedID404(t *testing.T) {
	e, p := charterEnv(t)
	tok := e.token(t, p.OwnerUserID)
	l := e.charterShop(t, p)
	c := e.charterClass(t, p, l, class7())
	unknown := uuid.Must(uuid.NewV7()).String()
	svc := map[string]any{"province_codes": []string{"48"}}
	cls := map[string]any{"name": "Xe 4 chỗ", "seats": 4, "base_fare": 0, "price_per_km": 10000}
	sur := map[string]any{"surcharges": []any{}}
	base := "/v1/partner/listings/" + l.ID.String()
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/partner/listings/abc/charter", nil},
		{"GET", "/v1/partner/listings/" + unknown + "/charter", nil},
		{"PUT", "/v1/partner/listings/abc/charter-service", svc},
		{"PUT", "/v1/partner/listings/" + unknown + "/charter-service", svc},
		{"POST", "/v1/partner/listings/abc/charter-classes", cls},
		{"POST", "/v1/partner/listings/" + unknown + "/charter-classes", cls},
		{"PATCH", base + "/charter-classes/abc", map[string]any{"seats": 5}},
		{"PATCH", base + "/charter-classes/" + unknown, map[string]any{"seats": 5}},
		{"PATCH", "/v1/partner/listings/abc/charter-classes/" + c.ID.String(), map[string]any{"seats": 5}},
		{"PUT", base + "/charter-classes/abc/surcharges", sur},
		{"PUT", base + "/charter-classes/" + unknown + "/surcharges", sur},
		{"PUT", "/v1/partner/listings/abc/charter-classes/" + c.ID.String() + "/surcharges", sur},
	} {
		requireCode(t, e.do(t, tc.method, tc.path, tok, tc.body), http.StatusNotFound, "not_found")
	}

	rec := e.do(t, "PUT", base+"/charter-service", tok, map[string]any{
		"province_codes": []string{"48", "46"}, "max_distance_km": 300.5, "night_start": "22:00", "night_end": "05:00",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"max_distance_km":300.5`)
	rec = e.do(t, "POST", base+"/charter-classes", tok, cls)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.do(t, "PUT", base+"/charter-classes/"+c.ID.String()+"/surcharges", tok, map[string]any{
		"surcharges": []any{map[string]any{"kind": "night", "calc": "fixed", "amount": 100000}},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"kind":"night"`)
	rec = e.do(t, "GET", base+"/charter", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"service":{`)
	require.Contains(t, rec.Body.String(), `"classes":[{`)

	// A rental is the wrong type.
	r := e.rentalListing(t, p, "Thuê xe")
	requireCode(t, e.do(t, "GET", "/v1/partner/listings/"+r.ID.String()+"/charter", tok, nil), http.StatusUnprocessableEntity, "wrong_service_type")
	requireCode(t, e.do(t, "PUT", base+"/charter-classes/"+c.ID.String()+"/surcharges", tok, `{}`), http.StatusUnprocessableEntity, "invalid_charter")
}

func TestHTTP_CharterService_UnknownField400(t *testing.T) {
	e, p := charterEnv(t)
	tok := e.token(t, p.OwnerUserID)
	l := e.charterShop(t, p)
	c := e.charterClass(t, p, l, class7())
	base := "/v1/partner/listings/" + l.ID.String()
	requireCode(t, e.do(t, "PUT", base+"/charter-service", tok, `{"province_codes":["48"],"listing_id":"x"}`), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "POST", base+"/charter-classes", tok, `{"name":"A","seats":4,"base_fare":0,"price_per_km":1000,"active":false}`), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "PATCH", base+"/charter-classes/"+c.ID.String(), tok, `{"colour":"red"}`), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "PUT", base+"/charter-classes/"+c.ID.String()+"/surcharges", tok, `{"surcharges":[{"kind":"night","calc":"fixed","amount":1,"colour":"red"}]}`), http.StatusBadRequest, "invalid_request")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM charter_vehicle_classes`))
}

func TestHTTP_CharterClass_OtherListing404(t *testing.T) {
	e, p := charterEnv(t)
	tok := e.token(t, p.OwnerUserID)
	l := e.charterShop(t, p)
	c := e.charterClass(t, p, l, class7())
	other := e.charterShop(t, p)
	base := "/v1/partner/listings/" + other.ID.String() + "/charter-classes/" + c.ID.String()
	requireCode(t, e.do(t, "PATCH", base, tok, map[string]any{"seats": 5}), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "PUT", base+"/surcharges", tok, map[string]any{"surcharges": []any{}}), http.StatusNotFound, "not_found")
	require.Equal(t, 7, e.count(t, `SELECT seats FROM charter_vehicle_classes WHERE id = $1`, c.ID))
}
