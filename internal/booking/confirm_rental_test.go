package booking_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

// R (request_test.go) is accepted at 10:00 on 05/11: payment due 12:00,
// hold released 12:05. 2.500.000 at 1000 bps: commission 250.000, partner
// 2.250.000.
const (
	rentalCommission = int64(250_000)
	rentalPartnerNet = int64(2_250_000)
)

// requireRentalBalance checks the rental partner's balance and its ledger.
func (r *rentalEnv) requireRentalBalance(t *testing.T, pending, available int64) {
	t.Helper()
	b, err := r.Ledger.Balance(context.Background(), r.RP.ID)
	require.NoError(t, err)
	require.Equal(t, ledger.Balance{Pending: pending, Available: available}, b)
	require.NoError(t, r.Ledger.Verify(context.Background(), r.RP.ID))
}

// requireRentalConfirmed: id is confirmed, holds its vehicle without end, the
// checkout is paid by the payment of oc, the sale is posted once and the
// partner was emailed about it.
func (r *rentalEnv) requireRentalConfirmed(t *testing.T, id uuid.UUID, oc int64) {
	t.Helper()
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, id))
	holds, until := r.rentalHold(t, id)
	require.True(t, holds)
	require.Nil(t, until)
	require.Equal(t, "paid", r.paymentStatus(t, oc))
	require.Equal(t, "paid", r.checkoutStatus(t, id))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM checkouts c JOIN payments p ON p.id = c.paid_payment_id
		WHERE p.provider_order_code = $1`, oc))
	require.Empty(t, r.refunds(t))
	r.requireRentalBalance(t, rentalPartnerNet, 0)
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_settlements WHERE booking_id = $1`, id))
	require.Equal(t, int(rentalCommission), r.count(t, `SELECT COALESCE(sum(amount), 0)::int FROM platform_commission_entries
		WHERE booking_id = $1 AND kind = 'commission'`, id))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'owner@thuexe.vn' AND args->>'subject' LIKE '%đã được thanh toán%'`))
}

// requireLateRefunded: id is expired without its vehicle, refunded in full
// (late_payment, per booking, awaiting bank info) and nothing was posted.
func (r *rentalEnv) requireLateRefunded(t *testing.T, id uuid.UUID, oc int64) {
	t.Helper()
	require.Equal(t, booking.StatusExpired, r.bookingStatus(t, id))
	r.requireReleased(t, id)
	require.Equal(t, []refundRow{
		{Amount: rentalGross, Reason: payment.RefundLatePayment, Status: "awaiting_bank_info", BookingID: &id},
	}, r.refunds(t))
	require.Equal(t, "paid", r.paymentStatus(t, oc))
	require.Equal(t, "closed", r.checkoutStatus(t, id))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM checkouts c JOIN bookings b ON b.checkout_id = c.id
		WHERE b.id = $1 AND c.paid_payment_id IS NOT NULL`, id))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM booking_settlements`))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM platform_commission_entries`))
	r.requireRentalBalance(t, 0, 0)
	require.Equal(t, booking.ResultLateRefunded, r.eventResult(t, oc))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'owner@thuexe.vn' AND args->>'subject' LIKE '%đã được thanh toán%'`))
}

func (r *rentalEnv) eventResult(t *testing.T, oc int64) string {
	t.Helper()
	return r.str(t, `SELECT result FROM payment_events WHERE dedupe_key = $1`, payment.DedupeKey(oc, ref(oc)))
}

// expiredR is R accepted at 10:00 and expired by the hold expiry job at
// 12:05; it returns R and its order code.
func (r *rentalEnv) expiredR(t *testing.T) (uuid.UUID, int64) {
	t.Helper()
	v := r.acceptedR(t)
	oc := r.orderCode(t, v.ID)
	r.Clock.T = vnAt(2026, 11, 5, 12, 5)
	require.Equal(t, 1, r.expireHolds(t))
	require.Equal(t, booking.StatusExpired, r.bookingStatus(t, v.ID))
	r.requireReleased(t, v.ID)
	return v.ID, oc
}

func TestRentalPaid_Confirms(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	oc := r.orderCode(t, v.ID)
	r.Clock.T = vnAt(2026, 11, 5, 11, 0)
	r.pay(t, oc, rentalGross)

	r.requireRentalConfirmed(t, v.ID, oc)
	require.Equal(t, booking.ResultConfirmed, r.eventResult(t, oc))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'awaiting_payment' AND to_status = 'confirmed'
		AND actor_kind = 'system' AND reason IS NULL`, v.ID))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'cuong@gmail.com' AND args->>'subject' LIKE '%đã được xác nhận%'`))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'owner@thuexe.vn' AND args->>'text' LIKE '%thanh toán trễ%'`))
}

// Paid after payment_due_at but inside the grace window: a normal
// confirmation, not the late path.
func TestRentalPaid_InGrace(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	oc := r.orderCode(t, v.ID)
	r.Clock.T = vnAt(2026, 11, 5, 12, 3)
	r.pay(t, oc, rentalGross)

	r.requireRentalConfirmed(t, v.ID, oc)
	require.Equal(t, booking.ResultConfirmed, r.eventResult(t, oc))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND to_status = 'confirmed' AND reason IS NOT NULL`, v.ID))
}

// Review Focus 3: nobody took the vehicle after R expired: R is re-confirmed.
func TestRentalLatePaid_ReconfirmsWhenFree(t *testing.T) {
	r := newRentalEnv(t)
	id, oc := r.expiredR(t)
	r.Clock.T = vnAt(2026, 11, 5, 12, 10)
	r.pay(t, oc, rentalGross)

	r.requireRentalConfirmed(t, id, oc)
	require.Equal(t, booking.ResultReconfirmed, r.eventResult(t, oc))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'expired' AND to_status = 'confirmed'
		AND actor_kind = 'system' AND reason = 'thanh toán trễ, xác nhận lại'`, id))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'owner@thuexe.vn' AND args->>'text' LIKE '%thanh toán trễ%vẫn phải phục vụ%'`))

	// The vehicle is held again: the last Vios is no longer available.
	c2 := r.customer(t, "khach2@gmail.com")
	_, err := r.Svc.CreateRental(context.Background(), c2, "late",
		r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 8, 8, 0), vnAt(2026, 11, 9, 8, 0)))
	requireErr(t, err, http.StatusConflict, "vehicle_unavailable")
}

// Review Focus 3: at 12:06 another customer took the last Vios over an
// overlapping range; R's money at 12:10 is refunded, the other hold stays.
func TestRentalLatePaid_UnitTakenRefunds(t *testing.T) {
	r := newRentalEnv(t)
	id, oc := r.expiredR(t)
	r.Clock.T = vnAt(2026, 11, 5, 12, 6)
	c2 := r.customer(t, "khach2@gmail.com")
	other := r.createRental(t, c2, "other", r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 8, 8, 0), vnAt(2026, 11, 9, 8, 0)))
	_, otherUntil := r.rentalHold(t, other.ID)
	require.NotNil(t, otherUntil)

	r.Clock.T = vnAt(2026, 11, 5, 12, 10)
	r.pay(t, oc, rentalGross)
	r.requireLateRefunded(t, id, oc)

	require.Equal(t, booking.StatusRequested, r.bookingStatus(t, other.ID))
	holds, until := r.rentalHold(t, other.ID)
	require.True(t, holds)
	require.True(t, otherUntil.Equal(*until))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_rental_details WHERE holds_vehicle`))
}

// Vision (quantity 2): two other requests overlap R but not each other, so
// the peak of concurrent holds is 1 and R is re-confirmed, where a raw count
// of overlapping holds (2) would refuse it. (R holds nothing while expired;
// the exclusion of R itself from the peak is what TestRentalPaid_Confirms
// relies on: an on-time payment on the last Vios must confirm.)
func TestRentalLatePaid_PeakNotRawCount(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.createRental(t, r.Customer, "r", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 9, 8, 0)))
	visionGross := int64(2 * 150_000)
	r.Clock.T = vnAt(2026, 11, 5, 10, 0)
	r.accept(t, v.ID)
	oc := r.orderCode(t, v.ID)
	r.Clock.T = vnAt(2026, 11, 5, 12, 5)
	require.Equal(t, 1, r.expireHolds(t))

	r.Clock.T = vnAt(2026, 11, 5, 12, 6)
	c2 := r.customer(t, "khach2@gmail.com")
	o1 := r.createRental(t, c2, "o1", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 8, 8, 0)))
	o2 := r.createRental(t, c2, "o2", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 8, 8, 0), vnAt(2026, 11, 9, 8, 0)))

	r.Clock.T = vnAt(2026, 11, 5, 12, 10)
	r.pay(t, oc, visionGross)
	require.Equal(t, booking.StatusConfirmed, r.bookingStatus(t, v.ID))
	holds, until := r.rentalHold(t, v.ID)
	require.True(t, holds)
	require.Nil(t, until)
	require.Empty(t, r.refunds(t))
	require.Equal(t, booking.ResultReconfirmed, r.eventResult(t, oc))
	r.requireRentalBalance(t, visionGross-visionGross/10, 0)
	for _, o := range []uuid.UUID{o1.ID, o2.ID} {
		holds, _ := r.rentalHold(t, o)
		require.True(t, holds)
	}
	// Now both units are taken over [07/11 08:00, 08/11 08:00).
	_, err := r.Svc.CreateRental(ctx, c2, "o3", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 7, 9, 0), vnAt(2026, 11, 7, 18, 0)))
	requireErr(t, err, http.StatusConflict, "vehicle_unavailable")
}

// The expiry job has not run: R is still awaiting_payment past 12:05 and
// the last Vios was taken at 12:06. The webhook expires R in its own tx and
// refunds it.
func TestRentalLatePaid_PendingPastReleaseUnitTaken(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	oc := r.orderCode(t, v.ID)
	r.Clock.T = vnAt(2026, 11, 5, 12, 6)
	c2 := r.customer(t, "khach2@gmail.com")
	other := r.createRental(t, c2, "other", r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 8, 8, 0), vnAt(2026, 11, 9, 8, 0)))
	require.Equal(t, booking.StatusAwaitingPayment, r.bookingStatus(t, v.ID))

	r.Clock.T = vnAt(2026, 11, 5, 12, 10)
	r.pay(t, oc, rentalGross)
	r.requireLateRefunded(t, v.ID, oc)
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'awaiting_payment' AND to_status = 'expired'
		AND reason = 'thanh toán trễ, không còn đặt được'`, v.ID))
	holds, _ := r.rentalHold(t, other.ID)
	require.True(t, holds)
}

// Past release, the job has not run and nobody took the vehicle: R is
// confirmed and its hold made permanent again.
func TestRentalLatePaid_PendingPastReleaseFreeConfirms(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	oc := r.orderCode(t, v.ID)
	r.Clock.T = vnAt(2026, 11, 5, 12, 10)
	r.pay(t, oc, rentalGross)
	r.requireRentalConfirmed(t, v.ID, oc)
}

func TestRentalLatePaid_ModelDeactivatedRefunds(t *testing.T) {
	r := newRentalEnv(t)
	id, oc := r.expiredR(t)
	off := false
	_, err := r.Listing.UpdateRentalModel(context.Background(), r.RP.ID, r.RP.OwnerUserID, r.RL.ID, r.Vios.ID,
		listing.RentalModelPatch{Active: &off})
	require.NoError(t, err)

	r.Clock.T = vnAt(2026, 11, 5, 12, 10)
	r.pay(t, oc, rentalGross)
	r.requireLateRefunded(t, id, oc)
}

func TestRentalLatePaid_ListingHiddenRefunds(t *testing.T) {
	r := newRentalEnv(t)
	id, oc := r.expiredR(t)
	_, err := r.Listing.Hide(context.Background(), r.Admin, r.RL.ID, "Vi phạm nội dung")
	require.NoError(t, err)

	r.Clock.T = vnAt(2026, 11, 5, 12, 10)
	r.pay(t, oc, rentalGross)
	r.requireLateRefunded(t, id, oc)
}

func TestRentalLatePaid_PartnerSuspendedRefunds(t *testing.T) {
	r := newRentalEnv(t)
	id, oc := r.expiredR(t)
	require.NoError(t, r.Partner.Suspend(context.Background(), r.Admin, r.RP.ID, "Vi phạm hợp đồng"))

	r.Clock.T = vnAt(2026, 11, 5, 12, 10)
	r.pay(t, oc, rentalGross)
	r.requireLateRefunded(t, id, oc)
}

// The money arrives once the pickup time has passed.
func TestRentalLatePaid_AfterPickupRefunds(t *testing.T) {
	r := newRentalEnv(t)
	id, oc := r.expiredR(t)
	r.Clock.T = vnAt(2026, 11, 7, 8, 0)
	r.pay(t, oc, rentalGross)
	r.requireLateRefunded(t, id, oc)
}

// A request that expired without the partner's acceptance is never
// re-confirmed, even if money reaches its checkout.
func TestRentalLatePaid_UnacceptedRequestRefunds(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.requestR(t, r.Customer, "r")
	r.Clock.T = vnAt(2026, 11, 5, 21, 0)
	n, err := r.Svc.ExpireRequests(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	var checkoutID uuid.UUID
	require.NoError(t, r.Pool.QueryRow(ctx, `SELECT checkout_id FROM bookings WHERE id = $1`, v.ID).Scan(&checkoutID))
	var oc int64
	require.NoError(t, db.InTx(ctx, r.Pool, func(tx pgx.Tx) error {
		p, err := r.Payments.ReservePayment(ctx, tx, checkoutID, rentalGross, vnAt(2026, 11, 5, 23, 0))
		oc = p.OrderCode
		return err
	}))

	r.pay(t, oc, rentalGross)
	require.Equal(t, booking.StatusExpired, r.bookingStatus(t, v.ID))
	r.requireReleased(t, v.ID)
	refunds := r.refunds(t)
	require.Len(t, refunds, 1)
	require.Equal(t, rentalGross, refunds[0].Amount)
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM ledger_entries`))
	r.requireRentalBalance(t, 0, 0)
}

// Money on a withdrawn request is refunded in full as a duplicate; the
// booking stays cancelled and nothing is posted.
func TestRentalPaidAfterWithdraw_RefundedInFull(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	oc := r.orderCode(t, v.ID)
	r.Clock.T = vnAt(2026, 11, 5, 11, 0)
	_, err := r.Svc.CancelByCustomer(context.Background(), r.Customer, v.ID, nil)
	require.NoError(t, err)

	r.pay(t, oc, rentalGross)
	refunds := r.refunds(t)
	require.Len(t, refunds, 1)
	require.Equal(t, rentalGross, refunds[0].Amount)
	require.Equal(t, payment.RefundDuplicatePayment, refunds[0].Reason)
	require.Nil(t, refunds[0].BookingID)
	require.Equal(t, booking.StatusCancelled, r.bookingStatus(t, v.ID))
	require.Equal(t, "customer_withdraw", r.str(t, `SELECT cancel_kind FROM bookings WHERE id = $1`, v.ID))
	r.requireReleased(t, v.ID)
	require.Equal(t, "closed", r.checkoutStatus(t, v.ID))
	require.Equal(t, booking.ResultDuplicateRefunded, r.eventResult(t, oc))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM platform_commission_entries`))
	r.requireRentalBalance(t, 0, 0)
}

// Pay (creating the link) races the withdraw: whatever the order, no live
// link is left on the withdrawn booking, and money arriving on any of its
// order codes is refunded in full without a ledger entry.
func TestRentalPayVsWithdraw_Race(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	for round := range 10 {
		c := r.customer(t, "race"+uuid.NewString()[:8]+"@gmail.com")
		v := r.createRental(t, c, "race", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 8, 8, 0)))
		r.Fake.FailCreateLink(errors.New("payos down"))
		r.Clock.T = vnAt(2026, 11, 5, 10, 0)
		r.accept(t, v.ID)
		r.Fake.FailCreateLink(nil)

		var (
			wg               sync.WaitGroup
			payErr, canceErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, payErr = r.Svc.Pay(ctx, c, v.ID)
		}()
		go func() {
			defer wg.Done()
			_, canceErr = r.Svc.CancelByCustomer(ctx, c, v.ID, nil)
		}()
		wg.Wait()
		require.NoError(t, canceErr, "round %d", round)
		if payErr != nil {
			requireErr(t, payErr, http.StatusConflict, "booking_not_payable")
		}
		require.Equal(t, booking.StatusCancelled, r.bookingStatus(t, v.ID))
		rows, err := r.Pool.Query(ctx, `SELECT p.provider_order_code FROM payments p
			JOIN bookings b ON b.checkout_id = p.checkout_id WHERE b.id = $1`, v.ID)
		require.NoError(t, err)
		var codes []int64
		for rows.Next() {
			var oc int64
			require.NoError(t, rows.Scan(&oc))
			codes = append(codes, oc)
		}
		require.NoError(t, rows.Err())
		for _, oc := range codes {
			st, err := r.Fake.GetStatus(ctx, oc)
			if err == nil {
				require.Equal(t, payment.StateCancelled, st.State, "round %d order %d", round, oc)
			} else {
				require.True(t, payment.IsOrderNotFound(err), "round %d: %v", round, err)
			}
		}
		before := len(r.refunds(t))
		r.pay(t, codes[0], 300_000)
		refunds := r.refunds(t)
		require.Len(t, refunds, before+1)
		require.Equal(t, payment.RefundDuplicatePayment, refunds[before].Reason)
		require.Equal(t, int64(300_000), refunds[before].Amount)
		require.Equal(t, booking.StatusCancelled, r.bookingStatus(t, v.ID))
	}
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM booking_rental_details WHERE holds_vehicle`))
}

func TestRentalWebhook_DuplicateDelivery(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	oc := r.orderCode(t, v.ID)
	r.Clock.T = vnAt(2026, 11, 5, 11, 0)
	body := r.Fake.MarkPaid(oc, rentalGross)
	for range 2 {
		rec := r.webhook(t, body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	r.requireRentalConfirmed(t, v.ID, oc)
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM payment_events`))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history WHERE to_status = 'confirmed'`))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM platform_commission_entries`))
}
