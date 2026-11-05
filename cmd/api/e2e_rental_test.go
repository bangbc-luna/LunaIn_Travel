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

const e2eRentalOwnerEmail, e2eRentalOwnerPass = "owner@thuexe.vn", "matkhau123"

// searchRental returns the public rental vehicle cards in Lâm Đồng (68) over
// [pickup, ret).
func (a *e2eApp) searchRental(t *testing.T, pickup, ret time.Time) []listing.RentalVehicleCard {
	t.Helper()
	q := url.Values{"province": {"68"}, "pickup_at": {pickup.Format(time.RFC3339)}, "return_at": {ret.Format(time.RFC3339)}}
	var res struct {
		Items []listing.RentalVehicleCard `json:"items"`
	}
	a.call(t, "GET", "/v1/rental-vehicles?"+q.Encode(), "", nil, http.StatusOK, &res)
	return res.Items
}

// requestRental sends a rental request of model at point over [pickup, ret)
// as the customer with Idempotency-Key key.
func (a *e2eApp) requestRental(t *testing.T, ct, key string, l listing.Listing, model, point uuid.UUID, pickup, ret time.Time) booking.BookingView {
	t.Helper()
	var v booking.BookingView
	a.call(t, "POST", "/v1/bookings/rental", ct, map[string]any{
		"listing_id": l.ID, "vehicle_model_id": model, "pickup_point_id": point,
		"pickup_at": pickup, "return_at": ret, "destination_province_code": "68",
		"contact_name": "Lê Văn Cường", "contact_phone": "0901234567", "contact_email": e2eCustomerEmail,
	}, http.StatusCreated, &v, "Idempotency-Key", key)
	return v
}

// TestE2E_RentalRequestToReturn runs a self-drive rental through the real mux
// with payment.Fake: the partner builds a rental shop, the customer requests
// the last Vios (held from request time), the partner accepts, one Fake
// payment confirms it, the vehicle is handed over by QR, nobody marks the
// return and FinishRentals completes it a day after the scheduled return; the
// money is released. A second rental is paid but never handed over and
// becomes no_show at its scheduled return.
func TestE2E_RentalRequestToReturn(t *testing.T) {
	// Friday 2026-11-06 09:00 in Vietnam.
	a := newE2EApp(t, time.Date(2026, 11, 6, 9, 0, 0, 0, clock.VN))
	ctx := context.Background()
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 11, day, hour, minute, 0, 0, clock.VN) }

	// 1. The admin sets the self_drive_rental commission (10%) and creates a
	// partner offering self-drive rentals.
	admin := a.login(t, e2eAdminEmail, e2eAdminPass)
	a.call(t, "POST", "/v1/admin/commission-rates", admin,
		map[string]any{"service_type": "self_drive_rental", "rate_bps": 1000}, http.StatusCreated, nil)
	var p partner.Partner
	a.call(t, "POST", "/v1/admin/partners", admin, map[string]any{
		"legal_name": "Công ty Thuê Xe Đà Lạt", "display_name": "Thuê Xe Đà Lạt",
		"owner_email": e2eRentalOwnerEmail, "owner_password": e2eRentalOwnerPass,
		"service_types": []string{"self_drive_rental"},
	}, http.StatusCreated, &p)
	pt := a.login(t, e2eRentalOwnerEmail, e2eRentalOwnerPass)

	// 2. The partner builds the rental listing in Lâm Đồng (68): store open
	// 06:00-22:00, Vios 800.000/day (1 vehicle), pickup points P0 (free) and
	// P1 (100.000), an image; submitted, then approved by the admin.
	var ward string
	require.NoError(t, a.pool.QueryRow(ctx, `SELECT code FROM wards WHERE province_code = '68' ORDER BY code LIMIT 1`).Scan(&ward))
	var l listing.Listing
	a.call(t, "POST", "/v1/partner/listings", pt, map[string]any{
		"service_type": "self_drive_rental", "title": "Thuê xe Đà Lạt", "description": "Xe tự lái đời mới",
	}, http.StatusCreated, &l)
	lp := "/v1/partner/listings/" + l.ID.String()
	a.call(t, "PATCH", lp, pt, map[string]any{
		"address": "1 Hoàng Văn Thụ", "province_code": "68", "ward_code": ward,
	}, http.StatusOK, nil)
	a.call(t, "PUT", lp+"/rental-store", pt, map[string]any{
		"handover_open": "06:00", "handover_close": "22:00", "deposit_terms": "Đặt cọc 5.000.000 hoặc xe máy",
	}, http.StatusOK, nil)
	var vios listing.RentalModel
	a.call(t, "POST", lp+"/rental-models", pt, map[string]any{
		"vehicle_kind": "car", "name": "Toyota Vios", "model_year": 2023, "seats": 5,
		"price_per_day": 800_000, "quantity": 1,
	}, http.StatusCreated, &vios)
	var p0, p1 listing.PickupPoint
	a.call(t, "POST", lp+"/rental-pickup-points", pt, map[string]any{
		"name": "Cửa hàng", "address": "1 Hoàng Văn Thụ", "province_code": "68", "ward_code": ward, "delivery_fee": 0,
	}, http.StatusCreated, &p0)
	a.call(t, "POST", lp+"/rental-pickup-points", pt, map[string]any{
		"name": "Sân bay Liên Khương", "address": "Sân bay Liên Khương, Đức Trọng", "province_code": "68",
		"ward_code": ward, "delivery_fee": 100_000,
	}, http.StatusCreated, &p1)
	img := a.uploadPNG(t, pt, "listing_image")
	a.call(t, "PUT", lp+"/images", pt, map[string]any{"file_ids": []uuid.UUID{img}}, http.StatusOK, nil)
	a.call(t, "POST", lp+"/submit", pt, nil, http.StatusOK, nil)
	a.call(t, "POST", "/v1/admin/listings/"+l.ID.String()+"/approve", admin, nil, http.StatusOK, nil)

	// 3. A customer registers, finds the Vios (1 left, 3 days) and requests
	// it at P1 over [07/11 08:00, 09/11 10:00): 3 × 800.000 + 100.000.
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

	pickup, ret := at(7, 8, 0), at(9, 10, 0)
	cards := a.searchRental(t, pickup, ret)
	require.Len(t, cards, 1)
	require.Equal(t, vios.ID, cards[0].ModelID)
	require.Equal(t, 1, cards[0].UnitsLeft)
	require.Equal(t, 3, cards[0].Days)

	const gross, net = int64(2_500_000), int64(2_250_000)
	r1 := a.requestRental(t, ct, "e2e-rental-1", l, vios.ID, p1.ID, pickup, ret)
	require.Equal(t, booking.StatusRequested, r1.Status)
	require.Equal(t, gross, r1.GrossAmount)
	require.Nil(t, r1.Payment)
	require.NotNil(t, r1.Rental)
	require.Equal(t, 3, r1.Rental.Days)
	require.True(t, r1.ServiceStartAt.Equal(pickup), r1.ServiceStartAt)
	require.True(t, r1.ServiceEndAt.Equal(ret), r1.ServiceEndAt)
	// The vehicle is held from request time: the search no longer offers it.
	require.Empty(t, a.searchRental(t, pickup, ret))

	// 4. At 10:00 the partner accepts; the customer has until 12:00 to pay.
	a.clock.T = at(6, 10, 0)
	pt = a.login(t, e2eRentalOwnerEmail, e2eRentalOwnerPass)
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	a.call(t, "POST", "/v1/partner/bookings/"+r1.ID.String()+"/accept", pt, nil, http.StatusOK, nil)
	a.call(t, "GET", "/v1/bookings/"+r1.ID.String(), ct, nil, http.StatusOK, &r1)
	require.Equal(t, booking.StatusAwaitingPayment, r1.Status)
	require.NotNil(t, r1.Request)
	require.NotNil(t, r1.Request.PaymentDueAt)
	require.True(t, r1.Request.PaymentDueAt.Equal(at(6, 12, 0)), r1.Request.PaymentDueAt)

	// 5. One Fake payment confirms it; the partner's pending is the net.
	a.call(t, "POST", "/v1/dev/payments/"+orderCode(t, r1)+"/simulate-paid", "", map[string]any{}, http.StatusOK, nil)
	a.call(t, "GET", "/v1/bookings/"+r1.ID.String(), ct, nil, http.StatusOK, &r1)
	require.Equal(t, booking.StatusConfirmed, r1.Status)
	require.NotNil(t, r1.QRToken)
	require.Equal(t, ledger.Balance{Pending: net}, a.balance(t, pt))

	// 6. At 07/11 07:30 the partner hands the vehicle over by QR; nobody
	// marks the return, so FinishRentals completes it at return + 24h.
	a.clock.T = at(7, 7, 30)
	pt = a.login(t, e2eRentalOwnerEmail, e2eRentalOwnerPass)
	var res booking.CheckInResult
	a.call(t, "POST", "/v1/partner/check-ins", pt, map[string]string{"qr_token": *r1.QRToken}, http.StatusOK, &res)
	require.Equal(t, booking.StatusInService, res.Booking.Status)
	a.clock.T = at(10, 9, 59)
	require.NoError(t, a.finish.Work(ctx, nil))
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	a.call(t, "GET", "/v1/bookings/"+r1.ID.String(), ct, nil, http.StatusOK, &r1)
	require.Equal(t, booking.StatusInService, r1.Status)
	a.clock.T = at(10, 10, 0)
	require.NoError(t, a.finish.Work(ctx, nil))
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	a.call(t, "GET", "/v1/bookings/"+r1.ID.String(), ct, nil, http.StatusOK, &r1)
	require.Equal(t, booking.StatusCompleted, r1.Status)

	// 7. A day later (release_delay_days = 1) the money is released and the
	// ledger balances.
	a.clock.T = at(11, 10, 0)
	require.NoError(t, a.release.Work(ctx, nil))
	pt = a.login(t, e2eRentalOwnerEmail, e2eRentalOwnerPass)
	require.Equal(t, ledger.Balance{Available: net}, a.balance(t, pt))
	require.NoError(t, a.ledger.Verify(ctx, p.ID))

	// 8. A second rental at P0 over [12/11 08:00, 13/11 08:00) is accepted
	// and paid but never handed over: at the scheduled return FinishRentals
	// marks it no_show, and its money is released a day later.
	const gross2, net2 = int64(800_000), int64(720_000)
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	r2 := a.requestRental(t, ct, "e2e-rental-2", l, vios.ID, p0.ID, at(12, 8, 0), at(13, 8, 0))
	require.Equal(t, booking.StatusRequested, r2.Status)
	require.Equal(t, gross2, r2.GrossAmount)
	a.call(t, "POST", "/v1/partner/bookings/"+r2.ID.String()+"/accept", pt, nil, http.StatusOK, nil)
	a.call(t, "GET", "/v1/bookings/"+r2.ID.String(), ct, nil, http.StatusOK, &r2)
	a.call(t, "POST", "/v1/dev/payments/"+orderCode(t, r2)+"/simulate-paid", "", map[string]any{}, http.StatusOK, nil)
	a.call(t, "GET", "/v1/bookings/"+r2.ID.String(), ct, nil, http.StatusOK, &r2)
	require.Equal(t, booking.StatusConfirmed, r2.Status)
	require.Equal(t, ledger.Balance{Available: net, Pending: net2}, a.balance(t, pt))

	a.clock.T = at(13, 7, 59)
	require.NoError(t, a.finish.Work(ctx, nil))
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	a.call(t, "GET", "/v1/bookings/"+r2.ID.String(), ct, nil, http.StatusOK, &r2)
	require.Equal(t, booking.StatusConfirmed, r2.Status)
	a.clock.T = at(13, 8, 0)
	require.NoError(t, a.finish.Work(ctx, nil))
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	a.call(t, "GET", "/v1/bookings/"+r2.ID.String(), ct, nil, http.StatusOK, &r2)
	require.Equal(t, booking.StatusNoShow, r2.Status)

	a.clock.T = at(14, 8, 0)
	require.NoError(t, a.release.Work(ctx, nil))
	pt = a.login(t, e2eRentalOwnerEmail, e2eRentalOwnerPass)
	require.Equal(t, ledger.Balance{Available: net + net2}, a.balance(t, pt))
	require.NoError(t, a.ledger.Verify(ctx, p.ID))
	var platform int64
	require.NoError(t, a.pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::bigint FROM platform_commission_entries`).Scan(&platform))
	require.Equal(t, gross-net+gross2-net2, platform)
	var holding int
	require.NoError(t, a.pool.QueryRow(ctx, `SELECT count(*) FROM booking_rental_details WHERE holds_vehicle`).Scan(&holding))
	require.Zero(t, holding)
}
