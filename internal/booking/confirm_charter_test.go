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
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

// Times below are Vietnam time. Charter request R (as in Task 5) is sent at
// 09:00 on 10/11: Xe 7 chỗ, airport -> Hoi An, pickup 11/11 08:00, 563.600,
// response due 21:00. Accepted at 10:00: payment due 12:00, hold released
// 12:05. At 1000 bps: commission 56.360, partner 507.240.
const (
	charterGross      = int64(563_600)
	charterCommission = int64(56_360)
	charterPartnerNet = int64(507_240)
)

// boundaryPickup is the pickup of the boundary request B (Review Focus 5):
// sent at 09:00, accepted at 09:30, payment due 11:30, released 11:35; the
// money must arrive by 12:10 (3 h before pickup) to re-confirm.
var boundaryPickup = vnAt(2026, 11, 10, 15, 10)

// acceptC accepts a charter request as CP's owner and requires success.
func (c *charterEnv) acceptC(t *testing.T, id uuid.UUID) booking.BookingView {
	t.Helper()
	v, err := c.Svc.AcceptRequest(context.Background(), c.CP.ID, c.CP.OwnerUserID, id)
	require.NoError(t, err)
	return v
}

// requestCR sends R for customer with key at the current clock.
func (c *charterEnv) requestCR(t *testing.T, customer uuid.UUID, key string) booking.BookingView {
	t.Helper()
	return c.createCharter(t, customer, key, c.charterInput(c.C7.ID, wed0800, false, nil, charterGross))
}

// acceptedCR is R accepted at 10:00; it returns R and its order code.
func (c *charterEnv) acceptedCR(t *testing.T) (booking.BookingView, int64) {
	t.Helper()
	v := c.requestCR(t, c.Customer, "r")
	c.Clock.T = vnAt(2026, 11, 10, 10, 0)
	v = c.acceptC(t, v.ID)
	return v, c.orderCode(t, v.ID)
}

// expiredCR is R accepted at 10:00 and expired by the hold expiry job at
// 12:05.
func (c *charterEnv) expiredCR(t *testing.T) (uuid.UUID, int64) {
	t.Helper()
	v, oc := c.acceptedCR(t)
	c.Clock.T = vnAt(2026, 11, 10, 12, 5)
	require.Equal(t, 1, c.expireHolds(t))
	require.Equal(t, booking.StatusExpired, c.bookingStatus(t, v.ID))
	return v.ID, oc
}

// acceptedB is the boundary request B, sent at 09:00 and accepted at 09:30.
func (c *charterEnv) acceptedB(t *testing.T) (uuid.UUID, int64) {
	t.Helper()
	v := c.createCharter(t, c.Customer, "b", c.charterInput(c.C7.ID, boundaryPickup, false, nil, charterGross))
	require.True(t, vnAt(2026, 11, 10, 13, 10).Equal(*v.Request.ResponseDueAt))
	c.Clock.T = vnAt(2026, 11, 10, 9, 30)
	v = c.acceptC(t, v.ID)
	require.True(t, vnAt(2026, 11, 10, 11, 30).Equal(*v.Request.PaymentDueAt))
	require.True(t, vnAt(2026, 11, 10, 11, 35).Equal(c.timeOf(t, `SELECT hold_release_at FROM bookings WHERE id = $1`, v.ID)))
	return v.ID, c.orderCode(t, v.ID)
}

// expiredB is B expired by the hold expiry job at 11:35.
func (c *charterEnv) expiredB(t *testing.T) (uuid.UUID, int64) {
	t.Helper()
	id, oc := c.acceptedB(t)
	c.Clock.T = vnAt(2026, 11, 10, 11, 35)
	require.Equal(t, 1, c.expireHolds(t))
	require.Equal(t, booking.StatusExpired, c.bookingStatus(t, id))
	return id, oc
}

func (c *charterEnv) requireCharterBalance(t *testing.T, pending int64) {
	t.Helper()
	b, err := c.Ledger.Balance(context.Background(), c.CP.ID)
	require.NoError(t, err)
	require.Equal(t, ledger.Balance{Pending: pending, Available: 0}, b)
	require.NoError(t, c.Ledger.Verify(context.Background(), c.CP.ID))
}

// requireCharterConfirmed: id is confirmed, the checkout is paid by the
// payment of oc, the sale is posted once (partner 507.240, platform 56.360)
// and the partner was emailed about it.
func (c *charterEnv) requireCharterConfirmed(t *testing.T, id uuid.UUID, oc int64) {
	t.Helper()
	require.Equal(t, booking.StatusConfirmed, c.bookingStatus(t, id))
	require.Equal(t, "paid", c.paymentStatus(t, oc))
	require.Equal(t, "paid", c.checkoutStatus(t, id))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM checkouts c JOIN payments p ON p.id = c.paid_payment_id
		WHERE p.provider_order_code = $1`, oc))
	require.Empty(t, c.refunds(t))
	c.requireCharterBalance(t, charterPartnerNet)
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM ledger_entries WHERE booking_id = $1`, id))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_settlements WHERE booking_id = $1`, id))
	require.Equal(t, int(charterCommission), c.count(t, `SELECT COALESCE(sum(amount), 0)::int FROM platform_commission_entries
		WHERE booking_id = $1 AND kind = 'commission'`, id))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'owner@xebaochuyen.vn' AND args->>'subject' LIKE '%đã được thanh toán%'`))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'binh@gmail.com' AND args->>'subject' LIKE '%đã được xác nhận%'`))
}

// requireCharterLateRefunded: id is expired, refunded in full (late_payment,
// per booking, awaiting bank info), nothing was posted and the customer was
// emailed about the refund.
func (c *charterEnv) requireCharterLateRefunded(t *testing.T, id uuid.UUID, oc int64) {
	t.Helper()
	require.Equal(t, booking.StatusExpired, c.bookingStatus(t, id))
	require.Equal(t, []refundRow{
		{Amount: charterGross, Reason: payment.RefundLatePayment, Status: "awaiting_bank_info", BookingID: &id},
	}, c.refunds(t))
	require.Equal(t, "paid", c.paymentStatus(t, oc))
	require.Equal(t, "closed", c.checkoutStatus(t, id))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM checkouts c JOIN bookings b ON b.checkout_id = c.id
		WHERE b.id = $1 AND c.paid_payment_id IS NOT NULL`, id))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM booking_settlements`))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM platform_commission_entries`))
	c.requireCharterBalance(t, 0)
	require.Equal(t, booking.ResultLateRefunded, c.eventResult(t, oc))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'binh@gmail.com' AND args->>'subject' LIKE '%hoàn tiền 563.600%'`))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'owner@xebaochuyen.vn' AND args->>'subject' LIKE '%đã được thanh toán%'`))
}

func (c *charterEnv) requireNoCharterMoney(t *testing.T) {
	t.Helper()
	require.Empty(t, c.refunds(t))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM platform_commission_entries`))
	c.requireCharterBalance(t, 0)
}

func TestCharterAccept_OpensPayment(t *testing.T) {
	c := newCharterEnv(t)
	v, oc := c.acceptedCR(t)

	require.Equal(t, booking.StatusAwaitingPayment, v.Status)
	due, release := vnAt(2026, 11, 10, 12, 0), vnAt(2026, 11, 10, 12, 5)
	require.True(t, vnAt(2026, 11, 10, 10, 0).Equal(*v.Request.AcceptedAt))
	require.True(t, due.Equal(*v.Request.PaymentDueAt))
	require.True(t, due.Equal(*v.HoldExpiresAt))
	require.True(t, release.Equal(c.timeOf(t, `SELECT hold_release_at FROM bookings WHERE id = $1`, v.ID)))
	require.Equal(t, int(charterGross), c.count(t, `SELECT amount FROM payments WHERE provider_order_code = $1 AND status = 'pending'`, oc))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM payments`))
	require.Equal(t, payment.StatePending, c.fakeState(t, oc))

	mine, err := c.Svc.GetMine(context.Background(), c.Customer, v.ID)
	require.NoError(t, err)
	require.NotNil(t, mine.Payment)
	require.NotNil(t, mine.Payment.CheckoutURL)
	require.True(t, due.Equal(mine.Payment.ExpiresAt))
	c.requireNoCharterMoney(t)
}

func TestCharterReject_AndWithdraw(t *testing.T) {
	c := newCharterEnv(t)
	ctx := context.Background()
	v := c.requestCR(t, c.Customer, "r")
	c.Clock.T = vnAt(2026, 11, 10, 10, 0)
	got, err := c.Svc.RejectRequest(ctx, c.CP.ID, c.CP.OwnerUserID, v.ID, "Hết xe ngày này")
	require.NoError(t, err)
	require.Equal(t, booking.StatusRejected, got.Status)
	require.Equal(t, booking.StatusRejected, c.bookingStatus(t, v.ID))
	require.Equal(t, "closed", c.checkoutStatus(t, v.ID))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM payments`))

	// Another request, accepted then withdrawn while awaiting payment.
	c2 := c.customer(t, "khach2@gmail.com")
	c.Clock.T = charterNow
	w := c.requestCR(t, c2, "w")
	c.Clock.T = vnAt(2026, 11, 10, 10, 0)
	c.acceptC(t, w.ID)
	oc := c.orderCode(t, w.ID)
	c.Clock.T = vnAt(2026, 11, 10, 11, 0)
	got, err = c.Svc.CancelByCustomer(ctx, c2, w.ID, nil)
	require.NoError(t, err)
	require.Equal(t, booking.StatusCancelled, got.Status)
	require.Equal(t, "customer_withdraw", c.str(t, `SELECT cancel_kind FROM bookings WHERE id = $1`, w.ID))
	require.Equal(t, "cancelled", c.paymentStatus(t, oc))
	require.Equal(t, payment.StateCancelled, c.fakeState(t, oc))
	require.Equal(t, "closed", c.checkoutStatus(t, w.ID))
	c.requireNoCharterMoney(t)
}

func TestCharterExpireRequests_AtResponseDue(t *testing.T) {
	c := newCharterEnv(t)
	ctx := context.Background()
	v := c.requestCR(t, c.Customer, "r")

	c.Clock.T = vnAt(2026, 11, 10, 20, 59)
	n, err := c.Svc.ExpireRequests(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, booking.StatusRequested, c.bookingStatus(t, v.ID))

	c.Clock.T = vnAt(2026, 11, 10, 21, 0)
	n, err = c.Svc.ExpireRequests(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, booking.StatusExpired, c.bookingStatus(t, v.ID))
	require.Equal(t, "closed", c.checkoutStatus(t, v.ID))
	c.requireNoCharterMoney(t)
}

func TestCharterPaid_Confirms(t *testing.T) {
	c := newCharterEnv(t)
	v, oc := c.acceptedCR(t)
	c.Clock.T = vnAt(2026, 11, 10, 11, 0)
	c.pay(t, oc, charterGross)

	c.requireCharterConfirmed(t, v.ID, oc)
	require.Equal(t, booking.ResultConfirmed, c.eventResult(t, oc))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'awaiting_payment' AND to_status = 'confirmed'
		AND actor_kind = 'system' AND reason IS NULL`, v.ID))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'owner@xebaochuyen.vn' AND args->>'text' LIKE '%thanh toán trễ%'`))
}

// After payment_due_at but inside the grace window: a normal confirmation.
func TestCharterPaid_InGrace(t *testing.T) {
	c := newCharterEnv(t)
	v, oc := c.acceptedCR(t)
	c.Clock.T = vnAt(2026, 11, 10, 12, 3)
	c.pay(t, oc, charterGross)

	c.requireCharterConfirmed(t, v.ID, oc)
	require.Equal(t, booking.ResultConfirmed, c.eventResult(t, oc))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND to_status = 'confirmed' AND reason IS NOT NULL`, v.ID))
}

func TestCharterLatePaid_Reconfirms(t *testing.T) {
	c := newCharterEnv(t)
	id, oc := c.expiredCR(t)
	c.Clock.T = vnAt(2026, 11, 10, 12, 10)
	c.pay(t, oc, charterGross)

	c.requireCharterConfirmed(t, id, oc)
	require.Equal(t, booking.ResultReconfirmed, c.eventResult(t, oc))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status = 'expired' AND to_status = 'confirmed'
		AND actor_kind = 'system' AND reason = 'thanh toán trễ, xác nhận lại'`, id))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email'
		AND args->>'to' = 'owner@xebaochuyen.vn' AND args->>'text' LIKE '%thanh toán trễ%vẫn phải phục vụ%'`))
}

// Review Focus 5: B (pickup 15:10) expired at 11:35. Money at 12:10 (exactly
// 3 h before pickup) re-confirms; at 12:11 (2 h 59) it is refunded. The same
// holds when the expiry job has not run yet (B still awaiting_payment past
// 11:35): the webhook expires B in its own tx.
func TestCharterLatePaid_ThreeHourBoundary(t *testing.T) {
	t.Run("expired, 3h00 before pickup: re-confirmed", func(t *testing.T) {
		c := newCharterEnv(t)
		id, oc := c.expiredB(t)
		c.Clock.T = vnAt(2026, 11, 10, 12, 10)
		c.pay(t, oc, charterGross)
		c.requireCharterConfirmed(t, id, oc)
		require.Equal(t, booking.ResultReconfirmed, c.eventResult(t, oc))
	})
	t.Run("expired, 2h59 before pickup: refunded", func(t *testing.T) {
		c := newCharterEnv(t)
		id, oc := c.expiredB(t)
		c.Clock.T = vnAt(2026, 11, 10, 12, 11)
		c.pay(t, oc, charterGross)
		c.requireCharterLateRefunded(t, id, oc)
	})
	t.Run("job not run, 3h00 before pickup: confirmed", func(t *testing.T) {
		c := newCharterEnv(t)
		id, oc := c.acceptedB(t)
		c.Clock.T = vnAt(2026, 11, 10, 12, 10)
		require.Equal(t, booking.StatusAwaitingPayment, c.bookingStatus(t, id))
		c.pay(t, oc, charterGross)
		c.requireCharterConfirmed(t, id, oc)
	})
	t.Run("job not run, 2h59 before pickup: expired and refunded", func(t *testing.T) {
		c := newCharterEnv(t)
		id, oc := c.acceptedB(t)
		c.Clock.T = vnAt(2026, 11, 10, 12, 11)
		require.Equal(t, booking.StatusAwaitingPayment, c.bookingStatus(t, id))
		c.pay(t, oc, charterGross)
		c.requireCharterLateRefunded(t, id, oc)
		require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_status_history
			WHERE booking_id = $1 AND from_status = 'awaiting_payment' AND to_status = 'expired'
			AND reason = 'thanh toán trễ, không còn đặt được'`, id))
	})
}

// charter_late_confirm_min_hours = 0: a late payment re-confirms up to the
// pickup time itself (>= 0).
func TestCharterLatePaid_SettingRespected(t *testing.T) {
	for _, at := range []time.Time{vnAt(2026, 11, 10, 15, 9), boundaryPickup} {
		t.Run(at.Format("15:04"), func(t *testing.T) {
			c := newCharterEnv(t)
			c.exec(t, `UPDATE platform_settings SET charter_late_confirm_min_hours = 0`)
			id, oc := c.expiredB(t)
			c.Clock.T = at
			c.pay(t, oc, charterGross)
			c.requireCharterConfirmed(t, id, oc)
			require.Equal(t, booking.ResultReconfirmed, c.eventResult(t, oc))
		})
	}
	// A larger setting is read too: 4 h refuses the 12:10 payment on B,
	// which the default (3 h) re-confirms.
	t.Run("4h refuses", func(t *testing.T) {
		c := newCharterEnv(t)
		c.exec(t, `UPDATE platform_settings SET charter_late_confirm_min_hours = 4`)
		id, oc := c.expiredB(t)
		c.Clock.T = vnAt(2026, 11, 10, 12, 10)
		c.pay(t, oc, charterGross)
		c.requireCharterLateRefunded(t, id, oc)
	})
}

// Ruling R2: the listing hidden after the hold expired: late money refunded.
func TestCharterLatePaid_ListingHiddenRefunds(t *testing.T) {
	c := newCharterEnv(t)
	id, oc := c.expiredCR(t)
	c.Clock.T = vnAt(2026, 11, 10, 12, 6)
	_, err := c.Listing.Hide(context.Background(), c.Admin, c.CL.ID, "Vi phạm nội dung")
	require.NoError(t, err)

	c.Clock.T = vnAt(2026, 11, 10, 12, 10)
	c.pay(t, oc, charterGross)
	c.requireCharterLateRefunded(t, id, oc)
}

// Ruling R2: the partner suspended after the hold expired: late money
// refunded.
func TestCharterLatePaid_PartnerSuspendedRefunds(t *testing.T) {
	c := newCharterEnv(t)
	id, oc := c.expiredCR(t)
	c.Clock.T = vnAt(2026, 11, 10, 12, 6)
	require.NoError(t, c.Partner.Suspend(context.Background(), c.Admin, c.CP.ID, "Vi phạm hợp đồng"))

	c.Clock.T = vnAt(2026, 11, 10, 12, 10)
	c.pay(t, oc, charterGross)
	c.requireCharterLateRefunded(t, id, oc)
}

// Ruling R2: money on time confirms even if the partner was suspended after
// accepting.
func TestCharterPaidOnTime_PartnerSuspendedStillConfirms(t *testing.T) {
	c := newCharterEnv(t)
	v, oc := c.acceptedCR(t)
	c.Clock.T = vnAt(2026, 11, 10, 10, 30)
	require.NoError(t, c.Partner.Suspend(context.Background(), c.Admin, c.CP.ID, "Vi phạm hợp đồng"))

	c.Clock.T = vnAt(2026, 11, 10, 11, 0)
	c.pay(t, oc, charterGross)
	c.requireCharterConfirmed(t, v.ID, oc)
	require.Equal(t, booking.ResultConfirmed, c.eventResult(t, oc))
}

// Review Focus 5: a request that expired without the partner's acceptance is
// never re-confirmed, even with money arriving ≫ 3 h before pickup.
func TestCharterLatePaid_UnacceptedRequestRefunds(t *testing.T) {
	c := newCharterEnv(t)
	ctx := context.Background()
	v := c.requestCR(t, c.Customer, "r")
	c.Clock.T = vnAt(2026, 11, 10, 21, 0)
	n, err := c.Svc.ExpireRequests(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	var checkoutID uuid.UUID
	require.NoError(t, c.Pool.QueryRow(ctx, `SELECT checkout_id FROM bookings WHERE id = $1`, v.ID).Scan(&checkoutID))
	var oc int64
	require.NoError(t, db.InTx(ctx, c.Pool, func(tx pgx.Tx) error {
		p, err := c.Payments.ReservePayment(ctx, tx, checkoutID, charterGross, vnAt(2026, 11, 10, 23, 0))
		oc = p.OrderCode
		return err
	}))

	c.pay(t, oc, charterGross)
	c.requireCharterLateRefunded(t, v.ID, oc)
}

func TestCharterPaidAfterWithdraw_RefundedInFull(t *testing.T) {
	c := newCharterEnv(t)
	v, oc := c.acceptedCR(t)
	c.Clock.T = vnAt(2026, 11, 10, 11, 0)
	_, err := c.Svc.CancelByCustomer(context.Background(), c.Customer, v.ID, nil)
	require.NoError(t, err)

	c.pay(t, oc, charterGross)
	refunds := c.refunds(t)
	require.Len(t, refunds, 1)
	require.Equal(t, charterGross, refunds[0].Amount)
	require.Equal(t, payment.RefundDuplicatePayment, refunds[0].Reason)
	require.Nil(t, refunds[0].BookingID)
	require.Equal(t, booking.StatusCancelled, c.bookingStatus(t, v.ID))
	require.Equal(t, booking.ResultDuplicateRefunded, c.eventResult(t, oc))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 0, c.count(t, `SELECT count(*) FROM platform_commission_entries`))
	c.requireCharterBalance(t, 0)
}

func TestCharterWebhook_DuplicateDelivery(t *testing.T) {
	c := newCharterEnv(t)
	v, oc := c.acceptedCR(t)
	c.Clock.T = vnAt(2026, 11, 10, 11, 0)
	body := c.Fake.MarkPaid(oc, charterGross)
	for range 2 {
		rec := c.webhook(t, body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	c.requireCharterConfirmed(t, v.ID, oc)
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM payment_events`))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM booking_status_history WHERE to_status = 'confirmed'`))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM ledger_entries`))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM platform_commission_entries`))
}
