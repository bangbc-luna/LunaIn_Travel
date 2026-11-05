package booking_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

func TestCreateRide_OneWayHoldsSeats(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	v := e.createRide(t, e.Customer, "ride-1", rideInput(e.A1.ID, nil, 2))

	require.Regexp(t, checkoutCodeRe, v.Code)
	require.Equal(t, "open", v.Status)
	require.Equal(t, int64(500_000), v.TotalAmount)
	require.Len(t, v.Bookings, 1)
	b := v.Bookings[0]
	require.Regexp(t, bookingCodeRe, b.Code)
	require.Equal(t, "pending_payment", b.Status)
	require.Equal(t, rideType, b.ServiceType)
	require.Equal(t, e.A.ID, b.Listing.ID)
	require.Equal(t, int64(500_000), b.GrossAmount)
	require.Nil(t, b.QRToken)
	require.False(t, b.CanReschedule)
	require.False(t, b.CanCancel)
	require.NotNil(t, b.Payment)
	require.True(t, testNow.Add(15*time.Minute).Equal(b.Payment.ExpiresAt))
	require.Equal(t, []listing.CancellationTier{{HoursBefore: 48, RefundBps: 10000}, {HoursBefore: 24, RefundBps: 5000}}, b.CancellationPolicy)
	require.NotNil(t, b.Ride)
	require.Equal(t, e.A1.ID, b.Ride.TripID)
	require.Equal(t, "outbound", b.Ride.Direction)
	require.Equal(t, 2, b.Ride.Seats)
	require.Equal(t, "Bến xe Miền Đông", b.Ride.OriginName)
	require.Equal(t, "Bến xe Đà Lạt", b.Ride.DestName)
	require.True(t, vnAt(2026, 11, 7, 7, 0).Equal(b.Ride.DepartureAt))
	require.True(t, vnAt(2026, 11, 7, 10, 0).Equal(b.Ride.ArriveAt))
	require.Nil(t, b.Ride.PairedBookingID)
	require.Len(t, b.Items, 1)
	require.Equal(t, "Ghế Bến xe Miền Đông → Bến xe Đà Lạt, 07:00 07/11/2026", b.Items[0].Description)
	require.Equal(t, int64(250_000), b.Items[0].UnitPrice)
	require.Equal(t, float64(2), b.Items[0].Quantity)
	require.Equal(t, int64(500_000), b.Items[0].LineTotal)
	require.Nil(t, b.Items[0].TicketTypeID)

	var (
		gross, commission            int64
		rate                         int
		holdExp, holdRel, start, end time.Time
		orig                         time.Time
		checkoutID                   uuid.UUID
		phone, mail                  string
	)
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT gross_amount, commission_rate_bps, commission_amount,
		hold_expires_at, hold_release_at, service_start_at, service_end_at, original_service_start_at, checkout_id,
		contact_phone, contact_email
		FROM bookings WHERE id = $1`, b.ID).Scan(&gross, &rate, &commission, &holdExp, &holdRel, &start, &end, &orig,
		&checkoutID, &phone, &mail))
	require.Equal(t, int64(500_000), gross)
	require.Equal(t, 1000, rate)
	require.Equal(t, int64(50_000), commission)
	require.True(t, testNow.Add(15*time.Minute).Equal(holdExp), holdExp)
	require.True(t, testNow.Add(20*time.Minute).Equal(holdRel), holdRel)
	require.True(t, vnAt(2026, 11, 7, 7, 0).Equal(start), start)
	require.True(t, vnAt(2026, 11, 7, 7, 0).Equal(orig), orig)
	require.True(t, vnAt(2026, 11, 7, 10, 0).Equal(end), end)
	require.Equal(t, v.ID, checkoutID)
	require.Equal(t, "0987654321", phone)
	require.Equal(t, "binh@gmail.com", mail)

	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_items WHERE booking_id = $1 AND line_kind = 'seat' AND quantity = 2`, b.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_ride_details WHERE booking_id = $1 AND trip_id = $2
		AND listing_id = $3 AND seats = 2 AND direction = 'outbound'`, b.ID, e.A1.ID, e.A.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history WHERE booking_id = $1 AND to_status = 'pending_payment'
		AND from_status IS NULL AND actor_kind = 'customer'`, b.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM checkouts WHERE id = $1 AND total_amount = 500000 AND status = 'open'`, v.ID))
	_, st, amount, oc := e.checkoutPayment(t, v.ID)
	require.Equal(t, "pending", st)
	require.Equal(t, int64(500_000), amount)
	require.Equal(t, payment.StatePending, e.fakeState(t, oc), "the link is created after commit")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM idempotency_keys WHERE key = 'ride-1' AND endpoint = 'POST /v1/bookings/ride'
		AND response_status = 201`))
}

func TestCreateRide_RoundTripOneCheckout(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	v := e.createRide(t, e.Customer, "rt", rideInput(e.A1.ID, ptrID(e.B1.ID), 2))

	require.Equal(t, int64(940_000), v.TotalAmount)
	require.Len(t, v.Bookings, 2)
	out, ret := v.Bookings[0], v.Bookings[1]
	require.Equal(t, "outbound", out.Ride.Direction)
	require.Equal(t, "return", ret.Ride.Direction)
	require.Equal(t, int64(500_000), out.GrossAmount)
	require.Equal(t, int64(440_000), ret.GrossAmount)
	require.Equal(t, e.B.ID, ret.Listing.ID)
	require.Equal(t, e.B1.ID, ret.Ride.TripID)
	require.Equal(t, "Bến xe Đà Lạt", ret.Ride.OriginName)
	require.Equal(t, "Bến xe Miền Đông", ret.Ride.DestName)
	require.True(t, vnAt(2026, 11, 9, 16, 0).Equal(ret.Ride.ArriveAt))
	require.Equal(t, "Ghế Bến xe Đà Lạt → Bến xe Miền Đông, 13:00 09/11/2026", ret.Items[0].Description)
	require.Equal(t, int64(220_000), ret.Items[0].UnitPrice)
	require.NotNil(t, out.Ride.PairedBookingID)
	require.NotNil(t, ret.Ride.PairedBookingID)
	require.Equal(t, ret.ID, *out.Ride.PairedBookingID)
	require.Equal(t, out.ID, *ret.Ride.PairedBookingID)

	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM bookings WHERE checkout_id = $1`, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM checkouts`))
	require.Equal(t, 94_000, e.count(t, `SELECT sum(commission_amount)::int FROM bookings WHERE checkout_id = $1`, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments WHERE checkout_id = $1`, v.ID))
	_, _, amount, _ := e.checkoutPayment(t, v.ID)
	require.Equal(t, int64(940_000), amount)
	require.Equal(t, 1, e.count(t, `SELECT count(DISTINCT hold_release_at) FROM bookings WHERE checkout_id = $1`, v.ID))
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 2, e.seatsReserved(t, e.B1.ID))

	// Each leg has its own QR token once confirmed.
	e.confirm(t, out.ID)
	e.confirm(t, ret.ID)
	o, err := e.Svc.GetMine(ctx, e.Customer, out.ID)
	require.NoError(t, err)
	r, err := e.Svc.GetMine(ctx, e.Customer, ret.ID)
	require.NoError(t, err)
	require.NotNil(t, o.QRToken)
	require.NotNil(t, r.QRToken)
	require.NotEqual(t, *o.QRToken, *r.QRToken)
}

func TestCreateRide_Idempotent(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	first := e.createRide(t, e.Customer, "same", rideInput(e.A1.ID, ptrID(e.B1.ID), 2))
	again := e.createRide(t, e.Customer, "same", rideInput(e.A1.ID, ptrID(e.B1.ID), 2))
	require.Equal(t, first.ID, again.ID)
	require.Len(t, again.Bookings, 2)
	require.Equal(t, first.Bookings[0].ID, again.Bookings[0].ID)
	require.Equal(t, first.Bookings[1].ID, again.Bookings[1].ID)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM checkouts`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments`))
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))

	_, err := e.Svc.CreateRide(ctx, e.Customer, "same", rideInput(e.A1.ID, ptrID(e.B1.ID), 3))
	requireErr(t, err, http.StatusUnprocessableEntity, "idempotency_key_reused")
	// A key of the attraction endpoint is not replayed as a ride.
	e.create(t, "attraction-key")
	_, err = e.Svc.CreateRide(ctx, e.Customer, "attraction-key", rideInput(e.A1.ID, nil, 2))
	requireErr(t, err, http.StatusUnprocessableEntity, "idempotency_key_reused")
	_, err = e.Svc.CreateAttraction(ctx, e.Customer, "same", e.input())
	requireErr(t, err, http.StatusUnprocessableEntity, "idempotency_key_reused")
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 2, e.seatsReserved(t, e.B1.ID))

	// HTTP: 201 with the checkout, the key is required, unknown fields 400.
	tok := e.token(t, e.Customer, auth.RoleCustomer)
	body := map[string]any{
		"outbound_trip_id": e.A1.ID, "seats": 1,
		"contact_name": "An", "contact_phone": "0912345678", "contact_email": "an@gmail.com",
	}
	rec := e.do(t, "POST", "/v1/bookings/ride", tok, body, "Idempotency-Key", "http-1")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	got := decode[booking.CheckoutView](t, rec)
	require.Equal(t, int64(250_000), got.TotalAmount)
	require.Len(t, got.Bookings, 1)
	require.Contains(t, rec.Body.String(), `"ride":{"trip_id":"`+e.A1.ID.String()+`"`)
	requireCode(t, e.do(t, "POST", "/v1/bookings/ride", tok, body), http.StatusBadRequest, "idempotency_key_required")
	body["extra"] = 1
	require.Equal(t, http.StatusBadRequest, e.do(t, "POST", "/v1/bookings/ride", tok, body, "Idempotency-Key", "http-2").Code)
	require.Equal(t, 3, e.seatsReserved(t, e.A1.ID))
}

// Review Focus 1: two customers race for the last seat.
func TestCreateRide_LastSeatRace(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	e.exec(t, `UPDATE ride_trips SET seats_reserved = 9 WHERE id = $1`, e.A1.ID)
	customers := []uuid.UUID{e.Customer, e.customer(t, "khach2@gmail.com")}

	errs := make([]error, len(customers))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, c := range customers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = e.Svc.CreateRide(ctx, c, "last-seat", rideInput(e.A1.ID, nil, 1))
		}()
	}
	close(start)
	wg.Wait()

	ok, lost := 0, 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		requireErr(t, err, http.StatusConflict, "not_enough_seats")
		lost++
	}
	require.Equal(t, 1, ok)
	require.Equal(t, 1, lost)
	require.Equal(t, 10, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM checkouts`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM bookings`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM idempotency_keys`))
}

// Review Focus 2: a hold past release that the job has not swept yet is
// released by the booking that needs its seats, in the same tx.
func TestCreateRide_ReleasesOverdueHoldOnTrip(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	small := e.trip(t, e.RP, e.A, vnAt(2026, 11, 8, 7, 0), 250_000, 2)
	old := e.createRide(t, e.Customer, "old", rideInput(small.ID, nil, 2))
	oldBooking := old.Bookings[0].ID
	oldPay, _, _, oc := e.checkoutPayment(t, old.ID)
	other := e.customer(t, "khach2@gmail.com")

	// Hold expired for the customer but not released yet: still taken.
	e.Clock.T = testNow.Add(19 * time.Minute)
	_, err := e.Svc.CreateRide(ctx, other, "new", rideInput(small.ID, nil, 2))
	requireErr(t, err, http.StatusConflict, "not_enough_seats")
	require.Equal(t, "pending_payment", e.status(t, "bookings", oldBooking))

	// Past hold_release_at (+20'), the job has not run.
	e.Clock.T = testNow.Add(21 * time.Minute)
	v := e.createRide(t, other, "new", rideInput(small.ID, nil, 2))
	require.Equal(t, int64(500_000), v.TotalAmount)
	require.Equal(t, 2, e.seatsReserved(t, small.ID))
	require.Equal(t, "expired", e.status(t, "bookings", oldBooking))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history WHERE booking_id = $1
		AND from_status = 'pending_payment' AND to_status = 'expired' AND actor_kind = 'system' AND reason = 'hết thời gian giữ chỗ'`, oldBooking))
	require.Equal(t, "expired", e.status(t, "payments", oldPay))
	require.Equal(t, "closed", e.status(t, "checkouts", old.ID))
	require.Equal(t, payment.StateCancelled, e.fakeState(t, oc), "the old link is cancelled after commit")

	// The job finds nothing left to do.
	n, err := e.Svc.ExpireHolds(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, 2, e.seatsReserved(t, small.ID))
}

// Review Focus 2, round trip: the other leg on an unlocked trip is left to
// the job.
func TestCreateRide_ReleasesOverdueHoldOnTrip_RoundTripOtherLegLeft(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	small := e.trip(t, e.RP, e.A, vnAt(2026, 11, 8, 7, 0), 250_000, 2)
	old := e.createRide(t, e.Customer, "old", rideInput(small.ID, ptrID(e.B1.ID), 2))
	outID, retID := old.Bookings[0].ID, old.Bookings[1].ID
	oldPay, _, _, oc := e.checkoutPayment(t, old.ID)
	require.Equal(t, 2, e.seatsReserved(t, e.B1.ID))

	e.Clock.T = testNow.Add(21 * time.Minute)
	other := e.customer(t, "khach2@gmail.com")
	e.createRide(t, other, "new", rideInput(small.ID, nil, 2))
	require.Equal(t, 2, e.seatsReserved(t, small.ID))
	require.Equal(t, "expired", e.status(t, "bookings", outID))
	require.Equal(t, "pending_payment", e.status(t, "bookings", retID), "the return leg's trip was not locked")
	require.Equal(t, 2, e.seatsReserved(t, e.B1.ID))
	require.Equal(t, "pending", e.status(t, "payments", oldPay))
	require.Equal(t, "open", e.status(t, "checkouts", old.ID))
	require.Equal(t, payment.StatePending, e.fakeState(t, oc))

	n, err := e.Svc.ExpireHolds(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, "expired", e.status(t, "bookings", retID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
	require.Equal(t, 2, e.seatsReserved(t, small.ID))
	require.Equal(t, "expired", e.status(t, "payments", oldPay))
	require.Equal(t, "closed", e.status(t, "checkouts", old.ID))
	require.Equal(t, payment.StateCancelled, e.fakeState(t, oc))
}

// Review Focus 3: the return must leave once the outbound arrived, across
// midnight too.
func TestCreateRide_ReturnBeforeArrival422(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	night := e.trip(t, e.RP, e.A, vnAt(2026, 11, 7, 22, 0), 250_000, 10)
	early := e.trip(t, e.RP, e.B, vnAt(2026, 11, 8, 0, 30), 220_000, 10)
	onTime := e.trip(t, e.RP, e.B, vnAt(2026, 11, 8, 1, 0), 220_000, 10)

	_, err := e.Svc.CreateRide(ctx, e.Customer, "early", rideInput(night.ID, ptrID(early.ID), 1))
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_return_trip")
	require.Equal(t, 0, e.seatsReserved(t, night.ID))
	require.Equal(t, 0, e.seatsReserved(t, early.ID))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM checkouts`))

	v := e.createRide(t, e.Customer, "on-time", rideInput(night.ID, ptrID(onTime.ID), 1))
	require.Len(t, v.Bookings, 2)
	require.Equal(t, 1, e.seatsReserved(t, night.ID))
	require.Equal(t, 1, e.seatsReserved(t, onTime.ID))
}

func TestCreateRide_ReturnNotReverseOrOtherPartner422(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	sameListing := e.trip(t, e.RP, e.A, vnAt(2026, 11, 10, 7, 0), 250_000, 10)
	toHanoi := e.rideListing(t, e.RP, "Đà Lạt - Hà Nội", "68", "01")
	notReverse := e.trip(t, e.RP, toHanoi, vnAt(2026, 11, 10, 7, 0), 300_000, 10)
	rival := e.ridePartner(t, "Thanh")
	rivalBack := e.rideListing(t, rival, "Đà Lạt - Sài Gòn (Thành)", "68", "79")
	otherPartner := e.trip(t, rival, rivalBack, vnAt(2026, 11, 10, 7, 0), 200_000, 10)

	for name, ret := range map[string]uuid.UUID{
		"same trip": e.A1.ID, "same listing": sameListing.ID, "not reverse": notReverse.ID, "other partner": otherPartner.ID,
	} {
		_, err := e.Svc.CreateRide(ctx, e.Customer, "k-"+name, rideInput(e.A1.ID, ptrID(ret), 1))
		requireErr(t, err, http.StatusUnprocessableEntity, "invalid_return_trip")
		require.Equal(t, 0, e.seatsReserved(t, ret), name)
	}
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM checkouts`))
}

func TestCreateRide_BookingCutoff(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	tooLate := e.trip(t, e.RP, e.A, vnAt(2026, 11, 5, 9, 59), 250_000, 10)
	justOK := e.trip(t, e.RP, e.A, vnAt(2026, 11, 5, 10, 1), 250_000, 10)

	_, err := e.Svc.CreateRide(ctx, e.Customer, "late", rideInput(tooLate.ID, nil, 1))
	requireErr(t, err, http.StatusUnprocessableEntity, "trip_unavailable")
	require.Equal(t, 0, e.seatsReserved(t, tooLate.ID))
	e.createRide(t, e.Customer, "ok", rideInput(justOK.ID, nil, 1))

	// Without a cutoff the hold (15') + grace (5') still apply.
	zero := 0
	_, err = settings.NewService(e.Pool, e.Clock).Update(ctx, e.Admin, settings.Patch{RideBookingCutoffMinutes: &zero})
	require.NoError(t, err)
	in20 := e.trip(t, e.RP, e.A, vnAt(2026, 11, 5, 9, 21), 250_000, 10)
	in19 := e.trip(t, e.RP, e.A, vnAt(2026, 11, 5, 9, 19), 250_000, 10)
	e.createRide(t, e.Customer, "in20", rideInput(in20.ID, nil, 1))
	_, err = e.Svc.CreateRide(ctx, e.Customer, "in19", rideInput(in19.ID, nil, 1))
	requireErr(t, err, http.StatusUnprocessableEntity, "trip_unavailable")
	require.Equal(t, 0, e.seatsReserved(t, in19.ID))
}

func TestCreateRide_CancelledTrip422(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	_, err := e.Listing.CancelTripByPartner(ctx, e.RP.ID, e.RP.OwnerUserID, e.A.ID, e.A1.ID, "Xe hỏng")
	require.NoError(t, err)
	_, err = e.Svc.CreateRide(ctx, e.Customer, "cancelled", rideInput(e.A1.ID, nil, 1))
	requireErr(t, err, http.StatusUnprocessableEntity, "trip_unavailable")
	// A cancelled return leg too, and an unknown trip.
	_, err = e.Listing.CancelTripByPartner(ctx, e.RP.ID, e.RP.OwnerUserID, e.B.ID, e.B1.ID, "Xe hỏng")
	require.NoError(t, err)
	a2 := e.trip(t, e.RP, e.A, vnAt(2026, 11, 8, 7, 0), 250_000, 10)
	_, err = e.Svc.CreateRide(ctx, e.Customer, "cancelled-return", rideInput(a2.ID, ptrID(e.B1.ID), 1))
	requireErr(t, err, http.StatusUnprocessableEntity, "trip_unavailable")
	require.Equal(t, 0, e.seatsReserved(t, a2.ID))
	_, err = e.Svc.CreateRide(ctx, e.Customer, "unknown", rideInput(uuid.Must(uuid.NewV7()), nil, 1))
	requireErr(t, err, http.StatusUnprocessableEntity, "trip_unavailable")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM checkouts`))
}

func TestCreateRide_PartnerSuspended422(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, e.RP.ID, "Vi phạm"))
	_, err := e.Svc.CreateRide(ctx, e.Customer, "suspended", rideInput(e.A1.ID, ptrID(e.B1.ID), 1))
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM checkouts`))
}

func TestCreateRide_WrongServiceType422(t *testing.T) {
	e := newRideEnv(t)
	// An attraction listing has no trips: the attraction path stays separate.
	_, err := e.Svc.CreateAttraction(context.Background(), e.Customer, "attr-on-ride", booking.CreateAttractionInput{
		ListingID: e.A.ID, VisitDate: visitDate, TimeSlotID: e.Slot.ID,
		Items:       []booking.TicketLine{{TicketTypeID: e.Adult.ID, Quantity: 1}},
		ContactName: "An", ContactPhone: "0912345678", ContactEmail: "an@gmail.com",
	})
	requireErr(t, err, http.StatusUnprocessableEntity, "wrong_service_type")
}

func TestCreateRide_SeatsBounds(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	for _, n := range []int{0, -1, listing.MaxSeatsPerOperation + 1} {
		_, err := e.Svc.CreateRide(ctx, e.Customer, "bad", rideInput(e.A1.ID, nil, n))
		requireErr(t, err, http.StatusUnprocessableEntity, "invalid_booking")
	}
	v := e.createRide(t, e.Customer, "whole-bus", rideInput(e.A1.ID, nil, 10))
	require.Equal(t, int64(2_500_000), v.TotalAmount)
	require.Equal(t, 10, e.seatsReserved(t, e.A1.ID))

	_, err := e.Svc.CreateRide(ctx, e.Customer, "too-many", rideInput(e.B1.ID, nil, 11))
	requireErr(t, err, http.StatusConflict, "not_enough_seats")
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM checkouts`))
	// Not enough on the return leg: nothing is held on the outbound either.
	a2 := e.trip(t, e.RP, e.A, vnAt(2026, 11, 8, 7, 0), 250_000, 20)
	_, err = e.Svc.CreateRide(ctx, e.Customer, "return-short", rideInput(a2.ID, ptrID(e.B1.ID), 11))
	requireErr(t, err, http.StatusConflict, "not_enough_seats")
	require.Equal(t, 0, e.seatsReserved(t, a2.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM checkouts`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM idempotency_keys WHERE key <> 'whole-bus'`))
}

func TestExpireHolds_RideReleasesSeats(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	v := e.createRide(t, e.Customer, "expire", rideInput(e.A1.ID, ptrID(e.B1.ID), 2))
	payID, _, _, oc := e.checkoutPayment(t, v.ID)

	e.Clock.T = testNow.Add(15*time.Minute + 6*time.Minute)
	n, err := e.Svc.ExpireHolds(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	for _, b := range v.Bookings {
		require.Equal(t, "expired", e.status(t, "bookings", b.ID))
		require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history WHERE booking_id = $1 AND to_status = 'expired'`, b.ID))
	}
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
	require.Equal(t, "expired", e.status(t, "payments", payID))
	require.Equal(t, "closed", e.status(t, "checkouts", v.ID))
	require.Equal(t, payment.StateCancelled, e.fakeState(t, oc))

	// A second run releases nothing twice.
	n, err = e.Svc.ExpireHolds(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
}

func TestRideView_NoRescheduleAndQR(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	v := e.createRide(t, e.Customer, "view", rideInput(e.A1.ID, nil, 2))
	id := v.Bookings[0].ID

	mine, err := e.Svc.GetMine(ctx, e.Customer, id)
	require.NoError(t, err)
	require.Nil(t, mine.QRToken)
	require.False(t, mine.CanReschedule)

	e.confirm(t, id)
	mine, err = e.Svc.GetMine(ctx, e.Customer, id)
	require.NoError(t, err)
	require.NotNil(t, mine.QRToken)
	require.False(t, mine.CanReschedule)
	require.True(t, mine.CanCancel)
	require.NotNil(t, mine.Ride)

	e.exec(t, `UPDATE bookings SET status = 'checked_in', checked_in_at = $2 WHERE id = $1`, id, testNow)
	mine, err = e.Svc.GetMine(ctx, e.Customer, id)
	require.NoError(t, err)
	require.NotNil(t, mine.QRToken, "a checked-in ride keeps its QR")

	pv, err := e.Svc.GetForPartner(ctx, e.RP.ID, id)
	require.NoError(t, err)
	require.Nil(t, pv.QRToken)
	require.Equal(t, int64(50_000), *pv.CommissionAmount)
	require.NotNil(t, pv.Ride)

	rec := e.do(t, "POST", "/v1/bookings/"+id.String()+"/reschedule", e.token(t, e.Customer, auth.RoleCustomer),
		map[string]any{"visit_date": visitDate, "time_slot_id": e.Slot.ID})
	requireCode(t, rec, http.StatusConflict, "reschedule_not_allowed")

	// The partner lists the passengers of one trip.
	items, _, err := e.Svc.ListForPartner(ctx, e.RP.ID, booking.ListFilter{TripID: &e.A1.ID})
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, id, items[0].ID)
	items, _, err = e.Svc.ListForPartner(ctx, e.RP.ID, booking.ListFilter{TripID: &e.B1.ID})
	require.NoError(t, err)
	require.Empty(t, items)
	ptok := e.token(t, e.RP.OwnerUserID, auth.RolePartner)
	rec = e.do(t, "GET", "/v1/partner/bookings?trip_id="+e.A1.ID.String(), ptok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), id.String())
	rec = e.do(t, "GET", "/v1/partner/bookings?trip_id="+e.B1.ID.String(), ptok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), id.String())
	require.Equal(t, http.StatusBadRequest, e.do(t, "GET", "/v1/partner/bookings?trip_id=nope", ptok, nil).Code)
}
