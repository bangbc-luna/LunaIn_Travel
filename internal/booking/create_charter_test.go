package booking_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/maps"
)

// Times below are Vietnam time; charterNow is Tuesday 2026-11-10 09:00.

var (
	wed0800 = vnAt(2026, 11, 11, 8, 0)
	wed2200 = vnAt(2026, 11, 11, 22, 0)
)

func TestCreateCharter_Requested(t *testing.T) {
	c := newCharterEnv(t)
	ctx := context.Background()
	v := c.createCharter(t, c.Customer, "charter-1", c.charterInput(c.C7.ID, wed0800, false, nil, 563_600))

	require.Equal(t, booking.StatusRequested, v.Status)
	require.Equal(t, charterType, v.ServiceType)
	require.Equal(t, "request", c.str(t, `SELECT booking_mode FROM bookings WHERE id = $1`, v.ID))
	require.EqualValues(t, 563_600, v.GrossAmount)
	require.Equal(t, 563_600, c.count(t, `SELECT gross_amount FROM bookings WHERE id = $1`, v.ID))
	require.Equal(t, 56_360, c.count(t, `SELECT commission_amount FROM bookings WHERE id = $1`, v.ID))
	require.Equal(t, 1000, c.count(t, `SELECT commission_rate_bps FROM bookings WHERE id = $1`, v.ID))
	require.Equal(t, 563_600, c.count(t, `SELECT sum(line_total)::int FROM booking_items WHERE booking_id = $1`, v.ID))

	require.Equal(t, []itemRow{
		{Kind: "charter_base", Description: "Giá mở cửa Xe 7 chỗ", Unit: 200_000, Quantity: 1, Total: 200_000},
		{Kind: "charter_distance", Description: "Quãng đường 30,3 km", Unit: 12_000, Quantity: 30.3, Total: 363_600},
	}, c.items(t, v.ID))
	require.Len(t, v.Items, 2)
	require.Equal(t, 30.3, v.Items[1].Quantity)
	require.EqualValues(t, 363_600, v.Items[1].LineTotal)

	require.True(t, wed0800.Equal(v.ServiceStartAt))
	require.True(t, vnAt(2026, 11, 11, 8, 45).Equal(v.ServiceEndAt))
	require.True(t, wed0800.Equal(c.timeOf(t, `SELECT original_service_start_at FROM bookings WHERE id = $1`, v.ID)))
	due := vnAt(2026, 11, 10, 21, 0)
	require.NotNil(t, v.Request)
	require.True(t, due.Equal(*v.Request.ResponseDueAt))
	require.Nil(t, v.Request.PaymentDueAt)
	require.Nil(t, v.HoldExpiresAt)

	d := c.charterDetail(t, v.ID)
	require.Equal(t, c.CL.ID, d.ListingID)
	require.Equal(t, c.C7.ID, d.VehicleClassID)
	require.Equal(t, "Xe 7 chỗ", d.ClassName)
	require.Equal(t, 7, d.Seats)
	require.True(t, wed0800.Equal(d.PickupAt))
	require.Equal(t, "Sân bay Đà Nẵng", d.PickupAddress)
	require.Equal(t, "Phố cổ Hội An", d.DropoffAddress)
	require.Equal(t, 16.0544, d.PickupLat)
	require.Equal(t, 108.2022, d.PickupLng)
	require.Equal(t, 15.8801, d.DropoffLat)
	require.Equal(t, 108.338, d.DropoffLng)
	require.Equal(t, 30.3, d.DistanceKm)
	require.Equal(t, 45, d.DurationMinutes)
	require.Equal(t, "48", d.PickupProvinceCode)
	require.False(t, d.IsRoundTrip)
	require.Nil(t, d.ReturnAt)
	require.Equal(t, int64(563_600), d.Quote.Total)
	require.Equal(t, int64(303), d.Quote.DistanceTenths)
	require.Equal(t, int64(363_600), d.Quote.DistanceTotal)
	require.Equal(t, "fake", d.Quote.Provider)
	require.Empty(t, d.Quote.Surcharges)

	require.NotNil(t, v.Charter)
	require.Equal(t, c.C7.ID, v.Charter.VehicleClassID)
	require.Equal(t, "Xe 7 chỗ", v.Charter.VehicleClassName)
	require.Equal(t, 7, v.Charter.Seats)
	require.Equal(t, 30.3, v.Charter.DistanceKm)
	require.Equal(t, 45, v.Charter.DurationMinutes)
	require.Equal(t, "48", v.Charter.PickupProvinceCode)

	// No payment and no hold: the checkout stays open for the whole gross.
	var checkoutID uuid.UUID
	require.NoError(t, c.Pool.QueryRow(ctx, `SELECT checkout_id FROM bookings WHERE id = $1`, v.ID).Scan(&checkoutID))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM payments`))
	require.Equal(t, "open", c.str(t, `SELECT status FROM checkouts WHERE id = $1`, checkoutID))
	require.Equal(t, 563_600, c.count(t, `SELECT total_amount FROM checkouts WHERE id = $1`, checkoutID))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND to_status = 'requested' AND actor_kind = 'customer' AND actor_user_id = $2`, v.ID, c.Customer))

	// The customer and the partner owner are emailed with the deadline.
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'binh@gmail.com' AND args->>'text' LIKE '%chờ nhà xe xác nhận trước 21:00 ngày 10/11/2026%'`))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'owner@xebaochuyen.vn' AND args->>'subject' LIKE 'Có yêu cầu xe bao chuyến mới%'
		AND args->>'text' LIKE '%Sân bay Đà Nẵng%' AND args->>'text' LIKE '%Phố cổ Hội An%'
		AND args->>'text' LIKE '%Xe 7 chỗ%' AND args->>'text' LIKE '%30,3 km%'
		AND args->>'text' LIKE '%563.600%' AND args->>'text' LIKE '%21:00 ngày 10/11/2026%'`))
}

func TestCreateCharter_RoundTripNightItems(t *testing.T) {
	c := newCharterEnv(t)
	ret := vnAt(2026, 11, 12, 6, 0)
	v := c.createCharter(t, c.Customer, "charter-rt", c.charterInput(c.C7.ID, wed2200, true, &ret, 1_177_200))

	require.EqualValues(t, 1_177_200, v.GrossAmount)
	require.Equal(t, 117_720, c.count(t, `SELECT commission_amount FROM bookings WHERE id = $1`, v.ID))
	require.Equal(t, []itemRow{
		{Kind: "charter_base", Description: "Giá mở cửa Xe 7 chỗ", Unit: 200_000, Quantity: 1, Total: 200_000},
		{Kind: "charter_distance", Description: "Quãng đường khứ hồi 2 × 30,3 km", Unit: 12_000, Quantity: 60.6, Total: 727_200},
		{Kind: "surcharge", Description: "Phụ phí ban đêm", Unit: 100_000, Quantity: 1, Total: 100_000},
		{Kind: "surcharge", Description: "Phụ phí chờ khứ hồi", Unit: 150_000, Quantity: 1, Total: 150_000},
	}, c.items(t, v.ID))
	require.Equal(t, 1_177_200, c.count(t, `SELECT sum(line_total)::int FROM booking_items WHERE booking_id = $1`, v.ID))
	require.True(t, wed2200.Equal(v.ServiceStartAt))
	require.True(t, ret.Equal(v.ServiceEndAt), "a round trip ends at its return")
	require.True(t, vnAt(2026, 11, 10, 21, 0).Equal(*v.Request.ResponseDueAt))

	d := c.charterDetail(t, v.ID)
	require.True(t, d.IsRoundTrip)
	require.NotNil(t, d.ReturnAt)
	require.True(t, ret.Equal(*d.ReturnAt))
	require.Equal(t, 30.3, d.DistanceKm, "the one-way distance is stored")
	require.Equal(t, int64(606), d.Quote.BilledTenths)
	require.Equal(t, []string{"night", "round_trip_waiting"}, []string{d.Quote.Surcharges[0].Kind, d.Quote.Surcharges[1].Kind})
	require.True(t, v.Charter.IsRoundTrip)
	require.True(t, ret.Equal(*v.Charter.ReturnAt))
}

// A weekend pickup with a percent surcharge: the item carries the rate.
func TestCreateCharter_PercentSurchargeItem(t *testing.T) {
	c := newCharterEnv(t)
	sat := vnAt(2026, 11, 14, 8, 0)
	// fare 563.600, weekend 10% = 56.360.
	v := c.createCharter(t, c.Customer, "charter-sat", c.charterInput(c.C7.ID, sat, false, nil, 619_960))
	require.Equal(t, []itemRow{
		{Kind: "charter_base", Description: "Giá mở cửa Xe 7 chỗ", Unit: 200_000, Quantity: 1, Total: 200_000},
		{Kind: "charter_distance", Description: "Quãng đường 30,3 km", Unit: 12_000, Quantity: 30.3, Total: 363_600},
		{Kind: "surcharge", Description: "Phụ phí cuối tuần (10%)", Unit: 56_360, Quantity: 1, Total: 56_360},
	}, c.items(t, v.ID))

	// A holiday on Saturday at 30% takes over the weekend 10%.
	require.NoError(t, c.Settings.AddHoliday(context.Background(), c.Admin, vnAt(2026, 11, 21, 0, 0), "Ngày lễ thử"))
	v = c.createCharter(t, c.Customer, "charter-hol", c.charterInput(c.C7.ID, vnAt(2026, 11, 21, 8, 0), false, nil, 732_680))
	items := c.items(t, v.ID)
	require.Len(t, items, 3)
	require.Equal(t, itemRow{Kind: "surcharge", Description: "Phụ phí ngày lễ (30%)", Unit: 169_080, Quantity: 1, Total: 169_080}, items[2])
	require.EqualValues(t, 732_680, v.GrossAmount)
}

// Review Focus 1: the price changes between the quote and the request.
func TestCreateCharter_QuoteChanged409(t *testing.T) {
	c := newCharterEnv(t)
	in := c.charterInput(c.C7.ID, wed0800, false, nil, 563_600)

	c.setPricePerKm(t, c.C7.ID, 13_000)
	_, err := c.Svc.CreateCharter(context.Background(), c.Customer, "charter-q", in)
	requireErr(t, err, http.StatusConflict, "quote_changed")
	require.Contains(t, err.Error(), "Giá chuyến đi đã thay đổi")
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM checkouts`))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM bookings`))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM booking_charter_details`))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM idempotency_keys`))

	// An admin adds the pickup day as a holiday (30% of the fare): changed again.
	require.NoError(t, c.Settings.AddHoliday(context.Background(), c.Admin, wed0800, "Ngày lễ thử"))
	in.QuotedTotal = 593_900 // 200.000 + 13.000 × 30,3
	_, err = c.Svc.CreateCharter(context.Background(), c.Customer, "charter-q", in)
	requireErr(t, err, http.StatusConflict, "quote_changed")
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM idempotency_keys`))

	// The same key with the new price goes through.
	in.QuotedTotal = 772_070 // 593.900 + 30% = 178.170
	v := c.createCharter(t, c.Customer, "charter-q", in)
	require.EqualValues(t, 772_070, v.GrossAmount)
	require.Equal(t, 772_070, c.count(t, `SELECT sum(line_total)::int FROM booking_items WHERE booking_id = $1`, v.ID))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM bookings`))
}

// Review Focus 1: the booking keeps its snapshot after the partner edits.
func TestCreateCharter_PriceEditAfterRequestKeepsSnapshot(t *testing.T) {
	c := newCharterEnv(t)
	ctx := context.Background()
	v := c.createCharter(t, c.Customer, "charter-s", c.charterInput(c.C7.ID, wed0800, false, nil, 563_600))
	before := c.items(t, v.ID)
	quoteBefore := c.str(t, `SELECT quote::text FROM booking_charter_details WHERE booking_id = $1`, v.ID)

	c.setPricePerKm(t, c.C7.ID, 13_000)
	_, err := c.Listing.UpdateCharterClass(ctx, c.CP.ID, c.CP.OwnerUserID, c.CL.ID, c.C7.ID,
		listing.CharterClassPatch{Name: ptrTo("Xe 7 chỗ đời mới"), BaseFare: ptrTo(int64(250_000))})
	require.NoError(t, err)

	got, err := c.Svc.GetMine(ctx, c.Customer, v.ID)
	require.NoError(t, err)
	require.EqualValues(t, 563_600, got.GrossAmount)
	require.Equal(t, before, c.items(t, v.ID))
	require.Equal(t, quoteBefore, c.str(t, `SELECT quote::text FROM booking_charter_details WHERE booking_id = $1`, v.ID))
	require.Equal(t, "Xe 7 chỗ", got.Charter.VehicleClassName)
	var q listing.CharterQuote
	require.NoError(t, json.Unmarshal(got.Charter.Quote, &q))
	require.Equal(t, int64(563_600), q.Total)
	require.Equal(t, int64(12_000), q.PricePerKm)

	// Accepted at the old price: the payment is the snapshot gross.
	_, err = c.Svc.AcceptRequest(ctx, c.CP.ID, c.CP.OwnerUserID, v.ID)
	require.NoError(t, err)
	require.Equal(t, 563_600, c.count(t, `SELECT p.amount FROM payments p JOIN bookings b ON b.checkout_id = p.checkout_id
		WHERE b.id = $1`, v.ID))
	require.Equal(t, 56_360, c.count(t, `SELECT commission_amount FROM bookings WHERE id = $1`, v.ID))
	require.NoError(t, c.Ledger.Verify(ctx, c.CP.ID))
}

// Review Focus 4: the map is down; nothing is kept, the same key works later.
func TestCreateCharter_MapDown503ThenRetry(t *testing.T) {
	c := newCharterEnv(t)
	in := c.charterInput(c.C7.ID, wed0800, false, nil, 563_600)
	c.Maps.SetErr(maps.ErrUnavailable)
	_, err := c.Svc.CreateCharter(context.Background(), c.Customer, "charter-m", in)
	requireErr(t, err, http.StatusServiceUnavailable, "map_unavailable")
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM idempotency_keys`))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM checkouts`))

	c.Maps.SetErr(maps.ErrNoRoute)
	_, err = c.Svc.CreateCharter(context.Background(), c.Customer, "charter-m", in)
	requireErr(t, err, http.StatusUnprocessableEntity, "no_route")
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM idempotency_keys`))

	c.Maps.SetErr(nil)
	v := c.createCharter(t, c.Customer, "charter-m", in)
	require.Equal(t, booking.StatusRequested, v.Status)
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM bookings`))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM idempotency_keys`))
}

// Review Focus 4: a replay answers the same booking without calling the map.
func TestCreateCharter_ReplayDoesNotCallMap(t *testing.T) {
	c := newCharterEnv(t)
	in := c.charterInput(c.C7.ID, wed0800, false, nil, 563_600)
	v := c.createCharter(t, c.Customer, "charter-r", in)
	calls := c.Maps.Calls()

	again := c.createCharter(t, c.Customer, "charter-r", in)
	require.Equal(t, v.ID, again.ID)
	require.Equal(t, v.Code, again.Code)
	require.Equal(t, calls, c.Maps.Calls(), "a replay does not call the map")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM bookings`))

	other := in
	other.Note = ptrTo("Có 2 vali lớn")
	_, err := c.Svc.CreateCharter(context.Background(), c.Customer, "charter-r", other)
	requireErr(t, err, http.StatusUnprocessableEntity, "idempotency_key_reused")
	require.Equal(t, calls, c.Maps.Calls())

	// Another customer may use the same key.
	c2 := c.customer(t, "khach2@gmail.com")
	w := c.createCharter(t, c2, "charter-r", in)
	require.NotEqual(t, v.ID, w.ID)
}

func TestCreateCharter_Validation(t *testing.T) {
	c := newCharterEnv(t)
	ctx := context.Background()
	in := c.charterInput(c.C7.ID, wed0800, false, nil, 563_600)
	try := func(in booking.CreateCharterInput) error {
		_, err := c.Svc.CreateCharter(ctx, c.Customer, uuid.NewString(), in)
		return err
	}

	soon := in
	soon.PickupAt = vnAt(2026, 11, 10, 11, 59)
	requireErr(t, try(soon), http.StatusUnprocessableEntity, "pickup_too_soon")
	outside := in
	outside.Pickup.ProvinceCode = "79"
	requireErr(t, try(outside), http.StatusUnprocessableEntity, "outside_service_area")
	noQuote := in
	noQuote.QuotedTotal = 0
	requireErr(t, try(noQuote), http.StatusUnprocessableEntity, "invalid_booking")
	badPlace := in
	badPlace.Dropoff = listing.CharterPlace{Address: "Singapore", Lat: 1.3, Lng: 103.8}
	requireErr(t, try(badPlace), http.StatusUnprocessableEntity, "invalid_location")
	oneWayReturn := in
	ret := wed0800.Add(3 * time.Hour)
	oneWayReturn.ReturnAt = &ret
	requireErr(t, try(oneWayReturn), http.StatusUnprocessableEntity, "invalid_charter_time")
	noContact := in
	noContact.ContactName = " "
	requireErr(t, try(noContact), http.StatusUnprocessableEntity, "invalid_booking")

	// A disabled class, another listing's class.
	_, err := c.Listing.UpdateCharterClass(ctx, c.CP.ID, c.CP.OwnerUserID, c.CL.ID, c.C16.ID, listing.CharterClassPatch{Active: ptrTo(false)})
	require.NoError(t, err)
	requireErr(t, try(c.charterInput(c.C16.ID, wed0800, false, nil, 945_400)), http.StatusNotFound, "not_found")
	other := c.charterListing(t, "Xe bao chuyến khác")
	_, err = c.Listing.SetCharterService(ctx, c.CP.ID, c.CP.OwnerUserID, other.ID, listing.CharterServiceInput{ProvinceCodes: []string{"48"}})
	require.NoError(t, err)
	otherClass := c.charterClass(t, other.ID, listing.CharterClassInput{Name: "Xe 4 chỗ", Seats: 4, BaseFare: 100_000, PricePerKm: 10_000})
	requireErr(t, try(c.charterInput(otherClass.ID, wed0800, false, nil, 403_000)), http.StatusNotFound, "not_found")

	// A rental listing is the wrong type.
	rental := in
	rental.ListingID = c.RL.ID
	err = try(rental)
	requireErr(t, err, http.StatusUnprocessableEntity, "wrong_service_type")
	require.ErrorIs(t, err, listing.ErrNotCharterListing)
	var ae *httpx.AppError
	require.True(t, errors.As(err, &ae))
	require.Equal(t, "Thao tác này chỉ áp dụng cho Listing xe bao chuyến", ae.Message)
	require.Equal(t, 0, c.Maps.Calls(), "every check above runs before the map call")

	// Beyond the 300 km maximum (one way): needs the route.
	farAway := listing.CharterPlace{Address: "Hà Nội", Lat: 21.0285, Lng: 105.8542}
	c.Maps.SetRoute(airportPt, maps.Point{Lat: 21.0285, Lng: 105.8542}, maps.Route{DistanceMeters: 300_001, DurationSeconds: 21_600})
	far := in
	far.Dropoff = farAway
	requireErr(t, try(far), http.StatusUnprocessableEntity, "distance_exceeds_max")

	// The partner is suspended.
	require.NoError(t, c.Partner.Suspend(ctx, c.Admin, c.CP.ID, "Vi phạm"))
	requireErr(t, try(in), http.StatusUnprocessableEntity, "listing_not_bookable")

	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM bookings`))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM checkouts`))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM idempotency_keys`))
}

func TestCharterView_RequestedHasNoPaymentNoQR(t *testing.T) {
	c := newCharterEnv(t)
	tok := c.token(t, c.Customer, auth.RoleCustomer)
	body := func() map[string]any {
		return map[string]any{
			"listing_id":       c.CL.ID,
			"vehicle_class_id": c.C7.ID,
			"pickup":           map[string]any{"address": "Sân bay Đà Nẵng", "lat": 16.0544, "lng": 108.2022, "province_code": "48"},
			"dropoff":          map[string]any{"address": "Phố cổ Hội An", "lat": 15.8801, "lng": 108.3380},
			"pickup_at":        "2026-11-11T08:00:00+07:00",
			"is_round_trip":    false,
			"quoted_total":     563_600,
			"contact_name":     "Trần Thị Bình",
			"contact_phone":    "0912 345 678",
			"contact_email":    "binh@gmail.com",
		}
	}

	requireCode(t, c.do(t, "POST", "/v1/bookings/charter", tok, body()), http.StatusBadRequest, "idempotency_key_required")
	unknown := body()
	unknown["coupon"] = "X"
	requireCode(t, c.do(t, "POST", "/v1/bookings/charter", tok, unknown, "Idempotency-Key", "k-unknown"), http.StatusBadRequest, "invalid_request")
	noQuote := body()
	delete(noQuote, "quoted_total")
	requireCode(t, c.do(t, "POST", "/v1/bookings/charter", tok, noQuote, "Idempotency-Key", "k-noquote"), http.StatusUnprocessableEntity, "invalid_booking")
	require.Equal(t, http.StatusUnauthorized, c.do(t, "POST", "/v1/bookings/charter", "", body(), "Idempotency-Key", "k-anon").Code)

	rec := c.do(t, "POST", "/v1/bookings/charter", tok, body(), "Idempotency-Key", "k-1")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.JSONEq(t, `null`, string(raw["payment"]))
	_, hasQR := raw["qr_token"]
	require.False(t, hasQR)
	require.JSONEq(t, `true`, string(raw["can_cancel"]))
	require.JSONEq(t, `false`, string(raw["can_reschedule"]))
	_, hasCommission := raw["commission_amount"]
	require.False(t, hasCommission)

	var v struct {
		ID      uuid.UUID `json:"id"`
		Charter struct {
			VehicleClassID     uuid.UUID       `json:"vehicle_class_id"`
			VehicleClassName   string          `json:"vehicle_class_name"`
			Seats              int             `json:"seats"`
			PickupAt           time.Time       `json:"pickup_at"`
			PickupAddress      string          `json:"pickup_address"`
			PickupLat          float64         `json:"pickup_lat"`
			PickupLng          float64         `json:"pickup_lng"`
			PickupProvinceCode string          `json:"pickup_province_code"`
			DropoffAddress     string          `json:"dropoff_address"`
			DropoffLat         float64         `json:"dropoff_lat"`
			DropoffLng         float64         `json:"dropoff_lng"`
			DistanceKm         float64         `json:"distance_km"`
			DurationMinutes    int             `json:"duration_minutes"`
			IsRoundTrip        bool            `json:"is_round_trip"`
			ReturnAt           *time.Time      `json:"return_at"`
			Quote              json.RawMessage `json:"quote"`
		} `json:"charter"`
		Request struct {
			ResponseDueAt *time.Time `json:"response_due_at"`
		} `json:"request"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	require.Equal(t, 30.3, v.Charter.DistanceKm)
	require.Equal(t, 45, v.Charter.DurationMinutes)
	require.Equal(t, "Xe 7 chỗ", v.Charter.VehicleClassName)
	require.Equal(t, 7, v.Charter.Seats)
	require.True(t, wed0800.Equal(v.Charter.PickupAt))
	require.Equal(t, 16.0544, v.Charter.PickupLat)
	require.Equal(t, 108.338, v.Charter.DropoffLng)
	require.Equal(t, "48", v.Charter.PickupProvinceCode)
	require.Nil(t, v.Charter.ReturnAt)
	var q struct {
		Total      int64   `json:"total"`
		DistanceKm float64 `json:"distance_km"`
	}
	require.NoError(t, json.Unmarshal(v.Charter.Quote, &q))
	require.Equal(t, int64(563_600), q.Total)
	require.Equal(t, 30.3, q.DistanceKm)
	require.NotNil(t, v.Request.ResponseDueAt)
	require.True(t, vnAt(2026, 11, 10, 21, 0).Equal(*v.Request.ResponseDueAt))

	// The same key replays with 201 and the same booking.
	again := c.do(t, "POST", "/v1/bookings/charter", tok, body(), "Idempotency-Key", "k-1")
	require.Equal(t, http.StatusCreated, again.Code)
	require.Equal(t, v.ID, decode[booking.BookingView](t, again).ID)

	// GET shows the same; the partner sees the commission but no QR.
	got := c.do(t, "GET", "/v1/bookings/"+v.ID.String(), tok, nil)
	require.Equal(t, http.StatusOK, got.Code)
	require.Equal(t, 30.3, decode[booking.BookingView](t, got).Charter.DistanceKm)
	pv, err := c.Svc.GetForPartner(context.Background(), c.CP.ID, v.ID)
	require.NoError(t, err)
	require.NotNil(t, pv.Charter)
	require.False(t, pv.CanCancel, "only the customer withdraws a request")
	require.False(t, pv.CanReschedule)
	require.Nil(t, pv.QRToken)
	require.EqualValues(t, 56_360, *pv.CommissionAmount)
}
