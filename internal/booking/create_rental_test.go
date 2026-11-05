package booking_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

// Times below are Vietnam time; testNow is 2026-11-05 09:00.

func TestCreateRental_RequestedHoldsVehicle(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	in := r.rentalInput(r.Vios.ID, r.P1.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 9, 10, 0))
	v := r.createRental(t, r.Customer, "rent-1", in)

	require.Equal(t, booking.StatusRequested, v.Status)
	require.Equal(t, rentalType, v.ServiceType)
	require.Equal(t, "request", r.str(t, `SELECT booking_mode FROM bookings WHERE id = $1`, v.ID))
	require.EqualValues(t, 2_500_000, v.GrossAmount)
	require.Equal(t, 2_500_000, r.count(t, `SELECT gross_amount FROM bookings WHERE id = $1`, v.ID))
	require.Equal(t, 250_000, r.count(t, `SELECT commission_amount FROM bookings WHERE id = $1`, v.ID))
	require.Equal(t, 1000, r.count(t, `SELECT commission_rate_bps FROM bookings WHERE id = $1`, v.ID))
	require.Equal(t, 2_500_000, r.count(t, `SELECT sum(line_total)::int FROM booking_items WHERE booking_id = $1`, v.ID))

	require.Len(t, v.Items, 2)
	require.Equal(t, "Thuê Toyota Vios 3 ngày", v.Items[0].Description)
	require.EqualValues(t, 800_000, v.Items[0].UnitPrice)
	require.EqualValues(t, 3, v.Items[0].Quantity)
	require.EqualValues(t, 2_400_000, v.Items[0].LineTotal)
	require.Equal(t, "Phí giao xe tại Sân bay Liên Khương", v.Items[1].Description)
	require.EqualValues(t, 100_000, v.Items[1].UnitPrice)
	require.EqualValues(t, 1, v.Items[1].Quantity)
	require.EqualValues(t, 100_000, v.Items[1].LineTotal)
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_items WHERE booking_id = $1 AND line_kind = 'rental_day'`, v.ID))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_items WHERE booking_id = $1 AND line_kind = 'delivery_fee'`, v.ID))

	due := vnAt(2026, 11, 5, 21, 0)
	require.NotNil(t, v.Request)
	require.NotNil(t, v.Request.ResponseDueAt)
	require.True(t, due.Equal(*v.Request.ResponseDueAt))
	require.Nil(t, v.Request.PaymentDueAt)
	require.Nil(t, v.HoldExpiresAt)
	holds, until := r.rentalHold(t, v.ID)
	require.True(t, holds)
	require.NotNil(t, until)
	require.True(t, due.Equal(*until))

	require.True(t, vnAt(2026, 11, 7, 8, 0).Equal(v.ServiceStartAt))
	require.True(t, vnAt(2026, 11, 9, 10, 0).Equal(v.ServiceEndAt))
	require.NotNil(t, v.Rental)
	require.Equal(t, r.Vios.ID, v.Rental.VehicleModelID)
	require.Equal(t, "Toyota Vios", v.Rental.ModelName)
	require.Equal(t, r.P1.ID, v.Rental.PickupPointID)
	require.Equal(t, "Sân bay Liên Khương", v.Rental.PickupPointName)
	require.Equal(t, 3, v.Rental.Days)
	require.Equal(t, "68", v.Rental.DestinationProvinceCode)

	// No payment yet; the checkout stays open for the whole gross.
	var checkoutID uuid.UUID
	require.NoError(t, r.Pool.QueryRow(ctx, `SELECT checkout_id FROM bookings WHERE id = $1`, v.ID).Scan(&checkoutID))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM payments WHERE checkout_id = $1`, checkoutID))
	require.Equal(t, "open", r.str(t, `SELECT status FROM checkouts WHERE id = $1`, checkoutID))
	require.Equal(t, 2_500_000, r.count(t, `SELECT total_amount FROM checkouts WHERE id = $1`, checkoutID))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND to_status = 'requested' AND actor_kind = 'customer' AND actor_user_id = $2`, v.ID, r.Customer))

	// The customer and the partner owner are emailed.
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'cuong@gmail.com' AND args->>'text' LIKE '%21:00 ngày 05/11/2026%'`))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'owner@thuexe.vn' AND args->>'text' LIKE '%21:00 ngày 05/11/2026%'`))
}

func TestCreateRental_ResponseDueCappedBeforePickup(t *testing.T) {
	r := newRentalEnv(t)
	v := r.createRental(t, r.Customer, "soon", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 5, 12, 0), vnAt(2026, 11, 6, 12, 0)))
	require.True(t, vnAt(2026, 11, 5, 10, 0).Equal(*v.Request.ResponseDueAt))
	_, until := r.rentalHold(t, v.ID)
	require.True(t, vnAt(2026, 11, 5, 10, 0).Equal(*until))
	// No delivery fee line for a free point.
	require.Len(t, v.Items, 1)
	require.EqualValues(t, 150_000, v.GrossAmount)
}

func TestCreateRental_PickupTooSoon422(t *testing.T) {
	r := newRentalEnv(t)
	_, err := r.Svc.CreateRental(context.Background(), r.Customer, "too-soon",
		r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 5, 11, 59), vnAt(2026, 11, 6, 12, 0)))
	requireErr(t, err, http.StatusUnprocessableEntity, "pickup_too_soon")
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM booking_rental_details`))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM idempotency_keys`))
}

// Review Focus 1: two customers race for the last Vios over overlapping
// periods; exactly one holds it.
func TestCreateRental_LastUnitRace(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	c2 := r.customer(t, "khach2@gmail.com")
	customers := []uuid.UUID{r.Customer, c2}
	for round := 0; round < 5; round++ {
		// Each round on fresh days, so earlier rounds do not interfere.
		d := 3 * round
		ins := []booking.CreateRentalInput{
			r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 7+d, 8, 0), vnAt(2026, 11, 9+d, 8, 0)),
			r.rentalInput(r.Vios.ID, r.P1.ID, vnAt(2026, 11, 8+d, 8, 0), vnAt(2026, 11, 10+d, 8, 0)),
		}
		var (
			wg   sync.WaitGroup
			errs = make([]error, 2)
		)
		start := make(chan struct{})
		for i := range 2 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = r.Svc.CreateRental(ctx, customers[i], uuid.NewString(), ins[i])
			}(i)
		}
		close(start)
		wg.Wait()
		ok := 0
		for _, err := range errs {
			if err == nil {
				ok++
				continue
			}
			requireErr(t, err, http.StatusConflict, "vehicle_unavailable")
		}
		require.Equal(t, 1, ok, "round %d: %v", round, errs)
		require.Equal(t, round+1, r.count(t, `SELECT count(*) FROM booking_rental_details WHERE holds_vehicle`))
		require.Equal(t, round+1, r.count(t, `SELECT count(*) FROM bookings WHERE status = 'requested'`))
	}
}

// Decision 5: availability is the peak of concurrent holds, not the count of
// overlapping holds.
func TestCreateRental_PeakAcceptsDisjointHolds(t *testing.T) {
	r := newRentalEnv(t)
	c2, c3 := r.customer(t, "a@gmail.com"), r.customer(t, "b@gmail.com")
	r.createRental(t, c2, "a", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 9, 8, 0), vnAt(2026, 11, 10, 8, 0)))
	r.createRental(t, c3, "b", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 11, 8, 0), vnAt(2026, 11, 12, 8, 0)))

	rec := r.do(t, http.MethodPost, "/v1/bookings/rental", r.token(t, r.Customer, auth.RoleCustomer),
		r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 9, 8, 0), vnAt(2026, 11, 12, 8, 0)), "Idempotency-Key", "peak-ok")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	v := decode[booking.BookingView](t, rec)
	require.Equal(t, booking.StatusRequested, v.Status)
	require.EqualValues(t, 450_000, v.GrossAmount)
	require.Equal(t, 3, r.count(t, `SELECT count(*) FROM booking_rental_details WHERE holds_vehicle`))
}

func TestCreateRental_PeakRefusesOverlappingHolds(t *testing.T) {
	r := newRentalEnv(t)
	c2, c3 := r.customer(t, "a@gmail.com"), r.customer(t, "b@gmail.com")
	r.createRental(t, c2, "a", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 9, 8, 0), vnAt(2026, 11, 11, 8, 0)))
	r.createRental(t, c3, "b", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 10, 8, 0), vnAt(2026, 11, 12, 8, 0)))

	rec := r.do(t, http.MethodPost, "/v1/bookings/rental", r.token(t, r.Customer, auth.RoleCustomer),
		r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 10, 8, 0), vnAt(2026, 11, 11, 8, 0)), "Idempotency-Key", "peak-no")
	requireCode(t, rec, http.StatusConflict, "vehicle_unavailable")
	require.Equal(t, 2, r.count(t, `SELECT count(*) FROM booking_rental_details`))
}

// Review Focus 5 (before handover): half-open periods, no buffer.
func TestCreateRental_BackToBackAllowed(t *testing.T) {
	r := newRentalEnv(t)
	c2 := r.customer(t, "a@gmail.com")
	r.createRental(t, c2, "r1", r.rentalInput(r.Vios.ID, r.P1.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 9, 10, 0)))
	v := r.createRental(t, r.Customer, "next", r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 9, 10, 0), vnAt(2026, 11, 10, 10, 0)))
	require.Equal(t, booking.StatusRequested, v.Status)
	require.Equal(t, 2, r.count(t, `SELECT count(*) FROM booking_rental_details WHERE holds_vehicle`))
}

func TestCreateRental_OverlapRejected409(t *testing.T) {
	r := newRentalEnv(t)
	c2 := r.customer(t, "a@gmail.com")
	r.createRental(t, c2, "r1", r.rentalInput(r.Vios.ID, r.P1.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 9, 10, 0)))
	_, err := r.Svc.CreateRental(context.Background(), r.Customer, "overlap",
		r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 8, 12, 0), vnAt(2026, 11, 9, 10, 0)))
	requireErr(t, err, http.StatusConflict, "vehicle_unavailable")
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_rental_details`))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM idempotency_keys WHERE key = 'overlap'`))
}

// A request past response_due_at that ExpireRequests has not swept yet no
// longer holds the vehicle.
func TestCreateRental_IgnoresOverdueHolds(t *testing.T) {
	r := newRentalEnv(t)
	c2 := r.customer(t, "a@gmail.com")
	old := r.createRental(t, c2, "old", r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 9, 8, 0), vnAt(2026, 11, 10, 8, 0)))

	// Just before 21:00 the hold still counts; from response_due_at on (hold_until > now) it no longer does.
	r.Clock.T = vnAt(2026, 11, 5, 20, 59)
	_, err := r.Svc.CreateRental(context.Background(), r.Customer, "before-due",
		r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 9, 8, 0), vnAt(2026, 11, 10, 8, 0)))
	requireErr(t, err, http.StatusConflict, "vehicle_unavailable")

	r.Clock.T = vnAt(2026, 11, 5, 21, 0)
	v := r.createRental(t, r.Customer, "at-due", r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 9, 8, 0), vnAt(2026, 11, 10, 8, 0)))
	require.Equal(t, booking.StatusRequested, v.Status)
	require.Equal(t, booking.StatusRequested, r.str(t, `SELECT status FROM bookings WHERE id = $1`, old.ID))
}

// Review Focus 5: the handover window is checked in Vietnam time, both ends
// included, whatever the zone the client sent.
func TestCreateRental_HandoverHours422(t *testing.T) {
	r := newRentalEnv(t)
	tok := r.token(t, r.Customer, auth.RoleCustomer)
	body := func(ret string) map[string]any {
		return map[string]any{
			"listing_id": r.RL.ID, "vehicle_model_id": r.Vision.ID, "pickup_point_id": r.P0.ID,
			"pickup_at": "2026-11-07T08:00:00+07:00", "return_at": ret, "destination_province_code": "68",
			"contact_name": "Lê Văn Cường", "contact_phone": "0901234567", "contact_email": "cuong@gmail.com",
		}
	}
	rec := r.do(t, http.MethodPost, "/v1/bookings/rental", tok, body("2026-11-07T22:30:00+07:00"), "Idempotency-Key", "late")
	requireCode(t, rec, http.StatusUnprocessableEntity, "outside_handover_hours")
	rec = r.do(t, http.MethodPost, "/v1/bookings/rental", tok, body("2026-11-07T15:31:00Z"), "Idempotency-Key", "late-utc")
	requireCode(t, rec, http.StatusUnprocessableEntity, "outside_handover_hours")
	b := body("2026-11-07T08:00:00Z")
	b["pickup_at"] = "2026-11-06T23:59:00Z" // 06:59 VN
	rec = r.do(t, http.MethodPost, "/v1/bookings/rental", tok, b, "Idempotency-Key", "early-utc-ok")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = r.do(t, http.MethodPost, "/v1/bookings/rental", tok, body("2026-11-07T15:00:00Z"), "Idempotency-Key", "edge")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	v := decode[booking.BookingView](t, rec)
	require.True(t, vnAt(2026, 11, 7, 22, 0).Equal(v.Rental.ReturnAt))
	require.Equal(t, 1, v.Rental.Days)

	// A pickup at 05:59 VN is outside too.
	b = body("2026-11-07T10:00:00+07:00")
	b["pickup_at"] = "2026-11-07T05:59:00+07:00"
	rec = r.do(t, http.MethodPost, "/v1/bookings/rental", tok, b, "Idempotency-Key", "early")
	requireCode(t, rec, http.StatusUnprocessableEntity, "outside_handover_hours")
}

func TestCreateRental_InactiveModelOrPoint(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	off := false
	_, err := r.Listing.UpdateRentalModel(ctx, r.RP.ID, r.RP.OwnerUserID, r.RL.ID, r.Vios.ID, listing.RentalModelPatch{Active: &off})
	require.NoError(t, err)
	_, err = r.Svc.CreateRental(ctx, r.Customer, "model-off",
		r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 8, 8, 0)))
	requireErr(t, err, http.StatusConflict, "vehicle_unavailable")

	_, err = r.Listing.UpdatePickupPoint(ctx, r.RP.ID, r.RP.OwnerUserID, r.RL.ID, r.P1.ID, listing.PickupPointPatch{Active: &off})
	require.NoError(t, err)
	_, err = r.Svc.CreateRental(ctx, r.Customer, "point-off",
		r.rentalInput(r.Vision.ID, r.P1.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 8, 8, 0)))
	requireErr(t, err, http.StatusNotFound, "not_found")
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM booking_rental_details`))
}

func TestCreateRental_InvalidDestination422(t *testing.T) {
	r := newRentalEnv(t)
	in := r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 8, 8, 0))
	in.DestinationProvinceCode = "99"
	_, err := r.Svc.CreateRental(context.Background(), r.Customer, "dest", in)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_destination")
	in.DestinationProvinceCode = ""
	_, err = r.Svc.CreateRental(context.Background(), r.Customer, "dest-empty", in)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_destination")
}

func TestCreateRental_ModelOfOtherListing404(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	// A second rental listing of the same partner (not approved) with its own model.
	l2, err := r.Listing.Create(ctx, r.RP.ID, r.RP.OwnerUserID, listing.CreateInput{ServiceType: rentalType, Title: "Thuê xe Bảo Lộc"})
	require.NoError(t, err)
	_, err = r.Listing.SetRentalStore(ctx, r.RP.ID, r.RP.OwnerUserID, l2.ID, listing.RentalStoreInput{HandoverOpen: "06:00", HandoverClose: "22:00"})
	require.NoError(t, err)
	other := r.rentalModel(t, l2.ID, "car", "Kia Morning", 4, 500_000, 3)

	_, err = r.Svc.CreateRental(ctx, r.Customer, "other",
		r.rentalInput(other.ID, r.P0.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 8, 8, 0)))
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = r.Svc.CreateRental(ctx, r.Customer, "unknown",
		r.rentalInput(uuid.Must(uuid.NewV7()), r.P0.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 8, 8, 0)))
	requireErr(t, err, http.StatusNotFound, "not_found")
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM booking_rental_details`))
}

func TestCreateRental_ModelNotOfListing404(t *testing.T) {
	r := newRentalEnv(t)
	// A rental model posted against the attraction listing: the model is
	// checked against the listing first.
	in := r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 8, 8, 0))
	in.ListingID = r.L.ID
	_, err := r.Svc.CreateRental(context.Background(), r.Customer, "wrong", in)
	requireErr(t, err, http.StatusNotFound, "not_found")
}

func TestCreateRental_PartnerSuspended422(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	require.NoError(t, r.Partner.Suspend(ctx, r.Admin, r.RP.ID, "Vi phạm"))
	_, err := r.Svc.CreateRental(ctx, r.Customer, "suspended",
		r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 8, 8, 0)))
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM booking_rental_details`))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM bookings WHERE service_type = 'self_drive_rental'`))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM checkouts`))
}

func TestCreateRental_Idempotent(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	in := r.rentalInput(r.Vios.ID, r.P1.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 9, 10, 0))
	a := r.createRental(t, r.Customer, "same", in)
	b := r.createRental(t, r.Customer, "same", in)
	require.Equal(t, a.ID, b.ID)
	require.Equal(t, a.Code, b.Code)
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM bookings WHERE service_type = 'self_drive_rental'`))
	// One customer email and one partner email only.
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email' AND args->>'to' = 'owner@thuexe.vn'`))

	in.ReturnAt = vnAt(2026, 11, 9, 11, 0)
	_, err := r.Svc.CreateRental(ctx, r.Customer, "same", in)
	requireErr(t, err, http.StatusUnprocessableEntity, "idempotency_key_reused")
	_, err = r.Svc.CreateRental(ctx, r.Customer, "", in)
	requireErr(t, err, http.StatusBadRequest, "idempotency_key_required")
	// The same key on another endpoint is reused too.
	_, err = r.Svc.CreateAttraction(ctx, r.Customer, "same", r.input())
	requireErr(t, err, http.StatusUnprocessableEntity, "idempotency_key_reused")
}

func TestRentalView_RequestedHasNoPaymentNoQR(t *testing.T) {
	r := newRentalEnv(t)
	v := r.createRental(t, r.Customer, "view", r.rentalInput(r.Vios.ID, r.P1.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 9, 10, 0)))

	rec := r.do(t, http.MethodGet, "/v1/bookings/"+v.ID.String(), r.token(t, r.Customer, auth.RoleCustomer), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
	require.Contains(t, m, "payment")
	require.Nil(t, m["payment"])
	require.NotContains(t, m, "qr_token")
	require.Equal(t, true, m["can_cancel"])
	require.Equal(t, false, m["can_reschedule"])
	req, ok := m["request"].(map[string]any)
	require.True(t, ok, rec.Body.String())
	due, err := time.Parse(time.RFC3339, req["response_due_at"].(string))
	require.NoError(t, err)
	require.True(t, vnAt(2026, 11, 5, 21, 0).Equal(due))
	rental, ok := m["rental"].(map[string]any)
	require.True(t, ok, rec.Body.String())
	require.Equal(t, "Sân bay Liên Khương, Đức Trọng", rental["pickup_address"])
	require.NotContains(t, m, "commission_amount")

	// The partner sees the request and its commission, never a QR token.
	pv, err := r.Svc.GetForPartner(context.Background(), r.RP.ID, v.ID)
	require.NoError(t, err)
	require.NotNil(t, pv.CommissionAmount)
	require.EqualValues(t, 250_000, *pv.CommissionAmount)
	require.Nil(t, pv.QRToken)
	require.NotNil(t, pv.Request)
	require.NotNil(t, pv.Rental)

	// An attraction or ride booking has no request block.
	a := r.create(t, "attraction")
	require.Nil(t, a.Request)
	require.Nil(t, a.Rental)
}
