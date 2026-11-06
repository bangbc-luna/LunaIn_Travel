package booking_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

// Final review I1: the webhook for R starts at 12:04:59 (on time) and reads
// R before it waits on the Vios lock. Meanwhile, at 12:05:00, R's hold has
// lapsed by time and another customer's overlapping request takes the last
// Vios and commits. The webhook must not confirm R as a second holder: R is
// refunded in full (late_payment, no ledger) and the other request keeps the
// vehicle. Whether the first delivery answers 500 (R became late under the
// lock: the provider retries) or refunds at once, after the retry exactly
// one booking holds the Vios.
func TestRentalPaid_HoldResoldWhileWaitingOnLock(t *testing.T) {
	r := newRentalEnv(t)
	v := r.acceptedR(t)
	oc := r.orderCode(t, v.ID)
	c2 := r.customer(t, "khach2@gmail.com")
	var other booking.BookingView
	r.Svc.SetBeforeRentalLock(func() {
		r.Svc.SetBeforeRentalLock(nil)
		r.Clock.T = vnAt(2026, 11, 5, 12, 5)
		other = r.createRental(t, c2, "other", r.rentalInput(r.Vios.ID, r.P0.ID, vnAt(2026, 11, 8, 8, 0), vnAt(2026, 11, 9, 8, 0)))
	})
	t.Cleanup(func() { r.Svc.SetBeforeRentalLock(nil) })
	r.Clock.T = vnAt(2026, 11, 5, 12, 4).Add(59 * time.Second)
	body := r.Fake.MarkPaid(oc, rentalGross)
	rec := r.webhook(t, body)
	require.NotEqual(t, uuid.Nil, other.ID, "the competing request ran inside the webhook")
	if rec.Code != http.StatusOK {
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		rec = r.webhook(t, body) // the provider retries
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	r.requireLateRefunded(t, v.ID, oc)
	require.Equal(t, booking.StatusRequested, r.bookingStatus(t, other.ID))
	holds, until := r.rentalHold(t, other.ID)
	require.True(t, holds)
	require.NotNil(t, until)
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM booking_rental_details WHERE holds_vehicle`))
}

// Final review (Task 7 M2): the customer withdraws an accepted request while
// Pay is creating its payment link (the Fake runs the withdraw inside
// CreateLink, before the link exists, so the withdraw's own link cancel
// finds nothing). Pay must answer 409 booking_not_payable and cancel the
// link it just created, so no live link is left on a withdrawn booking.
func TestPay_WithdrawWhileCreatingLink(t *testing.T) {
	r := newRentalEnv(t)
	ctx := context.Background()
	v := r.requestR(t, r.Customer, "r")
	r.Clock.T = vnAt(2026, 11, 5, 10, 0)
	// The accept creates no link (provider down), so Pay creates it.
	r.Fake.FailCreateLink(payment.ErrProviderUnavailable)
	r.accept(t, v.ID)
	r.Fake.FailCreateLink(nil)
	oc := r.orderCode(t, v.ID)
	require.Equal(t, 0, r.count(t, `SELECT count(*) FROM payments WHERE provider_order_code = $1 AND checkout_url IS NOT NULL`, oc))

	r.Clock.T = vnAt(2026, 11, 5, 11, 0)
	withdrawn := false
	r.Fake.OnNextCreateLink(func(payment.LinkRequest) {
		_, err := r.Svc.CancelByCustomer(ctx, r.Customer, v.ID, nil)
		require.NoError(t, err)
		withdrawn = true
	})
	tok := r.token(t, r.Customer, auth.RoleCustomer)
	rec := r.do(t, http.MethodPost, "/v1/bookings/"+v.ID.String()+"/pay", tok, nil)
	require.True(t, withdrawn, "the withdraw ran inside CreateLink")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"booking_not_payable"`)
	require.Equal(t, booking.StatusCancelled, r.bookingStatus(t, v.ID))
	require.Equal(t, "cancelled", r.paymentStatus(t, oc))
	require.Equal(t, payment.StateCancelled, r.fakeState(t, oc), "the link created during the withdraw is cancelled")
	r.requireReleased(t, v.ID)
}

// Final review (Task 7 M3): the hold lapses (ExpireHolds runs) while Pay is
// creating the link. Pay answers like its pre-check, 409 hold_expired, and
// cancels the link it created.
func TestPay_HoldExpiresWhileCreatingLink(t *testing.T) {
	r := newRentalEnv(t)
	v := r.requestR(t, r.Customer, "r")
	r.Clock.T = vnAt(2026, 11, 5, 10, 0)
	r.Fake.FailCreateLink(payment.ErrProviderUnavailable)
	r.accept(t, v.ID)
	r.Fake.FailCreateLink(nil)
	oc := r.orderCode(t, v.ID)

	r.Clock.T = vnAt(2026, 11, 5, 11, 59)
	r.Fake.OnNextCreateLink(func(payment.LinkRequest) {
		r.Clock.T = vnAt(2026, 11, 5, 12, 5)
		require.Equal(t, 1, r.expireHolds(t))
	})
	tok := r.token(t, r.Customer, auth.RoleCustomer)
	rec := r.do(t, http.MethodPost, "/v1/bookings/"+v.ID.String()+"/pay", tok, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"hold_expired"`)
	require.Equal(t, booking.StatusExpired, r.bookingStatus(t, v.ID))
	require.Equal(t, "expired", r.paymentStatus(t, oc))
	require.Equal(t, payment.StateCancelled, r.fakeState(t, oc))
}
