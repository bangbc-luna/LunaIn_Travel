package booking_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

// Times below are Vietnam time. Request R (as in Task 5) is sent at 09:00 on
// 05/11: Vios at "Sân bay Liên Khương" over [07/11 08:00, 09/11 10:00),
// 2.500.000, response due at 21:00.

const rentalGross = int64(2_500_000)

// requestR sends R for customer with key.
func (r *rentalEnv) requestR(t *testing.T, customer uuid.UUID, key string) booking.BookingView {
	t.Helper()
	return r.createRental(t, customer, key, r.rentalInput(r.Vios.ID, r.P1.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 9, 10, 0)))
}

// accept accepts a request as the rental partner's owner and requires success.
func (r *rentalEnv) accept(t *testing.T, id uuid.UUID) booking.BookingView {
	t.Helper()
	v, err := r.Svc.AcceptRequest(context.Background(), r.RP.ID, r.RP.OwnerUserID, id)
	require.NoError(t, err)
	return v
}

// acceptedR is R accepted at 10:00.
func (r *rentalEnv) acceptedR(t *testing.T) booking.BookingView {
	t.Helper()
	v := r.requestR(t, r.Customer, "r")
	r.Clock.T = vnAt(2026, 11, 5, 10, 0)
	return r.accept(t, v.ID)
}

// noMoneyMoved: no refund, no ledger posting.
func (r *rentalEnv) noMoneyMoved(t *testing.T) {
	t.Helper()
	require.Empty(t, r.refunds(t))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM platform_commission_entries`))
}

// requireReleased: the booking no longer holds its vehicle.
func (r *rentalEnv) requireReleased(t *testing.T, id uuid.UUID) {
	t.Helper()
	holds, until := r.rentalHold(t, id)
	require.False(t, holds)
	require.Nil(t, until)
}

func TestAcceptRequest_OpensPayment(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.acceptedR(t)

	require.Equal(t, booking.StatusAwaitingPayment, v.Status)
	due, release := vnAt(2026, 11, 5, 12, 0), vnAt(2026, 11, 5, 12, 5)
	require.True(t, vnAt(2026, 11, 5, 10, 0).Equal(*v.Request.AcceptedAt))
	require.True(t, due.Equal(*v.Request.PaymentDueAt))
	require.True(t, due.Equal(*v.HoldExpiresAt))
	require.True(t, release.Equal(r.timeOf(t, `SELECT hold_release_at FROM bookings WHERE id = $1`, v.ID)))
	holds, until := r.rentalHold(t, v.ID)
	require.True(t, holds)
	require.True(t, release.Equal(*until))
	require.Nil(t, v.Payment, "the partner never sees the payment link")
	require.NotNil(t, v.CommissionAmount)

	// One pending payment of the gross, expiring at payment_due_at, with a link.
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM payments p JOIN bookings b ON b.checkout_id = p.checkout_id
		WHERE b.id = $1`, v.ID))
	require.Equal(t, int(rentalGross), r.count(t, `SELECT p.amount FROM payments p JOIN bookings b ON b.checkout_id = p.checkout_id
		WHERE b.id = $1 AND p.status = 'pending'`, v.ID))
	require.True(t, due.Equal(r.timeOf(t, `SELECT p.expires_at FROM payments p JOIN bookings b ON b.checkout_id = p.checkout_id
		WHERE b.id = $1`, v.ID)))
	oc := r.orderCode(t, v.ID)
	require.Equal(t, payment.StatePending, r.fakeState(t, oc))

	mine, err := r.Svc.GetMine(ctx, r.Customer, v.ID)
	require.NoError(t, err)
	require.NotNil(t, mine.Payment)
	require.NotNil(t, mine.Payment.CheckoutURL)
	require.True(t, due.Equal(mine.Payment.ExpiresAt))
	require.True(t, mine.CanCancel, "an accepted, unpaid request can still be withdrawn")

	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'requested' AND to_status = 'awaiting_payment'
		AND actor_kind = 'partner' AND actor_user_id = $2`, v.ID, r.RP.OwnerUserID))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'cuong@gmail.com' AND args->>'text' LIKE '%12:00 ngày 05/11/2026%'`))
	r.noMoneyMoved(t)
}

func TestAcceptRequest_HTTP(t *testing.T) {
	r := newRentalEnv(t)
	v := r.requestR(t, r.Customer, "r")
	tok := r.token(t, r.RP.OwnerUserID, auth.RolePartner)

	rec := r.do(t, http.MethodPost, "/v1/partner/bookings/not-a-uuid/accept", tok, nil)
	requireCode(t, rec, http.StatusNotFound, "not_found")
	rec = r.do(t, http.MethodPost, "/v1/partner/bookings/"+uuid.NewString()+"/accept", tok, nil)
	requireCode(t, rec, http.StatusNotFound, "not_found")

	rec = r.do(t, http.MethodPost, "/v1/partner/bookings/"+v.ID.String()+"/accept", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, booking.StatusAwaitingPayment, decode[booking.BookingView](t, rec).Status)
}

// Carry (Task 3/5): from response_due_at on, the request no longer holds the
// vehicle (hold_until > now), so a late accept must fail even if
// ExpireRequests has not swept it; otherwise the last vehicle could be held
// twice.
func TestAcceptRequest_AfterResponseDue409(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.requestR(t, r.Customer, "r")

	r.Clock.T = vnAt(2026, 11, 5, 21, 0)
	_, err := r.Svc.AcceptRequest(ctx, r.RP.ID, r.RP.OwnerUserID, v.ID)
	requireErr(t, err, http.StatusConflict, "request_expired")
	require.Equal(t, booking.StatusRequested, r.bookingStatus(t, v.ID))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM payments`))

	r.Clock.T = vnAt(2026, 11, 5, 20, 59)
	require.Equal(t, booking.StatusAwaitingPayment, r.accept(t, v.ID).Status)
}

func TestAcceptRequest_LateAcceptCannotDoubleHold(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.requestR(t, r.Customer, "r")

	// 21:00, not swept yet: another customer takes the only Vios.
	r.Clock.T = vnAt(2026, 11, 5, 21, 0)
	c2 := r.customer(t, "khach2@gmail.com")
	other := r.createRental(t, c2, "other", r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 8, 8, 0), vnAt(2026, 11, 9, 8, 0)))
	_, err := r.Svc.AcceptRequest(ctx, r.RP.ID, r.RP.OwnerUserID, v.ID)
	requireErr(t, err, http.StatusConflict, "request_expired")
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_rental_details
		WHERE vehicle_model_id = $1 AND holds_vehicle AND (hold_until IS NULL OR hold_until > $2)`, r.Vios.ID, r.Clock.T))
	holds, _ := r.rentalHold(t, other.ID)
	require.True(t, holds)
}

func TestAcceptRequest_Twice409(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	_, err := r.Svc.AcceptRequest(context.Background(), r.RP.ID, r.RP.OwnerUserID, v.ID)
	requireErr(t, err, http.StatusConflict, "request_not_pending")
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM payments`))
}

func TestAcceptRequest_InstantBooking409(t *testing.T) {
	r := newRentalEnv(t)
	a := r.create(t, "attraction")
	_, err := r.Svc.AcceptRequest(context.Background(), r.P.ID, r.P.OwnerUserID, a.ID)
	requireErr(t, err, http.StatusConflict, "request_not_pending")
}

func TestAcceptRequest_OtherPartner404(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.requestR(t, r.Customer, "r")
	_, err := r.Svc.AcceptRequest(ctx, r.P.ID, r.P.OwnerUserID, v.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = r.Svc.RejectRequest(ctx, r.P.ID, r.P.OwnerUserID, v.ID, "Hết xe")
	requireErr(t, err, http.StatusNotFound, "not_found")
	rec := r.do(t, http.MethodPost, "/v1/partner/bookings/"+v.ID.String()+"/accept", r.token(t, r.P.OwnerUserID, auth.RolePartner), nil)
	requireCode(t, rec, http.StatusNotFound, "not_found")
	require.Equal(t, booking.StatusRequested, r.bookingStatus(t, v.ID))
}

// Spec §5.2 [Chốt]: a suspended partner takes no new booking but still
// answers the requests already waiting (accept or reject).
func TestAcceptRequest_PartnerSuspendedStillAnswers(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.requestR(t, r.Customer, "r")
	c2 := r.customer(t, "khach2@gmail.com")
	w := r.createRental(t, c2, "w", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 8, 8, 0)))
	require.NoError(t, r.Partner.Suspend(ctx, r.Admin, r.RP.ID, "Vi phạm"))

	require.Equal(t, booking.StatusAwaitingPayment, r.accept(t, v.ID).Status)
	rej, err := r.Svc.RejectRequest(ctx, r.RP.ID, r.RP.OwnerUserID, w.ID, "Hết xe")
	require.NoError(t, err)
	require.Equal(t, booking.StatusRejected, rej.Status)
}

func TestRejectRequest_ReleasesVehicle(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.requestR(t, r.Customer, "r")
	tok := r.token(t, r.RP.OwnerUserID, auth.RolePartner)
	path := "/v1/partner/bookings/" + v.ID.String() + "/reject"

	long := strings.Repeat("ạ", 501)
	for _, body := range []any{map[string]any{"reason": "   "}, map[string]any{}, map[string]any{"reason": long}, map[string]any{"reason": "a\x01b"}} {
		rec := r.do(t, http.MethodPost, path, tok, body)
		requireCode(t, rec, http.StatusUnprocessableEntity, "invalid_reject")
	}
	_, err := r.Svc.RejectRequest(ctx, r.RP.ID, r.RP.OwnerUserID, v.ID, long)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_reject")
	rec := r.do(t, http.MethodPost, path, tok, map[string]any{"reason": "x", "extra": 1})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, booking.StatusRequested, r.bookingStatus(t, v.ID))

	r.Clock.T = vnAt(2026, 11, 5, 10, 0)
	tok = r.token(t, r.RP.OwnerUserID, auth.RolePartner)
	rec = r.do(t, http.MethodPost, path, tok, map[string]any{"reason": "  Xe đang bảo dưỡng  "})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decode[booking.BookingView](t, rec)
	require.Equal(t, booking.StatusRejected, got.Status)
	require.Equal(t, "Xe đang bảo dưỡng", *got.Request.RejectReason)
	require.True(t, vnAt(2026, 11, 5, 10, 0).Equal(*got.Request.RejectedAt))
	r.requireReleased(t, v.ID)
	require.Equal(t, "closed", r.checkoutStatus(t, v.ID))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'requested' AND to_status = 'rejected'
		AND actor_kind = 'partner' AND actor_user_id = $2 AND reason = 'Xe đang bảo dưỡng'`, v.ID, r.RP.OwnerUserID))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'cuong@gmail.com' AND args->>'text' LIKE '%Xe đang bảo dưỡng%'`))
	r.noMoneyMoved(t)

	// The vehicle is free again over the same period.
	c2 := r.customer(t, "khach2@gmail.com")
	again := r.createRental(t, c2, "again", r.rentalInput(r.Vios.ID, r.P1.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 9, 10, 0)))
	require.Equal(t, booking.StatusRequested, again.Status)

	// Rejecting twice is 409.
	_, err = r.Svc.RejectRequest(ctx, r.RP.ID, r.RP.OwnerUserID, v.ID, "Lần nữa")
	requireErr(t, err, http.StatusConflict, "request_not_pending")
}

// Past response_due_at but not swept yet, a request can still be rejected.
func TestRejectRequest_AfterResponseDueStillRejects(t *testing.T) {
	r := newRentalEnv(t)
	v := r.requestR(t, r.Customer, "r")
	r.Clock.T = vnAt(2026, 11, 5, 21, 30)
	// 500 characters is the limit.
	got, err := r.Svc.RejectRequest(context.Background(), r.RP.ID, r.RP.OwnerUserID, v.ID, strings.Repeat("ạ", 500))
	require.NoError(t, err)
	require.Equal(t, booking.StatusRejected, got.Status)
	r.requireReleased(t, v.ID)
}

// Spec §6.2: once accepted, the partner has no way back.
func TestRejectAfterAccept409(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	_, err := r.Svc.RejectRequest(context.Background(), r.RP.ID, r.RP.OwnerUserID, v.ID, "Đổi ý")
	requireErr(t, err, http.StatusConflict, "request_not_pending")
	require.Equal(t, booking.StatusAwaitingPayment, r.bookingStatus(t, v.ID))
	holds, _ := r.rentalHold(t, v.ID)
	require.True(t, holds)
	require.Equal(t, "pending", r.paymentStatus(t, r.orderCode(t, v.ID)))
}

func TestWithdraw_Requested(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.requestR(t, r.Customer, "r")
	r.Clock.T = vnAt(2026, 11, 5, 10, 0)
	// The bank account in the body is ignored: nothing was paid.
	got, err := r.Svc.CancelByCustomer(ctx, r.Customer, v.ID, &booking.BankAccount{BankBIN: "bad"})
	require.NoError(t, err)
	require.Equal(t, booking.StatusCancelled, got.Status)
	require.False(t, got.CanCancel)
	require.Equal(t, "customer_withdraw", r.str(t, `SELECT cancel_kind FROM bookings WHERE id = $1`, v.ID))
	require.Equal(t, r.Customer.String(), r.str(t, `SELECT cancelled_by::text FROM bookings WHERE id = $1`, v.ID))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'requested' AND to_status = 'cancelled'
		AND actor_kind = 'customer' AND actor_user_id = $2`, v.ID, r.Customer))
	r.requireReleased(t, v.ID)
	require.Equal(t, "closed", r.checkoutStatus(t, v.ID))
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM customer_refund_accounts`))
	r.noMoneyMoved(t)
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'owner@thuexe.vn' AND args->>'subject' LIKE '%rút%'`))

	// The vehicle is free again; a second withdraw is 409.
	c2 := r.customer(t, "khach2@gmail.com")
	r.createRental(t, c2, "again", r.rentalInput(r.Vios.ID, r.P1.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 9, 10, 0)))
	_, err = r.Svc.CancelByCustomer(ctx, r.Customer, v.ID, nil)
	requireErr(t, err, http.StatusConflict, "invalid_booking_state")

	// The partner cannot accept a withdrawn request.
	_, err = r.Svc.AcceptRequest(ctx, r.RP.ID, r.RP.OwnerUserID, v.ID)
	requireErr(t, err, http.StatusConflict, "request_not_pending")
}

func TestWithdraw_OtherCustomer404(t *testing.T) {
	r := newRentalEnv(t)
	v := r.requestR(t, r.Customer, "r")
	c2 := r.customer(t, "khach2@gmail.com")
	_, err := r.Svc.CancelByCustomer(context.Background(), c2, v.ID, nil)
	requireErr(t, err, http.StatusNotFound, "not_found")
	require.Equal(t, booking.StatusRequested, r.bookingStatus(t, v.ID))
}

func TestWithdraw_AwaitingPaymentCancelsPayment(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	oc := r.orderCode(t, v.ID)
	r.Clock.T = vnAt(2026, 11, 5, 11, 0)
	tok := r.token(t, r.Customer, auth.RoleCustomer)
	rec := r.do(t, http.MethodPost, "/v1/bookings/"+v.ID.String()+"/cancel", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decode[booking.BookingView](t, rec)
	require.Equal(t, booking.StatusCancelled, got.Status)
	require.Nil(t, got.Payment)
	require.Equal(t, "customer_withdraw", r.str(t, `SELECT cancel_kind FROM bookings WHERE id = $1`, v.ID))
	require.Equal(t, "cancelled", r.paymentStatus(t, oc))
	require.Equal(t, payment.StateCancelled, r.fakeState(t, oc), "the link is cancelled after commit")
	require.Equal(t, "closed", r.checkoutStatus(t, v.ID))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'awaiting_payment' AND to_status = 'cancelled'`, v.ID))
	r.requireReleased(t, v.ID)
	r.noMoneyMoved(t)

	// Money arriving on the cancelled link later is refunded in full.
	r.pay(t, oc, rentalGross)
	refunds := r.refunds(t)
	require.Len(t, refunds, 1)
	require.Equal(t, rentalGross, refunds[0].Amount)
	require.Equal(t, payment.RefundDuplicatePayment, refunds[0].Reason)
	require.Equal(t, booking.StatusCancelled, r.bookingStatus(t, v.ID))
	r.requireReleased(t, v.ID)
}

// confirmRental puts an accepted rental in the state a payment on time leaves
// it in (Task 7 confirms for real): confirmed, holding its vehicle without
// end, payment paid, checkout paid, the sale posted to the ledger.
func (r *rentalEnv) confirmRental(t *testing.T, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	oc := r.orderCode(t, id)
	r.exec(t, `UPDATE payments SET status = 'paid', provider_reference = $2, paid_at = $3
		WHERE provider_order_code = $1`, oc, ref(oc), r.Clock.T)
	r.exec(t, `UPDATE checkouts c SET status = 'paid', paid_payment_id = p.id FROM payments p
		WHERE p.provider_order_code = $1 AND c.id = p.checkout_id`, oc)
	r.exec(t, `UPDATE bookings SET status = 'confirmed', confirmed_at = $2 WHERE id = $1`, id, r.Clock.T)
	r.exec(t, `UPDATE booking_rental_details SET hold_until = NULL WHERE booking_id = $1`, id)
	require.NoError(t, db.InTx(ctx, r.Pool, func(tx pgx.Tx) error {
		return r.Ledger.PostBookingConfirmed(ctx, tx, ledger.BookingMoney{
			BookingID: id, PartnerID: r.RP.ID, ServiceType: rentalType, Gross: rentalGross, Commission: 250_000,
		})
	}))
}

// A paid (confirmed) rental is cancelled by policy, not withdrawn.
func TestWithdraw_ConfirmedGoesToPolicyCancel(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	r.Clock.T = vnAt(2026, 11, 5, 11, 0)
	r.confirmRental(t, v.ID)

	// 45h before pickup: 50% refund.
	got, err := r.Svc.CancelByCustomer(context.Background(), r.Customer, v.ID, nil)
	require.NoError(t, err)
	require.Equal(t, booking.StatusCancelled, got.Status)
	require.Equal(t, "customer", r.str(t, `SELECT cancel_kind FROM bookings WHERE id = $1`, v.ID))
	refunds := r.refunds(t)
	require.Len(t, refunds, 1)
	require.Equal(t, rentalGross/2, refunds[0].Amount)
	require.Equal(t, payment.RefundCustomerCancel, refunds[0].Reason)
	r.requireReleased(t, v.ID)
	require.NoError(t, r.Ledger.Verify(context.Background(), r.RP.ID))
}

// Review Focus 4: the customer withdraws exactly when the partner accepts.
// Both lock model -> checkout -> booking, so they serialize: the withdraw
// always wins in the end (it is allowed at requested and awaiting_payment),
// no payment stays pending, the vehicle is released and the accept either
// went first or is 409 request_not_pending.
func TestAcceptVsWithdraw_Race(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	acceptWon := 0
	for round := range 20 {
		v := r.createRental(t, r.Customer, "race-"+uuid.NewString(),
			r.rentalInput(r.Vios.ID, r.P1.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 9, 10, 0)))
		var (
			wg                   sync.WaitGroup
			acceptErr, cancelErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, acceptErr = r.Svc.AcceptRequest(ctx, r.RP.ID, r.RP.OwnerUserID, v.ID)
		}()
		go func() {
			defer wg.Done()
			_, cancelErr = r.Svc.CancelByCustomer(ctx, r.Customer, v.ID, nil)
		}()
		wg.Wait()

		require.NoError(t, cancelErr, "round %d", round)
		if acceptErr == nil {
			acceptWon++
		} else {
			requireErr(t, acceptErr, http.StatusConflict, "request_not_pending")
		}
		require.Equal(t, booking.StatusCancelled, r.bookingStatus(t, v.ID), "round %d", round)
		require.Equal(t, "customer_withdraw", r.str(t, `SELECT cancel_kind FROM bookings WHERE id = $1`, v.ID))
		r.requireReleased(t, v.ID)
		require.Equal(t, "closed", r.checkoutStatus(t, v.ID))
		require.Equal(t, 0, r.count(t, `SELECT count(*) FROM payments p JOIN bookings b ON b.checkout_id = p.checkout_id
			WHERE b.id = $1 AND p.status = 'pending'`, v.ID), "round %d", round)
		// Every order code of the checkout is unpayable at the provider.
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
		if acceptErr == nil {
			require.NotEmpty(t, codes)
		}
		for _, oc := range codes {
			st, err := r.Fake.GetStatus(ctx, oc)
			if err == nil {
				require.Equal(t, payment.StateCancelled, st.State, "round %d order %d", round, oc)
			} else {
				require.True(t, payment.IsOrderNotFound(err), "round %d: %v", round, err)
			}
		}
	}
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM booking_rental_details WHERE holds_vehicle`))
	t.Logf("accept went first in %d of 20 rounds", acceptWon)
	r.noMoneyMoved(t)
}

func TestExpireRequests_AtResponseDue(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.requestR(t, r.Customer, "r")
	before := r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email' AND args->>'to' = 'cuong@gmail.com'`)

	r.Clock.T = vnAt(2026, 11, 5, 20, 59)
	n, err := r.Svc.ExpireRequests(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, booking.StatusRequested, r.bookingStatus(t, v.ID))

	r.Clock.T = vnAt(2026, 11, 5, 21, 0)
	n, err = r.Svc.ExpireRequests(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, booking.StatusExpired, r.bookingStatus(t, v.ID))
	r.requireReleased(t, v.ID)
	require.Equal(t, "closed", r.checkoutStatus(t, v.ID))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'requested' AND to_status = 'expired'
		AND actor_kind = 'system' AND actor_user_id IS NULL AND reason = 'đối tác không phản hồi đúng hạn'`, v.ID))
	require.Equal(t, before+1, r.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email' AND args->>'to' = 'cuong@gmail.com'`))
	r.noMoneyMoved(t)

	// A second run finds nothing; an expired request cannot be accepted.
	n, err = r.Svc.ExpireRequests(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	_, err = r.Svc.AcceptRequest(ctx, r.RP.ID, r.RP.OwnerUserID, v.ID)
	requireErr(t, err, http.StatusConflict, "request_not_pending")
}

// A failing checkout is skipped (and backs off) without blocking the others.
func TestExpireRequests_FailingItemSkipped(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	a := r.requestR(t, r.Customer, "a")
	c2 := r.customer(t, "khach2@gmail.com")
	b := r.createRental(t, c2, "b", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 8, 8, 0)))
	var bad uuid.UUID
	require.NoError(t, r.Pool.QueryRow(ctx, `SELECT checkout_id FROM bookings WHERE id = $1`, a.ID).Scan(&bad))
	r.Svc.SetJobHooks(0, func(id uuid.UUID) error {
		if id == bad {
			return errors.New("boom")
		}
		return nil
	})
	r.Clock.T = vnAt(2026, 11, 5, 21, 0)
	n, err := r.Svc.ExpireRequests(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, booking.StatusRequested, r.bookingStatus(t, a.ID))
	require.Equal(t, booking.StatusExpired, r.bookingStatus(t, b.ID))

	// Backed off: the next run (a minute later is still inside the wait) skips it.
	r.Svc.SetJobHooks(0, nil)
	r.Clock.T = r.Clock.T.Add(30 * time.Second)
	n, err = r.Svc.ExpireRequests(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	r.Clock.T = r.Clock.T.Add(2 * time.Minute)
	n, err = r.Svc.ExpireRequests(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, booking.StatusExpired, r.bookingStatus(t, a.ID))
}

func TestExpireHolds_AwaitingPayment(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	oc := r.orderCode(t, v.ID)

	r.Clock.T = vnAt(2026, 11, 5, 12, 4)
	require.Equal(t, 0, r.expireHolds(t))
	require.Equal(t, booking.StatusAwaitingPayment, r.bookingStatus(t, v.ID))
	holds, _ := r.rentalHold(t, v.ID)
	require.True(t, holds)

	r.Clock.T = vnAt(2026, 11, 5, 12, 5)
	require.Equal(t, 1, r.expireHolds(t))
	require.Equal(t, booking.StatusExpired, r.bookingStatus(t, v.ID))
	require.Equal(t, "expired", r.paymentStatus(t, oc))
	require.Equal(t, payment.StateCancelled, r.fakeState(t, oc))
	require.Equal(t, "closed", r.checkoutStatus(t, v.ID))
	r.requireReleased(t, v.ID)
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'awaiting_payment' AND to_status = 'expired' AND actor_kind = 'system'`, v.ID))
	r.noMoneyMoved(t)
}

func TestPay_AwaitingPaymentRecreatesLink(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.requestR(t, r.Customer, "r")
	r.Clock.T = vnAt(2026, 11, 5, 10, 0)
	r.Fake.FailCreateLink(errors.New("payos down"))
	got := r.accept(t, v.ID)
	require.Equal(t, booking.StatusAwaitingPayment, got.Status, "a provider failure does not fail the accept")
	mine, err := r.Svc.GetMine(ctx, r.Customer, v.ID)
	require.NoError(t, err)
	require.NotNil(t, mine.Payment)
	require.Nil(t, mine.Payment.CheckoutURL)

	r.Fake.FailCreateLink(nil)
	r.Clock.T = vnAt(2026, 11, 5, 11, 59)
	tok := r.token(t, r.Customer, auth.RoleCustomer)
	rec := r.do(t, http.MethodPost, "/v1/bookings/"+v.ID.String()+"/pay", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	paid := decode[booking.BookingView](t, rec)
	require.NotNil(t, paid.Payment)
	require.NotNil(t, paid.Payment.CheckoutURL)
	require.True(t, vnAt(2026, 11, 5, 12, 0).Equal(paid.Payment.ExpiresAt))
	require.Equal(t, payment.StatePending, r.fakeState(t, r.orderCode(t, v.ID)))
}

// Carry (Task 3): Pay refuses once the payment deadline passed, even before
// the hold expiry job ran.
func TestPay_AwaitingPaymentAfterDeadline409(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.acceptedR(t)
	for _, at := range []time.Time{vnAt(2026, 11, 5, 12, 0), vnAt(2026, 11, 5, 12, 6)} {
		r.Clock.T = at
		_, err := r.Svc.Pay(ctx, r.Customer, v.ID)
		requireErr(t, err, http.StatusConflict, "hold_expired")
	}
	// A requested (not yet accepted) request cannot be paid.
	c2 := r.customer(t, "khach2@gmail.com")
	w := r.createRental(t, c2, "w", r.rentalInput(r.Vision.ID, r.P0.ID, vnAt(2026, 11, 7, 8, 0), vnAt(2026, 11, 8, 8, 0)))
	_, err := r.Svc.Pay(ctx, c2, w.ID)
	requireErr(t, err, http.StatusConflict, "booking_not_payable")
}

// timeOf scans one timestamptz.
func (r *rentalEnv) timeOf(t *testing.T, query string, args ...any) time.Time {
	t.Helper()
	var at time.Time
	require.NoError(t, r.Pool.QueryRow(context.Background(), query, args...).Scan(&at))
	return at
}
