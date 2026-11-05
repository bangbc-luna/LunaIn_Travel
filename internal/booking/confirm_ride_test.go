package booking_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

// Round trip of 2 seats on A1 (2 x 250.000) and B1 (2 x 220.000): 940.000,
// commission 10% = 50.000 + 44.000, partner 450.000 + 396.000.
const (
	rideTotal    = int64(940_000)
	rideOutGross = int64(500_000)
	rideRetGross = int64(440_000)
)

// roundTrip books 2 seats on A1 and B1 for the customer and returns the
// checkout, its outbound and return booking ids and its order code.
func (e *rideEnv) roundTrip(t *testing.T) (booking.CheckoutView, uuid.UUID, uuid.UUID, int64) {
	t.Helper()
	v := e.createRide(t, e.Customer, "rt", rideInput(e.A1.ID, ptrID(e.B1.ID), 2))
	require.Len(t, v.Bookings, 2)
	_, _, amount, oc := e.checkoutPayment(t, v.ID)
	require.Equal(t, rideTotal, amount)
	return v, v.Bookings[0].ID, v.Bookings[1].ID, oc
}

// requireRideBalance checks the ride partner's balance and its ledger.
func (e *rideEnv) requireRideBalance(t *testing.T, pending, available int64) {
	t.Helper()
	b, err := e.Ledger.Balance(context.Background(), e.RP.ID)
	require.NoError(t, err)
	require.Equal(t, ledger.Balance{Pending: pending, Available: available}, b)
	require.NoError(t, e.Ledger.Verify(context.Background(), e.RP.ID))
}

// requireSeatsConsistent: on every trip, the seats held by bookings that
// keep their seats (not expired or cancelled) add up to seats_reserved.
func (e *rideEnv) requireSeatsConsistent(t *testing.T) {
	t.Helper()
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ride_trips t
		WHERE t.seats_reserved <> COALESCE((SELECT sum(d.seats) FROM booking_ride_details d
			JOIN bookings b ON b.id = d.booking_id
			WHERE d.trip_id = t.id AND b.status NOT IN ('expired', 'cancelled')), 0)`))
}

// requireNoLedger: nothing was posted for any booking.
func (e *rideEnv) requireNoLedger(t *testing.T) {
	t.Helper()
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM booking_settlements`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM platform_commission_entries`))
	e.requireRideBalance(t, 0, 0)
}

// requireBothRefunded: both legs are expired and refunded in full
// (late_payment, per booking), the transfer is kept on a paid payment.
func (e *rideEnv) requireBothRefunded(t *testing.T, v booking.CheckoutView, out, ret uuid.UUID, oc int64) {
	t.Helper()
	require.Equal(t, "expired", e.bookingStatus(t, out))
	require.Equal(t, "expired", e.bookingStatus(t, ret))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
	require.Equal(t, "closed", e.status(t, "checkouts", v.ID))
	require.Equal(t, []refundRow{
		{Amount: rideOutGross, Reason: "late_payment", Status: "awaiting_bank_info", BookingID: &out},
		{Amount: rideRetGross, Reason: "late_payment", Status: "awaiting_bank_info", BookingID: &ret},
	}, e.refunds(t))
	require.Nil(t, e.nullUUID(t, `SELECT paid_payment_id FROM checkouts WHERE id = $1`, v.ID))
	e.requireNoLedger(t)
	e.requireSeatsConsistent(t)
	require.Equal(t, "late_refunded", e.eventResult(t, oc))
}

func (e *rideEnv) nullUUID(t *testing.T, query string, args ...any) *uuid.UUID {
	t.Helper()
	var id *uuid.UUID
	require.NoError(t, e.Pool.QueryRow(context.Background(), query, args...).Scan(&id))
	return id
}

func (e *rideEnv) eventResult(t *testing.T, oc int64) string {
	t.Helper()
	return e.str(t, `SELECT result FROM payment_events WHERE dedupe_key = $1`, payment.DedupeKey(oc, ref(oc)))
}

// expireRide runs the hold expiry job past the hold release.
func (e *rideEnv) expireRide(t *testing.T) {
	t.Helper()
	e.Clock.T = testNow.Add(21 * time.Minute)
	require.Equal(t, 2, e.expireHolds(t)) // both legs
}

func TestRidePaid_ConfirmsBothLegs(t *testing.T) {
	e := newRideEnv(t)
	v, out, ret, oc := e.roundTrip(t)
	e.pay(t, oc, rideTotal)

	require.Equal(t, "confirmed", e.bookingStatus(t, out))
	require.Equal(t, "confirmed", e.bookingStatus(t, ret))
	require.Equal(t, "paid", e.status(t, "checkouts", v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
	payID, _, _, _ := e.checkoutPayment(t, v.ID)
	require.Equal(t, &payID, e.nullUUID(t, `SELECT paid_payment_id FROM checkouts WHERE id = $1`, v.ID))
	e.requireRideBalance(t, 846_000, 0)
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 94_000, e.count(t, `SELECT COALESCE(sum(amount), 0)::int FROM platform_commission_entries WHERE kind = 'commission'`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM platform_commission_entries WHERE booking_id = $1 AND amount = 50000`, out))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM platform_commission_entries WHERE booking_id = $1 AND amount = 44000`, ret))
	require.Empty(t, e.refunds(t))
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 2, e.seatsReserved(t, e.B1.ID))
	e.requireSeatsConsistent(t)
	require.Equal(t, "confirmed", e.eventResult(t, oc))
}

// Review Focus 5: expired, seats back to 0, then the money comes: both legs
// are re-confirmed and their seats reserved again.
func TestRideLatePaid_ReconfirmsAndReReserves(t *testing.T) {
	e := newRideEnv(t)
	v, out, ret, oc := e.roundTrip(t)
	e.expireRide(t)
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))

	e.pay(t, oc, rideTotal)
	for _, id := range []uuid.UUID{out, ret} {
		require.Equal(t, "confirmed", e.bookingStatus(t, id))
		require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
			WHERE booking_id = $1 AND from_status = 'expired' AND to_status = 'confirmed'
			AND actor_kind = 'system' AND reason = 'thanh toán trễ, xác nhận lại'`, id))
		require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_settlements WHERE booking_id = $1`, id))
	}
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 2, e.seatsReserved(t, e.B1.ID))
	require.Equal(t, "paid", e.status(t, "checkouts", v.ID))
	require.Equal(t, "paid", e.paymentStatus(t, oc))
	require.Empty(t, e.refunds(t))
	e.requireRideBalance(t, 846_000, 0)
	e.requireSeatsConsistent(t)
	require.Equal(t, "reconfirmed", e.eventResult(t, oc))
}

// Review Focus 5: expired, A1 was sold out to someone else, then the money
// comes: both legs are refunded in full, nothing is posted, no seat moves.
func TestRideLatePaid_SeatsResoldRefundsBoth(t *testing.T) {
	e := newRideEnv(t)
	v, out, ret, oc := e.roundTrip(t)
	e.expireRide(t)
	other := e.customer(t, "khach2@gmail.com")
	e.createRide(t, other, "all", rideInput(e.A1.ID, nil, 10))
	require.Equal(t, 10, e.seatsReserved(t, e.A1.ID))

	e.pay(t, oc, rideTotal)
	e.requireBothRefunded(t, v, out, ret, oc)
	require.Equal(t, 10, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
}

func TestRideLatePaid_TripCancelledRefunds(t *testing.T) {
	e := newRideEnv(t)
	v, out, ret, oc := e.roundTrip(t)
	e.expireRide(t)
	_, err := e.Listing.CancelTripByPartner(context.Background(), e.RP.ID, e.RP.OwnerUserID, e.A.ID, e.A1.ID, "Xe hỏng")
	require.NoError(t, err)

	e.pay(t, oc, rideTotal)
	e.requireBothRefunded(t, v, out, ret, oc)
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
}

// The partner moved A1 once its seats were back to 0: only the hour sold is
// re-confirmed.
func TestRideLatePaid_TripTimeChangedRefunds(t *testing.T) {
	e := newRideEnv(t)
	v, out, ret, oc := e.roundTrip(t)
	e.expireRide(t)
	at := vnAt(2026, 11, 7, 8, 0)
	_, err := e.Listing.UpdateTrip(context.Background(), e.RP.ID, e.RP.OwnerUserID, e.A.ID, e.A1.ID, listing.TripPatch{DepartureAt: &at})
	require.NoError(t, err)

	e.pay(t, oc, rideTotal)
	e.requireBothRefunded(t, v, out, ret, oc)
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
}

// Paid in time, but an admin cancelled A1 meanwhile: both legs expire with
// their seats returned, and both are refunded.
func TestRideOnTimePaid_TripCancelledByAdminRefunds(t *testing.T) {
	e := newRideEnv(t)
	ctx := context.Background()
	v, out, ret, oc := e.roundTrip(t)
	require.NoError(t, db.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
		if _, err := e.Listing.LockTrips(ctx, tx, []uuid.UUID{e.A1.ID}); err != nil {
			return err
		}
		return e.Listing.CancelTripByAdmin(ctx, tx, e.A1.ID, e.Admin, "Đường bị sạt lở", testNow)
	}))
	e.Clock.Advance(5 * time.Minute)

	e.pay(t, oc, rideTotal)
	e.requireBothRefunded(t, v, out, ret, oc)
	for _, id := range []uuid.UUID{out, ret} {
		require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
			WHERE booking_id = $1 AND from_status = 'pending_payment' AND to_status = 'expired'
			AND reason = 'thanh toán trễ, không còn đặt được'`, id))
	}
	require.Equal(t, 0, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
}

// The hold passed release but the job has not run: the seats are still
// held, so the legs are confirmed without reserving more.
func TestRideLatePaid_PendingPastReleaseKeepsSeats(t *testing.T) {
	e := newRideEnv(t)
	v, out, ret, oc := e.roundTrip(t)
	e.Clock.T = testNow.Add(21 * time.Minute)

	e.pay(t, oc, rideTotal)
	require.Equal(t, "confirmed", e.bookingStatus(t, out))
	require.Equal(t, "confirmed", e.bookingStatus(t, ret))
	require.Equal(t, "paid", e.status(t, "checkouts", v.ID))
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 2, e.seatsReserved(t, e.B1.ID))
	require.Empty(t, e.refunds(t))
	e.requireRideBalance(t, 846_000, 0)
	e.requireSeatsConsistent(t)
}

// Another checkout's overdue hold on A1 is released in the webhook's tx and
// its link cancelled after commit.
func TestRidePaid_ReleasesOtherOverdueHolds(t *testing.T) {
	e := newRideEnv(t)
	other := e.customer(t, "khach2@gmail.com")
	old := e.createRide(t, other, "old", rideInput(e.A1.ID, nil, 3))
	oldPay, _, _, oldOC := e.checkoutPayment(t, old.ID)

	e.Clock.T = testNow.Add(10 * time.Minute)
	v, out, ret, oc := e.roundTrip(t)
	require.Equal(t, 5, e.seatsReserved(t, e.A1.ID))

	e.Clock.T = testNow.Add(21 * time.Minute) // old past release, ours not
	e.pay(t, oc, rideTotal)
	require.Equal(t, "confirmed", e.bookingStatus(t, out))
	require.Equal(t, "confirmed", e.bookingStatus(t, ret))
	require.Equal(t, "paid", e.status(t, "checkouts", v.ID))
	require.Equal(t, "expired", e.bookingStatus(t, old.Bookings[0].ID))
	require.Equal(t, "expired", e.status(t, "payments", oldPay))
	require.Equal(t, "closed", e.status(t, "checkouts", old.ID))
	require.Equal(t, payment.StateCancelled, e.fakeState(t, oldOC), "the old link is cancelled after commit")
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 2, e.seatsReserved(t, e.B1.ID))
	e.requireSeatsConsistent(t)
	e.requireRideBalance(t, 846_000, 0)
}

func TestRideWebhook_DuplicateDelivery(t *testing.T) {
	e := newRideEnv(t)
	_, out, ret, oc := e.roundTrip(t)
	body := e.Fake.MarkPaid(oc, rideTotal)
	for range 2 {
		rec := e.webhook(t, body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	require.Equal(t, "confirmed", e.bookingStatus(t, out))
	require.Equal(t, "confirmed", e.bookingStatus(t, ret))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payment_events`))
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM booking_status_history WHERE to_status = 'confirmed'`))
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM platform_commission_entries`))
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
	e.requireRideBalance(t, 846_000, 0)
}

// Task 5 carry: a round trip's outbound hold on a small trip is released by
// another customer's booking (releaseOverdueOnTrips), the return leg stays
// pending past release; the late webhook must expire the return leg with
// its seats returned and refund both.
func TestRideLatePaid_OneLegReleasedByCreateRefundsBoth(t *testing.T) {
	e := newRideEnv(t)
	small := e.trip(t, e.RP, e.A, vnAt(2026, 11, 8, 7, 0), 250_000, 2)
	v := e.createRide(t, e.Customer, "rt", rideInput(small.ID, ptrID(e.B1.ID), 2))
	out, ret := v.Bookings[0].ID, v.Bookings[1].ID
	_, _, _, oc := e.checkoutPayment(t, v.ID)

	e.Clock.T = testNow.Add(21 * time.Minute)
	other := e.customer(t, "khach2@gmail.com")
	e.createRide(t, other, "new", rideInput(small.ID, nil, 2))
	require.Equal(t, "expired", e.bookingStatus(t, out))
	require.Equal(t, "pending_payment", e.bookingStatus(t, ret))
	require.Equal(t, 2, e.seatsReserved(t, e.B1.ID))

	e.pay(t, oc, rideTotal)
	e.requireBothRefunded(t, v, out, ret, oc)
	require.Equal(t, 2, e.seatsReserved(t, small.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID), "the return leg's seats are returned")
}

// Same, but the small trip has 1 seat left: the outbound cannot re-reserve,
// so both legs are refunded.
func TestRideLatePaid_OneLegReleasedTooFewSeatsRefundsBoth(t *testing.T) {
	e := newRideEnv(t)
	small := e.trip(t, e.RP, e.A, vnAt(2026, 11, 8, 7, 0), 250_000, 4)
	v := e.createRide(t, e.Customer, "rt", rideInput(small.ID, ptrID(e.B1.ID), 2))
	out, ret := v.Bookings[0].ID, v.Bookings[1].ID
	_, _, _, oc := e.checkoutPayment(t, v.ID)

	e.Clock.T = testNow.Add(21 * time.Minute)
	other := e.customer(t, "khach2@gmail.com")
	e.createRide(t, other, "new", rideInput(small.ID, nil, 3))
	require.Equal(t, "expired", e.bookingStatus(t, out))
	require.Equal(t, 3, e.seatsReserved(t, small.ID))

	// 1 seat left on small: the outbound's 2 cannot come back.
	e.pay(t, oc, rideTotal)
	e.requireBothRefunded(t, v, out, ret, oc)
	require.Equal(t, 3, e.seatsReserved(t, small.ID))
	require.Equal(t, 0, e.seatsReserved(t, e.B1.ID))
}

// Same, with room left: the outbound re-reserves, the return keeps its held
// seats, both are confirmed.
func TestRideLatePaid_OneLegReleasedRoomLeftReconfirmsBoth(t *testing.T) {
	e := newRideEnv(t)
	small := e.trip(t, e.RP, e.A, vnAt(2026, 11, 8, 7, 0), 250_000, 4)
	v := e.createRide(t, e.Customer, "rt", rideInput(small.ID, ptrID(e.B1.ID), 2))
	out, ret := v.Bookings[0].ID, v.Bookings[1].ID
	_, _, _, oc := e.checkoutPayment(t, v.ID)

	e.Clock.T = testNow.Add(21 * time.Minute)
	other := e.customer(t, "khach2@gmail.com")
	e.createRide(t, other, "new", rideInput(small.ID, nil, 2))
	require.Equal(t, "expired", e.bookingStatus(t, out))

	e.pay(t, oc, rideTotal)
	require.Equal(t, "confirmed", e.bookingStatus(t, out))
	require.Equal(t, "confirmed", e.bookingStatus(t, ret))
	require.Equal(t, 4, e.seatsReserved(t, small.ID))
	require.Equal(t, 2, e.seatsReserved(t, e.B1.ID))
	require.Equal(t, "paid", e.status(t, "checkouts", v.ID))
	require.Empty(t, e.refunds(t))
	e.requireSeatsConsistent(t)
	e.requireRideBalance(t, 846_000, 0)
}

// Review I1: the seats freed by releasing another customer's overdue hold in
// the webhook's tx count for our expired legs.
func TestRideLatePaid_SeatsFreedByOverdueReleaseReconfirms(t *testing.T) {
	e := newRideEnv(t)
	v, out, ret, oc := e.roundTrip(t)
	e.expireRide(t) // testNow+21'
	other := e.customer(t, "khach2@gmail.com")
	old := e.createRide(t, other, "all", rideInput(e.A1.ID, nil, 10))
	_, _, _, oldOC := e.checkoutPayment(t, old.ID)
	require.Equal(t, 10, e.seatsReserved(t, e.A1.ID))

	e.Clock.T = testNow.Add(42 * time.Minute) // customer 2 past release, job not run
	e.pay(t, oc, rideTotal)
	require.Equal(t, "confirmed", e.bookingStatus(t, out))
	require.Equal(t, "confirmed", e.bookingStatus(t, ret))
	require.Equal(t, "expired", e.bookingStatus(t, old.Bookings[0].ID))
	require.Equal(t, "closed", e.status(t, "checkouts", old.ID))
	require.Equal(t, payment.StateCancelled, e.fakeState(t, oldOC))
	require.Equal(t, 2, e.seatsReserved(t, e.A1.ID))
	require.Equal(t, 2, e.seatsReserved(t, e.B1.ID))
	require.Equal(t, "paid", e.status(t, "checkouts", v.ID))
	require.Empty(t, e.refunds(t))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries
		WHERE kind = 'booking_earning' AND booking_id = $1 AND amount = 450000`, out))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM ledger_entries
		WHERE kind = 'booking_earning' AND booking_id = $1 AND amount = 396000`, ret))
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM ledger_entries`))
	e.requireSeatsConsistent(t)
	e.requireRideBalance(t, 846_000, 0)
	require.Equal(t, "reconfirmed", e.eventResult(t, oc))
}
