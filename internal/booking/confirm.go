package booking

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
)

// Statuses and values used by payment confirmation.
const (
	StatusExpired = "expired"

	checkoutPaid = "paid"
	actorSystem  = "system"
	// reasonLateReconfirm is the history reason of a late payment that
	// re-confirms an expired booking.
	reasonLateReconfirm = "thanh toán trễ, xác nhận lại"
)

// HandlePaid results (payment_events.result).
const (
	ResultConfirmed         = "confirmed"
	ResultReconfirmed       = "reconfirmed"
	ResultLateRefunded      = "late_refunded"
	ResultUnderpaid         = "underpaid"
	ResultOverpaid          = "overpaid"
	ResultDuplicateRefunded = "duplicate_refunded"
	ResultAlreadyProcessed  = "already_processed"
)

// errStateChanged: a booking expired between the unlocked read and the lock,
// so its re-confirmation was not checked (that must happen before the
// checkout is locked). The webhook answers 500 and the provider retries.
var errStateChanged = errors.New("booking: booking state changed while confirming payment, retry")

var _ payment.PaymentHandler = (*Service)(nil)

// HandlePaid applies money the provider confirmed on a payment, inside the
// webhook's tx (payment.PaymentHandler). It is called at most once per
// incoming transfer (the webhook dedupes on order code + reference first).
//
// Lock order (DB §1.1): the checkout and its bookings are read unlocked;
// for every booking that is (or is about to be) expired, the re-confirmation
// checks run first — listing.BookableListing per listing in id order
// (partners, listings FOR SHARE), then SlotsForDate — and only then the
// checkout, its bookings (id order) and the payment are locked FOR UPDATE
// and the state is decided from the locked rows:
//
//   - The payment already received a transfer with this reference: nothing.
//   - It already received another transfer, or it was cancelled (its order
//     code was replaced): duplicate payment, refunded in full
//     (duplicate_payment, payment-level); no booking changes.
//   - Less than the payment amount: needs_review, refunded in full
//     (payment_mismatch, payment-level); the booking is not confirmed.
//   - Otherwise every pending_payment booking is confirmed, and every expired
//     booking that is still bookable (listing bookable, slot still offered on
//     the visit date, service not started) is re-confirmed; both post
//     ledger.PostBookingConfirmed. An expired booking that is no longer
//     bookable stays expired and is refunded in full (late_payment, per
//     booking) without any ledger entry. More than the amount: the excess is
//     refunded (payment_mismatch, payment-level) and the payment is
//     needs_review.
//
// Refunds start awaiting_bank_info, or pending when the customer saved a
// refund account. The customer is emailed (outbox) about each confirmation
// and refund.
func (s *Service) HandlePaid(ctx context.Context, tx pgx.Tx, ev payment.PaidEvent) (string, error) {
	q := store.New(tx)
	now := s.clock.Now()

	rows, err := q.ListCheckoutBookings(ctx, ev.CheckoutID)
	if err != nil {
		return "", fmt.Errorf("list checkout bookings: %w", err)
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("booking: checkout %s has no bookings", ev.CheckoutID)
	}
	rebookable, err := s.reconfirmChecks(ctx, tx, rows, now)
	if err != nil {
		return "", err
	}

	co, err := q.LockCheckout(ctx, ev.CheckoutID)
	if err != nil {
		return "", fmt.Errorf("lock checkout: %w", err)
	}
	bookings, err := q.LockCheckoutBookings(ctx, ev.CheckoutID)
	if err != nil {
		return "", fmt.Errorf("lock checkout bookings: %w", err)
	}
	pay, err := s.payments.LockPayment(ctx, tx, ev.PaymentID)
	if err != nil {
		return "", err
	}

	c := &confirmation{s: s, q: q, tx: tx, ev: ev, co: co, pay: pay, bookings: bookings, now: now}
	if pay.ProviderReference != nil && *pay.ProviderReference == ev.Reference {
		return ResultAlreadyProcessed, nil
	}
	if pay.ProviderReference != nil || !c.firstMoneyPossible() {
		return c.duplicate(ctx)
	}
	if ev.AmountPaid < pay.Amount {
		return c.underpaid(ctx)
	}
	return c.confirm(ctx, rebookable)
}

// reconfirmChecks evaluates, before any checkout lock, whether each booking
// that is expired (or whose hold is already past release, so the expiry job
// may expire it any moment) could still be re-confirmed. Bookings that do
// not need the check are absent from the map.
func (s *Service) reconfirmChecks(ctx context.Context, tx pgx.Tx, rows []store.ListCheckoutBookingsRow, now time.Time) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	var listingIDs []uuid.UUID
	var check []store.ListCheckoutBookingsRow
	for _, r := range rows {
		b := r.Booking
		late := b.Status == StatusPendingPayment && b.HoldReleaseAt != nil && !b.HoldReleaseAt.After(now)
		if b.Status != StatusExpired && !late {
			continue
		}
		check = append(check, r)
		if !slices.Contains(listingIDs, b.ListingID) {
			listingIDs = append(listingIDs, b.ListingID)
		}
	}
	slices.SortFunc(listingIDs, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	bookable := map[uuid.UUID]bool{}
	for _, id := range listingIDs {
		_, err := s.listings.BookableListing(ctx, tx, id)
		switch {
		case err == nil:
			bookable[id] = true
		case errors.Is(err, listing.ErrListingNotBookable):
			bookable[id] = false
		default:
			return nil, err
		}
	}
	for _, r := range check {
		ok := bookable[r.Booking.ListingID]
		if ok {
			var err error
			if ok, err = s.slotStillOffered(ctx, tx, r, now); err != nil {
				return nil, err
			}
		}
		out[r.Booking.ID] = ok
	}
	return out, nil
}

// slotStillOffered: the booking's service has not started and its time slot
// still runs on the visit date.
func (s *Service) slotStillOffered(ctx context.Context, tx pgx.Tx, r store.ListCheckoutBookingsRow, now time.Time) (bool, error) {
	b := r.Booking
	if b.ServiceType != serviceTypeAttraction || r.TimeSlotID == nil || !b.ServiceStartAt.After(now) {
		return false, nil
	}
	day, err := time.ParseInLocation(dateLayout, r.VisitDate, clock.VN)
	if err != nil {
		return false, fmt.Errorf("booking %s visit date %q: %w", b.ID, r.VisitDate, err)
	}
	slots, err := s.listings.SlotsForDate(ctx, tx, b.ListingID, day)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(slots, func(sl listing.TimeSlot) bool { return sl.ID == *r.TimeSlotID }), nil
}

// confirmation is one HandlePaid call after the locks were taken.
type confirmation struct {
	s        *Service
	q        *store.Queries
	tx       pgx.Tx
	ev       payment.PaidEvent
	co       store.Checkout
	pay      payment.Payment
	bookings []store.Booking
	now      time.Time
}

// firstMoneyPossible: the payment is still the one this checkout waits for
// and nothing of the checkout was paid by another payment.
func (c *confirmation) firstMoneyPossible() bool {
	if c.pay.Status != payment.StatusPending && c.pay.Status != payment.StatusExpired {
		return false
	}
	if c.co.Status == checkoutPaid {
		return false
	}
	for _, b := range c.bookings {
		if b.Status != StatusPendingPayment && b.Status != StatusExpired {
			return false
		}
	}
	return true
}

// duplicate refunds a transfer that arrived on a payment which already has
// money (or was replaced): nothing else changes.
func (c *confirmation) duplicate(ctx context.Context) (string, error) {
	if c.pay.ProviderReference == nil {
		// A cancelled (replaced) payment receives its first money.
		if err := c.s.payments.RecordReceived(ctx, c.tx, c.pay.ID, payment.StatusNeedsReview,
			c.ev.Reference, c.ev.PaidAt, c.ev.Raw); err != nil {
			return "", err
		}
	}
	if err := c.paymentRefund(ctx, c.ev.AmountPaid, payment.RefundDuplicatePayment); err != nil {
		return "", err
	}
	return ResultDuplicateRefunded, nil
}

// underpaid: less than the amount due. Nothing is confirmed, the transfer
// goes back in full and the payment needs a human look.
func (c *confirmation) underpaid(ctx context.Context) (string, error) {
	if err := c.s.payments.RecordReceived(ctx, c.tx, c.pay.ID, payment.StatusNeedsReview,
		c.ev.Reference, c.ev.PaidAt, c.ev.Raw); err != nil {
		return "", err
	}
	if err := c.paymentRefund(ctx, c.ev.AmountPaid, payment.RefundPaymentMismatch); err != nil {
		return "", err
	}
	return ResultUnderpaid, nil
}

// confirm handles a payment of at least the amount due.
func (c *confirmation) confirm(ctx context.Context, rebookable map[uuid.UUID]bool) (string, error) {
	var results []string
	confirmed := false
	for _, b := range c.bookings {
		switch b.Status {
		case StatusPendingPayment:
			if err := c.confirmBooking(ctx, b, nil); err != nil {
				return "", err
			}
			confirmed = true
			results = appendOnce(results, ResultConfirmed)
		case StatusExpired:
			ok, checked := rebookable[b.ID]
			if !checked {
				return "", errStateChanged
			}
			if ok {
				reason := reasonLateReconfirm
				if err := c.confirmBooking(ctx, b, &reason); err != nil {
					return "", err
				}
				confirmed = true
				results = appendOnce(results, ResultReconfirmed)
				continue
			}
			id := b.ID
			if err := c.refund(ctx, b.GrossAmount, payment.RefundLatePayment, &id, nil, b.ContactEmail); err != nil {
				return "", err
			}
			results = appendOnce(results, ResultLateRefunded)
		}
	}
	if confirmed {
		if err := c.q.SetCheckoutStatus(ctx, store.SetCheckoutStatusParams{Status: checkoutPaid, Now: c.now, ID: c.co.ID}); err != nil {
			return "", fmt.Errorf("set checkout paid: %w", err)
		}
	}
	status := payment.StatusPaid
	excess := c.ev.AmountPaid - c.pay.Amount
	if excess > 0 {
		status = payment.StatusNeedsReview
	}
	if err := c.s.payments.RecordReceived(ctx, c.tx, c.pay.ID, status, c.ev.Reference, c.ev.PaidAt, c.ev.Raw); err != nil {
		return "", err
	}
	if excess > 0 {
		if err := c.paymentRefund(ctx, excess, payment.RefundPaymentMismatch); err != nil {
			return "", err
		}
		results = append(results, ResultOverpaid)
	}
	return strings.Join(results, "+"), nil
}

// confirmBooking moves a locked booking to confirmed (from pending_payment,
// or from expired with lateReason), writes its history, posts it to the
// ledger and emails the customer.
func (c *confirmation) confirmBooking(ctx context.Context, b store.Booking, lateReason *string) error {
	if err := c.q.ConfirmBooking(ctx, store.ConfirmBookingParams{Now: c.now, ID: b.ID}); err != nil {
		return fmt.Errorf("confirm booking: %w", err)
	}
	histID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	from := b.Status
	if err := c.q.InsertStatusHistory(ctx, store.InsertStatusHistoryParams{
		ID: histID, BookingID: b.ID, FromStatus: &from, ToStatus: StatusConfirmed,
		ActorKind: actorSystem, Reason: lateReason, Now: c.now,
	}); err != nil {
		return fmt.Errorf("insert status history: %w", err)
	}
	if err := c.s.ledger.PostBookingConfirmed(ctx, c.tx, ledger.BookingMoney{
		BookingID: b.ID, PartnerID: b.PartnerID, ServiceType: b.ServiceType,
		Gross: b.GrossAmount, Commission: b.CommissionAmount,
	}); err != nil {
		return fmt.Errorf("post booking confirmed: %w", err)
	}
	return c.s.enqueueEmail(ctx, c.tx, mailer.Message{
		To:      b.ContactEmail,
		Subject: "Đặt chỗ " + b.Code + " đã được xác nhận",
		Text: "Xin chào " + b.ContactName + ",\n\n" +
			"LunaIn đã nhận được thanh toán và xác nhận đơn đặt chỗ của bạn.\n" +
			"Mã đặt chỗ: " + b.Code + "\n" +
			"Thời gian sử dụng dịch vụ: " + b.ServiceStartAt.In(clock.VN).Format("15:04 ngày 02/01/2006") + "\n\n" +
			"Vui lòng mở đơn trong tài khoản LunaIn để xem mã QR khi check-in.\n",
	})
}

// paymentRefund refunds amount of this transfer at payment level.
func (c *confirmation) paymentRefund(ctx context.Context, amount int64, reason string) error {
	ref := c.ev.Reference
	return c.refund(ctx, amount, reason, nil, &ref, c.bookings[0].ContactEmail)
}

func (c *confirmation) refund(ctx context.Context, amount int64, reason string, bookingID *uuid.UUID, reference *string, to string) error {
	r, err := c.s.payments.CreateRefund(ctx, c.tx, payment.RefundInput{
		PaymentID: c.pay.ID, CheckoutID: c.co.ID, CustomerID: c.co.CustomerID,
		BookingID: bookingID, ProviderReference: reference, Amount: amount, Reason: reason,
	})
	if err != nil {
		return err
	}
	text := "Xin chào,\n\n" +
		"LunaIn đã nhận được khoản chuyển " + formatVND(c.ev.AmountPaid) + " cho mã thanh toán " +
		strconv.FormatInt(c.ev.OrderCode, 10) + ". " + refundExplanation(reason) + "\n" +
		"Số tiền sẽ được hoàn lại: " + formatVND(amount) + ".\n"
	if r.Status == payment.RefundAwaitingBankInfo {
		text += "Vui lòng cập nhật tài khoản ngân hàng nhận hoàn tiền trong tài khoản LunaIn của bạn.\n"
	} else {
		text += "Tiền sẽ được chuyển vào tài khoản ngân hàng bạn đã lưu.\n"
	}
	return c.s.enqueueEmail(ctx, c.tx, mailer.Message{
		To: to, Subject: "LunaIn sẽ hoàn tiền " + formatVND(amount) + " cho bạn", Text: text,
	})
}

func refundExplanation(reason string) string {
	switch reason {
	case payment.RefundLatePayment:
		return "Tiền về sau khi hết thời gian giữ chỗ và chỗ không còn đặt được, nên đơn không thể xác nhận."
	case payment.RefundDuplicatePayment:
		return "Đơn này đã được thanh toán trước đó, khoản chuyển này bị trùng."
	default:
		return "Số tiền chuyển không khớp với số tiền cần thanh toán."
	}
}

// formatVND writes 380000 as "380.000 đ".
func formatVND(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(ch)
	}
	out := b.String() + " đ"
	if neg {
		out = "-" + out
	}
	return out
}

func appendOnce(xs []string, x string) []string {
	if slices.Contains(xs, x) {
		return xs
	}
	return append(xs, x)
}

// enqueueEmail queues an email in tx (sent only if tx commits). Without a
// job client (some tests) the email is only logged.
func (s *Service) enqueueEmail(ctx context.Context, tx pgx.Tx, m mailer.Message) error {
	if s.jobs == nil {
		s.log.Info("booking: email not queued (no job client)", "to", m.To, "subject", m.Subject)
		return nil
	}
	if err := mailer.EnqueueTx(ctx, tx, s.jobs, m); err != nil {
		return fmt.Errorf("enqueue email: %w", err)
	}
	return nil
}
