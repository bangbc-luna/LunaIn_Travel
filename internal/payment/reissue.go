package payment

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/payment/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

// checkoutOpen is checkouts.status of a checkout still waiting for money.
const checkoutOpen = "open"

// ReissueAfterMismatch gives a checkout a new pending payment after a
// mismatched transfer (M-4): an underpayment leaves its payment
// needs_review, refunded in full, and no pending payment, so the customer
// could not pay again. In one tx (checkout -> bookings -> payments):
//
//   - a pending payment already exists: it is returned (a concurrent call
//     or a previous reissue made it);
//   - the checkout is open and its latest payment (largest id) is
//     needs_review with money received (provider_reference set) and
//     expires_at still ahead: a new pending payment of the same amount and
//     the same expires_at is reserved (the hold is never extended);
//   - anything else: ErrPaymentNotFound.
//
// No provider call: StartPayment creates the link after commit.
func (s *Service) ReissueAfterMismatch(ctx context.Context, checkoutID uuid.UUID) (Payment, error) {
	var out Payment
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		co, err := q.LockCheckoutRow(ctx, checkoutID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPaymentNotFound
		}
		if err != nil {
			return fmt.Errorf("lock checkout: %w", err)
		}
		if _, err := q.LockCheckoutBookingRows(ctx, checkoutID); err != nil {
			return fmt.Errorf("lock bookings: %w", err)
		}
		pays, err := s.LockCheckoutPayments(ctx, tx, checkoutID)
		if err != nil {
			return err
		}
		for _, p := range pays {
			if p.Status == StatusPending {
				out = p
				return nil
			}
		}
		if co.Status != checkoutOpen || len(pays) == 0 {
			return ErrPaymentNotFound
		}
		latest := pays[len(pays)-1] // id order
		if latest.Status != StatusNeedsReview || latest.ProviderReference == nil ||
			!latest.ExpiresAt.After(s.clock.Now()) {
			return ErrPaymentNotFound
		}
		out, err = s.ReservePayment(ctx, tx, checkoutID, latest.Amount, latest.ExpiresAt)
		if err != nil {
			return err
		}
		s.log.Info("payment: new payment reserved after a mismatched transfer",
			"checkout_id", checkoutID, "previous_order_code", latest.OrderCode, "order_code", out.OrderCode)
		return nil
	})
	return out, err
}
