package booking

import (
	"bytes"
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
	StatusExpired   = "expired"
	StatusCancelled = "cancelled"

	checkoutPaid   = "paid"
	checkoutClosed = "closed"
	actorSystem    = "system"
	// reasonLateReconfirm is the history reason of a late payment that
	// re-confirms an expired booking.
	reasonLateReconfirm = "thanh toán trễ, xác nhận lại"
	// reasonLateExpire: a pending booking whose hold was already past
	// release when the money arrived, and that can no longer be served.
	reasonLateExpire = "thanh toán trễ, không còn đặt được"
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
	// ResultAcceptedReplaced: money on a replaced (cancelled) order code was
	// accepted and the replacement payment cancelled (Ruling 10).
	ResultAcceptedReplaced = "accepted_replaced_link"
)

// errStateChanged: a booking became late (expired, or its hold passed
// release) between the unlocked read and the lock, so its re-confirmation was
// not checked (that must happen before the checkout is locked). The webhook
// answers 500 and the provider retries.
var errStateChanged = errors.New("booking: booking state changed while confirming payment, retry")

var _ payment.PaymentHandler = (*Service)(nil)

// HandlePaid applies money the provider confirmed on a payment, inside the
// webhook's tx (payment.PaymentHandler). It is called at most once per
// incoming transfer (the webhook dedupes on order code + reference first).
//
// Lock order (DB §1.1): the checkout and its bookings are read unlocked;
// for every booking that is late — expired, or pending_payment with its hold
// already past release (the expiry job lags) — the re-confirmation checks run
// first: listing.BookableListing per listing in id order (partners, listings
// FOR SHARE), then SlotsForDate and service_start_at > now. Only then the
// checkout and its bookings (id order) are locked FOR UPDATE, its payments
// (id order) FOR NO KEY UPDATE, and the state is decided from the locked rows:
//
//   - The payment already received a transfer with this reference: nothing.
//   - The checkout cannot take this money — it is paid (or one of its
//     payments is paid), a booking is past payment, or the payment was
//     replaced (cancelled, or another payment is now the pending one) and
//     some booking is no longer bookable: duplicate payment, refunded in full
//     (duplicate_payment, payment-level). Refunded strays and underpayments
//     (needs_review payments) do NOT block a later full payment (Ruling 14).
//   - Less than the payment amount: needs_review, refunded in full
//     (payment_mismatch, payment-level); nothing is confirmed.
//   - Otherwise, if the payment had been replaced (Ruling 10), it is accepted
//     and the replacement pending payment is cancelled (its link too, after
//     commit). Every booking still bookable is confirmed (pending_payment) or
//     re-confirmed (expired); a late booking that is no longer bookable is (or
//     stays) expired and refunded in full (late_payment, per booking) without
//     any ledger entry. More than the amount: the excess is refunded
//     (payment_mismatch, payment-level) and the payment is needs_review.
//
// Ledger postings are made after all booking updates, sorted by partner_id
// (then booking id) so that a future multi-partner checkout always locks
// partner_balances in ascending order. Refunds start awaiting_bank_info, or
// pending when the customer saved a refund account. The customer is emailed
// (outbox) about each confirmation and refund. An event the provider's status
// did not confirm (ev.NeedsReview) always leaves the payment needs_review.
func (s *Service) HandlePaid(ctx context.Context, tx pgx.Tx, ev payment.PaidEvent) (payment.PaidOutcome, error) {
	q := store.New(tx)
	now := s.clock.Now()

	rows, err := q.ListCheckoutBookings(ctx, ev.CheckoutID)
	if err != nil {
		return payment.PaidOutcome{}, fmt.Errorf("list checkout bookings: %w", err)
	}
	if len(rows) == 0 {
		return payment.PaidOutcome{}, fmt.Errorf("booking: checkout %s has no bookings", ev.CheckoutID)
	}
	rebookable, err := s.reconfirmChecks(ctx, tx, rows, now)
	if err != nil {
		return payment.PaidOutcome{}, err
	}

	co, err := q.LockCheckout(ctx, ev.CheckoutID)
	if err != nil {
		return payment.PaidOutcome{}, fmt.Errorf("lock checkout: %w", err)
	}
	bookings, err := q.LockCheckoutBookings(ctx, ev.CheckoutID)
	if err != nil {
		return payment.PaidOutcome{}, fmt.Errorf("lock checkout bookings: %w", err)
	}
	pays, err := s.payments.LockCheckoutPayments(ctx, tx, ev.CheckoutID)
	if err != nil {
		return payment.PaidOutcome{}, err
	}
	i := slices.IndexFunc(pays, func(p payment.Payment) bool { return p.ID == ev.PaymentID })
	if i < 0 {
		return payment.PaidOutcome{}, payment.ErrPaymentNotFound
	}

	c := &confirmation{
		s: s, q: q, tx: tx, ev: ev, co: co, pay: pays[i], pays: pays,
		bookings: bookings, rebookable: rebookable, now: now,
	}
	if ref := c.pay.ProviderReference; ref != nil && *ref == ev.Reference {
		return payment.PaidOutcome{Result: ResultAlreadyProcessed}, nil
	}
	canTake, err := c.canTakeMoney(ctx)
	if err != nil {
		return payment.PaidOutcome{}, err
	}
	if !canTake {
		return c.duplicate(ctx)
	}
	if ev.AmountPaid < c.pay.Amount {
		return c.underpaid(ctx)
	}
	return c.confirm(ctx)
}

// lateBooking: expired, or pending_payment with the hold already past
// release at now.
func lateBooking(b store.Booking, now time.Time) bool {
	return b.Status == StatusExpired ||
		(b.Status == StatusPendingPayment && b.HoldReleaseAt != nil && !b.HoldReleaseAt.After(now))
}

// reconfirmChecks evaluates, before any checkout lock, whether each late
// booking could still be (re-)confirmed. Bookings that are not late are
// absent from the map.
func (s *Service) reconfirmChecks(ctx context.Context, tx pgx.Tx, rows []store.ListCheckoutBookingsRow, now time.Time) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	var listingIDs []uuid.UUID
	var check []store.ListCheckoutBookingsRow
	for _, r := range rows {
		if !lateBooking(r.Booking, now) {
			continue
		}
		check = append(check, r)
		if !slices.Contains(listingIDs, r.Booking.ListingID) {
			listingIDs = append(listingIDs, r.Booking.ListingID)
		}
	}
	slices.SortFunc(listingIDs, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
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
// still runs on the visit date at the booked start and end times.
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
	i := slices.IndexFunc(slots, func(sl listing.TimeSlot) bool { return sl.ID == *r.TimeSlotID })
	if i < 0 {
		return false, nil
	}
	// The slot's times may have changed after the hold expired (the booking
	// guard no longer counted it): re-confirm only the hours that were sold
	// (the booking_attraction_details snapshot).
	return slots[i].StartTime == r.SlotStart && slots[i].EndTime == r.SlotEnd, nil
}

// confirmation is one HandlePaid call after the locks were taken.
type confirmation struct {
	s          *Service
	q          *store.Queries
	tx         pgx.Tx
	ev         payment.PaidEvent
	co         store.Checkout
	pay        payment.Payment
	pays       []payment.Payment
	bookings   []store.Booking
	rebookable map[uuid.UUID]bool
	now        time.Time
	out        payment.PaidOutcome
	// replaced: this payment is no longer the checkout's current one
	// (cancelled, or another payment is pending).
	replaced bool
}

// eligible reports whether a locked booking can be (re-)confirmed now. A
// late booking that was not checked before the locks is errStateChanged.
func (c *confirmation) eligible(b store.Booking) (bool, error) {
	ok, checked := c.rebookable[b.ID]
	switch {
	case checked:
		return ok, nil
	case lateBooking(b, c.now):
		return false, errStateChanged
	default:
		return b.Status == StatusPendingPayment, nil
	}
}

// canTakeMoney: the checkout can still be paid by this transfer (Ruling 14:
// only a paid checkout or a paid payment makes it a duplicate). A replaced
// payment additionally needs every booking still bookable (Ruling 10);
// otherwise late bookings are refunded one by one.
//
// Ruling 15: money already settled for this checkout also makes it a
// duplicate: a booking of the checkout was refunded (a booking is refunded at
// most once) or cancelled. A closed checkout by itself is not a duplicate
// (Ruling 17): after hold expiry, a late corrective payment (following a
// refunded underpayment or stray transfer) still re-confirms bookings that
// are bookable (Review Focus 2).
func (c *confirmation) canTakeMoney(ctx context.Context) (bool, error) {
	if c.co.Status == checkoutPaid {
		return false, nil
	}
	c.replaced = c.pay.Status == payment.StatusCancelled
	for _, p := range c.pays {
		if p.Status == payment.StatusPaid {
			return false, nil
		}
		if p.ID != c.pay.ID && p.Status == payment.StatusPending {
			c.replaced = true
		}
	}
	for _, b := range c.bookings {
		if b.Status == StatusCancelled {
			return false, nil
		}
	}
	refunded, err := c.s.payments.CheckoutHasBookingRefund(ctx, c.tx, c.co.ID)
	if err != nil {
		return false, err
	}
	if refunded {
		return false, nil
	}
	switch c.pay.Status {
	case payment.StatusPending, payment.StatusExpired, payment.StatusCancelled, payment.StatusNeedsReview:
	default:
		return false, nil
	}
	for _, b := range c.bookings {
		if b.Status != StatusPendingPayment && b.Status != StatusExpired {
			return false, nil
		}
		ok, err := c.eligible(b)
		if err != nil {
			return false, err
		}
		if !ok && c.replaced {
			return false, nil
		}
	}
	return true, nil
}

// duplicate refunds a transfer the checkout cannot take: nothing else
// changes.
func (c *confirmation) duplicate(ctx context.Context) (payment.PaidOutcome, error) {
	if c.pay.ProviderReference == nil {
		// A payment that cannot take its first money (e.g. a replaced one).
		if err := c.s.payments.RecordReceived(ctx, c.tx, c.pay.ID, payment.StatusNeedsReview,
			c.ev.Reference, c.ev.PaidAt, c.ev.Raw); err != nil {
			return c.out, err
		}
	}
	if c.ev.NeedsReview && c.pay.ProviderReference != nil {
		if err := c.s.payments.MarkNeedsReview(ctx, c.tx, c.pay.ID); err != nil {
			return c.out, err
		}
	}
	if err := c.paymentRefund(ctx, c.ev.AmountPaid, payment.RefundDuplicatePayment); err != nil {
		return c.out, err
	}
	c.out.Result = ResultDuplicateRefunded
	return c.out, nil
}

// underpaid: less than the amount due. Nothing is confirmed, the transfer
// goes back in full and the payment needs a human look.
func (c *confirmation) underpaid(ctx context.Context) (payment.PaidOutcome, error) {
	if err := c.s.payments.RecordReceived(ctx, c.tx, c.pay.ID, payment.StatusNeedsReview,
		c.ev.Reference, c.ev.PaidAt, c.ev.Raw); err != nil {
		return c.out, err
	}
	if err := c.paymentRefund(ctx, c.ev.AmountPaid, payment.RefundPaymentMismatch); err != nil {
		return c.out, err
	}
	c.out.Result = ResultUnderpaid
	return c.out, nil
}

// confirm handles a payment of at least the amount due.
func (c *confirmation) confirm(ctx context.Context) (payment.PaidOutcome, error) {
	var results []string
	if c.replaced {
		// Ruling 10: the replaced payment is accepted, its replacement goes.
		for _, p := range c.pays {
			if p.ID == c.pay.ID || p.Status != payment.StatusPending {
				continue
			}
			if err := c.s.payments.CancelPending(ctx, c.tx, p.ID); err != nil {
				return c.out, err
			}
			c.out.CancelOrderCodes = append(c.out.CancelOrderCodes, p.OrderCode)
		}
		results = append(results, ResultAcceptedReplaced)
	}

	var toPost []store.Booking
	for _, b := range c.bookings {
		ok, err := c.eligible(b)
		if err != nil {
			return c.out, err
		}
		switch {
		case ok && b.Status == StatusPendingPayment:
			if err := c.confirmBooking(ctx, b, nil); err != nil {
				return c.out, err
			}
			toPost = append(toPost, b)
			results = appendOnce(results, ResultConfirmed)
		case ok:
			reason := reasonLateReconfirm
			if err := c.confirmBooking(ctx, b, &reason); err != nil {
				return c.out, err
			}
			toPost = append(toPost, b)
			results = appendOnce(results, ResultReconfirmed)
		default:
			if b.Status == StatusPendingPayment {
				if err := c.expireBooking(ctx, b); err != nil {
					return c.out, err
				}
			}
			id := b.ID
			if err := c.refund(ctx, b.GrossAmount, payment.RefundLatePayment, &id, nil, b.ContactEmail); err != nil {
				return c.out, err
			}
			results = appendOnce(results, ResultLateRefunded)
		}
	}
	if err := c.postLedger(ctx, toPost); err != nil {
		return c.out, err
	}

	if len(toPost) > 0 {
		// The confirming payment is recorded: cancellations refund against it.
		if err := c.q.SetCheckoutPaid(ctx, store.SetCheckoutPaidParams{PaidPaymentID: &c.pay.ID, Now: c.now, ID: c.co.ID}); err != nil {
			return c.out, fmt.Errorf("set checkout paid: %w", err)
		}
	} else if err := c.q.SetCheckoutStatus(ctx, store.SetCheckoutStatusParams{Status: checkoutClosed, Now: c.now, ID: c.co.ID}); err != nil {
		return c.out, fmt.Errorf("set checkout closed: %w", err)
	}
	status := payment.StatusPaid
	excess := c.ev.AmountPaid - c.pay.Amount
	if excess > 0 || c.ev.NeedsReview {
		status = payment.StatusNeedsReview
	}
	if err := c.s.payments.RecordReceived(ctx, c.tx, c.pay.ID, status, c.ev.Reference, c.ev.PaidAt, c.ev.Raw); err != nil {
		return c.out, err
	}
	if excess > 0 {
		if err := c.paymentRefund(ctx, excess, payment.RefundPaymentMismatch); err != nil {
			return c.out, err
		}
		results = append(results, ResultOverpaid)
	}
	c.out.Result = strings.Join(results, "+")
	return c.out, nil
}

// postLedger posts the confirmed bookings sorted by partner_id, then
// booking id: partner_balances rows are then always locked in ascending
// order, even for a checkout spanning several partners.
func (c *confirmation) postLedger(ctx context.Context, bs []store.Booking) error {
	slices.SortFunc(bs, func(a, b store.Booking) int {
		if n := bytes.Compare(a.PartnerID[:], b.PartnerID[:]); n != 0 {
			return n
		}
		return bytes.Compare(a.ID[:], b.ID[:])
	})
	for _, b := range bs {
		if err := c.s.ledger.PostBookingConfirmed(ctx, c.tx, ledger.BookingMoney{
			BookingID: b.ID, PartnerID: b.PartnerID, ServiceType: b.ServiceType,
			Gross: b.GrossAmount, Commission: b.CommissionAmount,
		}); err != nil {
			return fmt.Errorf("post booking confirmed: %w", err)
		}
	}
	return nil
}

func (c *confirmation) history(ctx context.Context, b store.Booking, to string, reason *string) error {
	histID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	from := b.Status
	if err := c.q.InsertStatusHistory(ctx, store.InsertStatusHistoryParams{
		ID: histID, BookingID: b.ID, FromStatus: &from, ToStatus: to,
		ActorKind: actorSystem, Reason: reason, Now: c.now,
	}); err != nil {
		return fmt.Errorf("insert status history: %w", err)
	}
	return nil
}

// expireBooking expires a pending booking whose hold passed release and
// that can no longer be confirmed (Ruling 11).
func (c *confirmation) expireBooking(ctx context.Context, b store.Booking) error {
	if err := c.q.ExpireBooking(ctx, store.ExpireBookingParams{Now: c.now, ID: b.ID}); err != nil {
		return fmt.Errorf("expire booking: %w", err)
	}
	reason := reasonLateExpire
	return c.history(ctx, b, StatusExpired, &reason)
}

// confirmBooking moves a locked booking to confirmed (from pending_payment,
// or from expired with lateReason), writes its history and emails the
// customer. The ledger posting follows in postLedger.
func (c *confirmation) confirmBooking(ctx context.Context, b store.Booking, lateReason *string) error {
	if err := c.q.ConfirmBooking(ctx, store.ConfirmBookingParams{Now: c.now, ID: b.ID}); err != nil {
		return fmt.Errorf("confirm booking: %w", err)
	}
	if err := c.history(ctx, b, StatusConfirmed, lateReason); err != nil {
		return err
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
		strconv.FormatInt(c.ev.OrderCode, 10) + ". " + refundExplanation(reason, c.co.Status == checkoutPaid) + "\n" +
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

// refundExplanation: alreadyPaid is whether the checkout was already paid
// before this transfer (only then is a duplicate truly "paid before").
func refundExplanation(reason string, alreadyPaid bool) string {
	switch reason {
	case payment.RefundLatePayment:
		return "Tiền về sau khi hết thời gian giữ chỗ và chỗ không còn đặt được, nên đơn không thể xác nhận."
	case payment.RefundDuplicatePayment:
		if alreadyPaid {
			return "Đơn này đã được thanh toán trước đó, khoản chuyển này bị trùng."
		}
		return "Khoản chuyển này sẽ được hoàn lại; vui lòng liên hệ hỗ trợ để hoàn tất đơn."
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
