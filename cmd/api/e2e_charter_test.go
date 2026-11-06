package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/maps"
)

const e2eCharterOwnerEmail, e2eCharterOwnerPass = "owner@xebaochuyen.vn", "matkhau123"

var (
	// Da Nang airport -> Hoi An, pinned on the fake map to 30,240 m (30.3 km
	// billed) and 2,700 s (45 min).
	e2eAirport = map[string]any{"address": "Sân bay Đà Nẵng", "lat": 16.0544, "lng": 108.2022, "province_code": "48"}
	e2eHoiAn   = map[string]any{"address": "Phố cổ Hội An", "lat": 15.8801, "lng": 108.3380}
)

// quoteCharter asks the public quote of l from the airport to Hoi An, one
// way, at pickup.
func (a *e2eApp) quoteCharter(t *testing.T, l listing.Listing, pickup time.Time) listing.CharterQuoteResult {
	t.Helper()
	var res listing.CharterQuoteResult
	a.call(t, "POST", "/v1/charter-quotes", "", map[string]any{
		"listing_id": l.ID, "pickup": e2eAirport, "dropoff": e2eHoiAn, "pickup_at": pickup,
	}, http.StatusOK, &res)
	return res
}

// requestCharter sends a one-way charter request of class from the airport to
// Hoi An at pickup with the quoted total, as the customer with
// Idempotency-Key key.
func (a *e2eApp) requestCharter(t *testing.T, ct, key string, l listing.Listing, class uuid.UUID, pickup time.Time, quoted int64) booking.BookingView {
	t.Helper()
	var v booking.BookingView
	a.call(t, "POST", "/v1/bookings/charter", ct, map[string]any{
		"listing_id": l.ID, "vehicle_class_id": class, "pickup": e2eAirport, "dropoff": e2eHoiAn,
		"pickup_at": pickup, "quoted_total": quoted,
		"contact_name": "Trần Thị Bình", "contact_phone": "0912345678", "contact_email": e2eCustomerEmail,
	}, http.StatusCreated, &v, "Idempotency-Key", key)
	return v
}

// acceptAndPay is the partner accepting v and one Fake payment confirming it.
func (a *e2eApp) acceptAndPay(t *testing.T, pt, ct string, v booking.BookingView) booking.BookingView {
	t.Helper()
	a.call(t, "POST", "/v1/partner/bookings/"+v.ID.String()+"/accept", pt, nil, http.StatusOK, nil)
	a.call(t, "GET", "/v1/bookings/"+v.ID.String(), ct, nil, http.StatusOK, &v)
	require.Equal(t, booking.StatusAwaitingPayment, v.Status)
	a.call(t, "POST", "/v1/dev/payments/"+orderCode(t, v)+"/simulate-paid", "", map[string]any{}, http.StatusOK, nil)
	a.call(t, "GET", "/v1/bookings/"+v.ID.String(), ct, nil, http.StatusOK, &v)
	require.Equal(t, booking.StatusConfirmed, v.Status)
	return v
}

// TestE2E_CharterRequestToComplete runs a private charter through the real
// mux with payment.Fake and maps.Fake: the partner builds a charter service
// (Xe 7 chỗ 200.000 + 12.000/km with surcharges), the customer quotes the
// airport -> Hoi An trip and requests it with the quoted total, the partner
// accepts, one Fake payment confirms it, the partner picks the customer up by
// QR and completes the trip, and the money is released. A second charter is
// paid but nobody is picked up: FinishCharters marks it no_show a day after
// its end. A third one is cancelled by the customer 36 h ahead (50% tier).
func TestE2E_CharterRequestToComplete(t *testing.T) {
	// Tuesday 2026-11-10 09:00 in Vietnam.
	a := newE2EApp(t, time.Date(2026, 11, 10, 9, 0, 0, 0, clock.VN))
	ctx := context.Background()
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 11, day, hour, minute, 0, 0, clock.VN) }
	a.maps.SetRoute(maps.Point{Lat: 16.0544, Lng: 108.2022}, maps.Point{Lat: 15.8801, Lng: 108.3380},
		maps.Route{DistanceMeters: 30_240, DurationSeconds: 2_700})

	// 1. The admin sets the private_charter commission (10%) and creates a
	// partner offering private charters.
	admin := a.login(t, e2eAdminEmail, e2eAdminPass)
	a.call(t, "POST", "/v1/admin/commission-rates", admin,
		map[string]any{"service_type": "private_charter", "rate_bps": 1000}, http.StatusCreated, nil)
	var p partner.Partner
	a.call(t, "POST", "/v1/admin/partners", admin, map[string]any{
		"legal_name": "Công ty Xe Bao Chuyến Đà Nẵng", "display_name": "Xe Bao Chuyến Đà Nẵng",
		"owner_email": e2eCharterOwnerEmail, "owner_password": e2eCharterOwnerPass,
		"service_types": []string{"private_charter"},
	}, http.StatusCreated, &p)
	pt := a.login(t, e2eCharterOwnerEmail, e2eCharterOwnerPass)

	// 2. The partner builds the charter listing in Đà Nẵng (48): service
	// areas 48 and 46, max 300 km, night 22:00-05:00; Xe 7 chỗ with night,
	// weekend, holiday and round-trip waiting surcharges; an image and a
	// refund policy; submitted, then approved by the admin.
	var ward string
	require.NoError(t, a.pool.QueryRow(ctx, `SELECT code FROM wards WHERE province_code = '48' ORDER BY code LIMIT 1`).Scan(&ward))
	var l listing.Listing
	a.call(t, "POST", "/v1/partner/listings", pt, map[string]any{
		"service_type": "private_charter", "title": "Xe bao chuyến Đà Nẵng", "description": "Xe bao chuyến đưa đón sân bay",
	}, http.StatusCreated, &l)
	lp := "/v1/partner/listings/" + l.ID.String()
	a.call(t, "PATCH", lp, pt, map[string]any{
		"address": "1 Bạch Đằng", "province_code": "48", "ward_code": ward,
	}, http.StatusOK, nil)
	a.call(t, "PUT", lp+"/charter-service", pt, map[string]any{
		"province_codes": []string{"48", "46"}, "max_distance_km": 300, "night_start": "22:00", "night_end": "05:00",
	}, http.StatusOK, nil)
	var c7 listing.CharterClass
	a.call(t, "POST", lp+"/charter-classes", pt, map[string]any{
		"name": "Xe 7 chỗ", "seats": 7, "base_fare": 200_000, "price_per_km": 12_000,
	}, http.StatusCreated, &c7)
	a.call(t, "PUT", lp+"/charter-classes/"+c7.ID.String()+"/surcharges", pt, map[string]any{"surcharges": []map[string]any{
		{"kind": "night", "calc": "fixed", "amount": 100_000, "active": true},
		{"kind": "weekend", "calc": "percent", "amount": 1000, "active": true},
		{"kind": "holiday", "calc": "percent", "amount": 3000, "active": true},
		{"kind": "round_trip_waiting", "calc": "fixed", "amount": 150_000, "active": true},
	}}, http.StatusOK, &c7)
	require.Len(t, c7.Surcharges, 4)
	img := a.uploadPNG(t, pt, "listing_image")
	a.call(t, "PUT", lp+"/images", pt, map[string]any{"file_ids": []uuid.UUID{img}}, http.StatusOK, nil)
	a.call(t, "PUT", lp+"/cancellation-policy", pt, map[string]any{"tiers": []map[string]int{
		{"hours_before": 48, "refund_bps": 10000}, {"hours_before": 24, "refund_bps": 5000},
	}}, http.StatusOK, nil)
	a.call(t, "POST", lp+"/submit", pt, nil, http.StatusOK, nil)
	a.call(t, "POST", "/v1/admin/listings/"+l.ID.String()+"/approve", admin, nil, http.StatusOK, nil)

	// 3. A customer registers, quotes Wednesday 11/11 08:00 (no surcharge):
	// 200.000 + 30,3 × 12.000 = 563.600, and requests it with that total.
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

	const gross, net = int64(563_600), int64(507_240)
	pickup := at(11, 8, 0)
	q := a.quoteCharter(t, l, pickup)
	require.Equal(t, 30.3, q.DistanceKm)
	require.Equal(t, 45, q.DurationMinutes)
	require.Len(t, q.Quotes, 1)
	require.Equal(t, c7.ID, q.Quotes[0].VehicleClassID)
	require.Equal(t, int64(303), q.Quotes[0].DistanceTenths)
	require.Equal(t, int64(363_600), q.Quotes[0].DistanceTotal)
	require.Empty(t, q.Quotes[0].Surcharges)
	require.Equal(t, gross, q.Quotes[0].Total)

	r1 := a.requestCharter(t, ct, "e2e-charter-1", l, c7.ID, pickup, q.Quotes[0].Total)
	require.Equal(t, booking.StatusRequested, r1.Status)
	require.Equal(t, gross, r1.GrossAmount)
	require.Nil(t, r1.Payment)
	require.NotNil(t, r1.Charter)
	require.Equal(t, "Xe 7 chỗ", r1.Charter.VehicleClassName)
	require.Equal(t, 30.3, r1.Charter.DistanceKm)
	require.True(t, r1.ServiceStartAt.Equal(pickup), r1.ServiceStartAt)
	require.True(t, r1.ServiceEndAt.Equal(at(11, 8, 45)), r1.ServiceEndAt)
	require.False(t, r1.CanReschedule)

	// 4. At 10:00 the partner accepts; the customer sees the payment link,
	// due at 12:00.
	a.clock.T = at(10, 10, 0)
	pt = a.login(t, e2eCharterOwnerEmail, e2eCharterOwnerPass)
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	a.call(t, "POST", "/v1/partner/bookings/"+r1.ID.String()+"/accept", pt, nil, http.StatusOK, nil)
	a.call(t, "GET", "/v1/bookings/"+r1.ID.String(), ct, nil, http.StatusOK, &r1)
	require.Equal(t, booking.StatusAwaitingPayment, r1.Status)
	require.NotNil(t, r1.Payment)
	require.NotNil(t, r1.Payment.CheckoutURL)
	require.NotNil(t, r1.Request)
	require.True(t, r1.Request.PaymentDueAt.Equal(at(10, 12, 0)), r1.Request.PaymentDueAt)

	// 5. One Fake payment confirms it; the partner's pending is the net.
	a.call(t, "POST", "/v1/dev/payments/"+orderCode(t, r1)+"/simulate-paid", "", map[string]any{}, http.StatusOK, nil)
	a.call(t, "GET", "/v1/bookings/"+r1.ID.String(), ct, nil, http.StatusOK, &r1)
	require.Equal(t, booking.StatusConfirmed, r1.Status)
	require.NotNil(t, r1.QRToken)
	require.Equal(t, ledger.Balance{Pending: net}, a.balance(t, pt))

	// 6. At 11/11 07:30 the partner picks the customer up by QR; at 09:00 the
	// trip is completed (twice: the second is a no-op).
	a.clock.T = at(11, 7, 30)
	pt = a.login(t, e2eCharterOwnerEmail, e2eCharterOwnerPass)
	var res booking.CheckInResult
	a.call(t, "POST", "/v1/partner/check-ins", pt, map[string]string{"qr_token": *r1.QRToken}, http.StatusOK, &res)
	require.Equal(t, booking.StatusInService, res.Booking.Status)
	a.clock.T = at(11, 9, 0)
	pt = a.login(t, e2eCharterOwnerEmail, e2eCharterOwnerPass)
	var done booking.BookingView
	a.call(t, "POST", "/v1/partner/bookings/"+r1.ID.String()+"/complete", pt, nil, http.StatusOK, &done)
	require.Equal(t, booking.StatusCompleted, done.Status)
	a.call(t, "POST", "/v1/partner/bookings/"+r1.ID.String()+"/complete", pt, nil, http.StatusOK, &done)
	require.Equal(t, booking.StatusCompleted, done.Status)

	// 7. A day later (release_delay_days = 1) the money is released and the
	// ledger balances.
	a.clock.T = at(12, 8, 59)
	require.NoError(t, a.release.Work(ctx, nil))
	pt = a.login(t, e2eCharterOwnerEmail, e2eCharterOwnerPass)
	require.Equal(t, ledger.Balance{Pending: net}, a.balance(t, pt))
	a.clock.T = at(12, 9, 0)
	require.NoError(t, a.release.Work(ctx, nil))
	pt = a.login(t, e2eCharterOwnerEmail, e2eCharterOwnerPass)
	require.Equal(t, ledger.Balance{Available: net}, a.balance(t, pt))
	require.NoError(t, a.ledger.Verify(ctx, p.ID))

	// 8. Two more charters are requested, accepted and paid at 12/11 09:00:
	// R2 on Friday 13/11 08:00 and R3 on Monday 16/11 08:00 (563.600 each).
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	r2 := a.requestCharter(t, ct, "e2e-charter-2", l, c7.ID, at(13, 8, 0), gross)
	r2 = a.acceptAndPay(t, pt, ct, r2)
	r3 := a.requestCharter(t, ct, "e2e-charter-3", l, c7.ID, at(16, 8, 0), gross)
	r3 = a.acceptAndPay(t, pt, ct, r3)
	require.Equal(t, ledger.Balance{Available: net, Pending: 2 * net}, a.balance(t, pt))

	// R2 is never picked up: FinishCharters marks it no_show at its end
	// (08:45) + charter_auto_complete_hours (24), not a minute earlier.
	a.clock.T = at(14, 8, 44)
	require.NoError(t, a.finishCharters.Work(ctx, nil))
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	a.call(t, "GET", "/v1/bookings/"+r2.ID.String(), ct, nil, http.StatusOK, &r2)
	require.Equal(t, booking.StatusConfirmed, r2.Status)
	a.clock.T = at(14, 8, 45)
	require.NoError(t, a.finishCharters.Work(ctx, nil))
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	a.call(t, "GET", "/v1/bookings/"+r2.ID.String(), ct, nil, http.StatusOK, &r2)
	require.Equal(t, booking.StatusNoShow, r2.Status)
	a.call(t, "GET", "/v1/bookings/"+r3.ID.String(), ct, nil, http.StatusOK, &r3)
	require.Equal(t, booking.StatusConfirmed, r3.Status)

	// R3 is cancelled by the customer at 14/11 20:00, 36 h before pickup:
	// tier 24 h..48 h refunds 50% (281.800); the commission (56.360) is kept,
	// so 563.600 − 56.360 − 281.800 = 225.440 is released at once.
	a.clock.T = at(14, 20, 0)
	ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
	a.call(t, "POST", "/v1/bookings/"+r3.ID.String()+"/cancel", ct, map[string]any{"bank_account": map[string]string{
		"bank_bin": "970436", "account_number": "0011001234567", "account_holder": "TRAN THI BINH",
	}}, http.StatusOK, &r3)
	require.Equal(t, booking.StatusCancelled, r3.Status)
	var refunds struct {
		Items []payment.Refund `json:"items"`
	}
	a.call(t, "GET", "/v1/refunds", ct, nil, http.StatusOK, &refunds)
	require.Len(t, refunds.Items, 1)
	require.Equal(t, int64(281_800), refunds.Items[0].Amount)
	require.Equal(t, r3.ID, *refunds.Items[0].BookingID)
	pt = a.login(t, e2eCharterOwnerEmail, e2eCharterOwnerPass)
	require.Equal(t, ledger.Balance{Available: net + 225_440, Pending: net}, a.balance(t, pt))

	// R2's money is released a day after its no-show.
	a.clock.T = at(15, 8, 45)
	require.NoError(t, a.release.Work(ctx, nil))
	pt = a.login(t, e2eCharterOwnerEmail, e2eCharterOwnerPass)
	require.Equal(t, ledger.Balance{Available: 2*net + 225_440}, a.balance(t, pt))
	require.NoError(t, a.ledger.Verify(ctx, p.ID))
	var platform int64
	require.NoError(t, a.pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::bigint FROM platform_commission_entries`).Scan(&platform))
	require.Equal(t, 3*(gross-net), platform)
}
