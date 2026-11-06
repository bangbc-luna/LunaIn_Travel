package booking

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

// Request Booking lifecycle shared by every service type sold on request
// (spec §6.2): the booking is created requested, the partner accepts or
// rejects it before response_due_at, the customer pays before
// payment_due_at. What is specific to a service type (its capacity) lives in
// capacity.go.

// Request Booking statuses and mode.
const (
	StatusRequested       = "requested"
	StatusAwaitingPayment = "awaiting_payment"
	StatusInService       = "in_service"
	StatusRejected        = "rejected"

	bookingModeRequest = "request"
)

// PartnerContact finds where partner emails are sent (*partner.Service).
type PartnerContact interface {
	OwnerEmail(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID) (string, error)
}

// requestWithdrawable reports whether the customer may still withdraw a
// Request Booking for free: it is requested or accepted but not yet paid.
func requestWithdrawable(b store.Booking) bool {
	return b.BookingMode == bookingModeRequest && (b.Status == StatusRequested || b.Status == StatusAwaitingPayment)
}

// requestResponseDue is the deadline for the partner to answer a request
// made at now for a service starting at serviceStart: request_response_hours
// after now, but no later than request_payment_hours before the start, so an
// accepted request always leaves the customer the full time to pay.
func requestResponseDue(cfg settings.Settings, now, serviceStart time.Time) time.Time {
	due := now.Add(time.Duration(cfg.RequestResponseHours) * time.Hour)
	if latest := serviceStart.Add(-time.Duration(cfg.RequestPaymentHours) * time.Hour); latest.Before(due) {
		return latest
	}
	return due
}

// insertRequestedBooking inserts a requested Request Booking (no hold
// columns; p.ResponseDueAt set) with a fresh id and LT code (p.ID and p.Code
// are set here) and returns both, retrying on a code collision (ON CONFLICT
// DO NOTHING keeps the tx usable).
func insertRequestedBooking(ctx context.Context, q *store.Queries, p store.InsertRequestedBookingParams) (uuid.UUID, string, error) {
	if p.ResponseDueAt == nil {
		return uuid.Nil, "", errors.New("booking: a requested booking needs response_due_at")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, "", err
	}
	p.ID = id
	for range codeInsertTries {
		if p.Code, err = randomCode("LT"); err != nil {
			return uuid.Nil, "", err
		}
		n, err := q.InsertRequestedBooking(ctx, p)
		if err != nil {
			return uuid.Nil, "", fmt.Errorf("insert requested booking: %w", err)
		}
		if n == 1 {
			return id, p.Code, nil
		}
	}
	return uuid.Nil, "", errors.New("booking code: too many collisions")
}

// emailPartner queues an email to the partner's owner in tx (outbox). Without
// a PartnerContact (tests) it is only logged.
func (s *Service) emailPartner(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, subject, text string) error {
	if s.partners == nil {
		s.log.Info("booking: partner email not queued (no partner contact)", "partner_id", partnerID, "subject", subject)
		return nil
	}
	to, err := s.partners.OwnerEmail(ctx, tx, partnerID)
	if err != nil {
		return err
	}
	return s.enqueueEmail(ctx, tx, mailer.Message{To: to, Subject: subject, Text: text})
}

const (
	cancelKindCustomerWithdraw = "customer_withdraw"
	maxRejectReason            = 500

	// reasonRequestExpired: history reason of the request expiry job.
	reasonRequestExpired = "đối tác không phản hồi đúng hạn"
)

var (
	ErrRequestNotPending = httpx.NewError(http.StatusConflict, "request_not_pending", "Yêu cầu này không còn chờ đối tác phản hồi")
	ErrRequestExpired    = httpx.NewError(http.StatusConflict, "request_expired", "Đã quá hạn phản hồi yêu cầu")
	ErrInvalidReject     = httpx.NewError(http.StatusUnprocessableEntity, "invalid_reject",
		fmt.Sprintf("Lý do từ chối phải có từ 1 đến %d ký tự", maxRejectReason))

	// errWithdrawStateChanged: the request was paid (or ended) between the
	// unlocked read and the locks; a paid booking is cancelled by policy.
	errWithdrawStateChanged = httpx.NewError(http.StatusConflict, ErrInvalidBookingState.Code,
		"Đơn đặt chỗ vừa thay đổi trạng thái, vui lòng tải lại đơn")
)

// bookingEnded: the booking will never be paid nor served.
func bookingEnded(status string) bool {
	return status == StatusExpired || status == StatusCancelled || status == StatusRejected
}

// partnerRequest reads, unlocked, a Request Booking of the partner (another
// partner's booking or an unknown id is ErrBookingNotFound; an Instant
// Booking is ErrRequestNotPending).
func (s *Service) partnerRequest(ctx context.Context, partnerID, bookingID uuid.UUID) (store.Booking, error) {
	b, err := store.New(s.pool).GetBooking(ctx, bookingID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && b.PartnerID != partnerID) {
		return b, ErrBookingNotFound
	}
	if err != nil {
		return b, fmt.Errorf("get booking: %w", err)
	}
	if b.BookingMode != bookingModeRequest {
		return b, ErrRequestNotPending
	}
	return b, nil
}

// lockRequest takes, in tx, the capacity rows of the checkout, the checkout
// and all its bookings FOR UPDATE (DB §1.1) and returns the checkout, its
// bookings and the index of bookingID among them.
func (s *Service) lockRequest(ctx context.Context, tx pgx.Tx, checkoutID, bookingID uuid.UUID) (store.Checkout, []store.Booking, int, error) {
	q := store.New(tx)
	if err := s.lockCheckoutCapacity(ctx, tx, checkoutID); err != nil {
		return store.Checkout{}, nil, 0, err
	}
	co, err := q.LockCheckout(ctx, checkoutID)
	if err != nil {
		return co, nil, 0, fmt.Errorf("lock checkout: %w", err)
	}
	bookings, err := q.LockCheckoutBookings(ctx, checkoutID)
	if err != nil {
		return co, nil, 0, fmt.Errorf("lock checkout bookings: %w", err)
	}
	i := slices.IndexFunc(bookings, func(b store.Booking) bool { return b.ID == bookingID })
	if i < 0 {
		return co, nil, 0, fmt.Errorf("booking: %s not in its checkout %s", bookingID, checkoutID)
	}
	return co, bookings, i, nil
}

// closeIfEnded closes the open checkout once every booking of it ended.
func closeIfEnded(ctx context.Context, q *store.Queries, co store.Checkout, bookings []store.Booking, now time.Time) error {
	if co.Status != checkoutOpen {
		return nil
	}
	for _, b := range bookings {
		if !bookingEnded(b.Status) {
			return nil
		}
	}
	if err := q.SetCheckoutStatus(ctx, store.SetCheckoutStatusParams{Status: checkoutClosed, Now: now, ID: co.ID}); err != nil {
		return fmt.Errorf("close checkout: %w", err)
	}
	return nil
}

// AcceptRequest accepts a requested booking of partnerID as userID. In one
// tx, after the capacity rows, the checkout and its bookings are locked
// (DB §1.1), the booking must still be requested (ErrRequestNotPending) and
// now before response_due_at (ErrRequestExpired: from then on the request no
// longer holds its capacity, even if ExpireRequests has not run yet). The
// booking becomes awaiting_payment with payment_due_at = hold_expires_at =
// now + request_payment_hours and hold_release_at = payment_due_at +
// hold_grace_minutes; its capacity stays held until hold_release_at (it is
// held since the request, so availability is not checked again); a pending
// payment of the checkout total expiring at payment_due_at is reserved; the
// history (actor partner) and the customer email are written.
//
// A suspended partner may still answer the requests already waiting (spec
// §5.2 [Chốt]): neither the partner status nor the listing visibility is
// checked.
//
// After commit the payment link is created; a provider failure is only
// logged (POST /pay creates it again).
func (s *Service) AcceptRequest(ctx context.Context, partnerID, userID, bookingID uuid.UUID) (BookingView, error) {
	b, err := s.partnerRequest(ctx, partnerID, bookingID)
	if err != nil {
		return BookingView{}, err
	}
	var paymentID uuid.UUID
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		co, bookings, i, err := s.lockRequest(ctx, tx, b.CheckoutID, bookingID)
		if err != nil {
			return err
		}
		b := bookings[i]
		now := s.clock.Now()
		if b.Status != StatusRequested {
			return ErrRequestNotPending
		}
		if b.ResponseDueAt == nil || !now.Before(*b.ResponseDueAt) {
			return ErrRequestExpired
		}
		cfg, err := s.settings.GetTx(ctx, tx)
		if err != nil {
			return err
		}
		due := now.Add(time.Duration(cfg.RequestPaymentHours) * time.Hour)
		release := due.Add(time.Duration(cfg.HoldGraceMinutes) * time.Minute)
		if err := q.AcceptRequest(ctx, store.AcceptRequestParams{
			Now: now, PaymentDueAt: due, HoldReleaseAt: release, ID: b.ID,
		}); err != nil {
			return fmt.Errorf("accept request: %w", err)
		}
		if err := s.keepHoldUntil(ctx, tx, b, release); err != nil {
			return err
		}
		pay, err := s.payments.ReservePayment(ctx, tx, co.ID, co.TotalAmount, due)
		if err != nil {
			return err
		}
		paymentID = pay.ID
		if err := insertHistory(ctx, q, b, StatusAwaitingPayment, actorPartner, &userID, nil, now); err != nil {
			return err
		}
		return s.enqueueEmail(ctx, tx, mailer.Message{
			To:      b.ContactEmail,
			Subject: "Yêu cầu đặt chỗ " + b.Code + " đã được chấp nhận",
			Text: "Xin chào " + b.ContactName + ",\n\n" +
				"Đối tác đã xác nhận yêu cầu đặt chỗ " + b.Code + " của bạn.\n" +
				"Vui lòng thanh toán " + formatVND(co.TotalAmount) + " trước " + vnTime(due) + ".\n" +
				"Quá hạn mà chưa thanh toán, yêu cầu sẽ tự hủy.\n",
		})
	})
	if err != nil {
		return BookingView{}, err
	}
	// Outside the tx: never call the provider with a tx open.
	p, err := s.payments.ActivateLink(context.WithoutCancel(ctx), paymentID)
	switch {
	case err != nil:
		s.log.Warn("booking: payment link not created on accept", "booking_id", bookingID, "err", err)
	case p.Status != payment.StatusPending:
		// The customer withdrew while the link was being created: the
		// withdraw's own cancel may have run before the link existed.
		s.cancelLink(ctx, p.OrderCode)
	}
	return s.GetForPartner(ctx, partnerID, bookingID)
}

// RejectRequest rejects a requested booking of partnerID as userID with
// reason (trimmed, 1..500 characters, no control character, else
// ErrInvalidReject). Under the locks of AcceptRequest the booking must still
// be requested (ErrRequestNotPending; a request past response_due_at that
// ExpireRequests has not swept can still be rejected). It becomes rejected,
// its capacity is released, the checkout is closed once every booking ended,
// the history (actor partner, reason) and the customer email are written.
// Nothing was paid: no money moves.
func (s *Service) RejectRequest(ctx context.Context, partnerID, userID, bookingID uuid.UUID, reason string) (BookingView, error) {
	b, err := s.partnerRequest(ctx, partnerID, bookingID)
	if err != nil {
		return BookingView{}, err
	}
	reason = strings.TrimSpace(reason)
	if n := utf8.RuneCountInString(reason); n < 1 || n > maxRejectReason || strings.ContainsFunc(reason, unicode.IsControl) {
		return BookingView{}, ErrInvalidReject
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		co, bookings, i, err := s.lockRequest(ctx, tx, b.CheckoutID, bookingID)
		if err != nil {
			return err
		}
		b := bookings[i]
		if b.Status != StatusRequested {
			return ErrRequestNotPending
		}
		now := s.clock.Now()
		if err := q.RejectRequest(ctx, store.RejectRequestParams{Now: now, RejectReason: reason, ID: b.ID}); err != nil {
			return fmt.Errorf("reject request: %w", err)
		}
		if err := insertHistory(ctx, q, b, StatusRejected, actorPartner, &userID, &reason, now); err != nil {
			return err
		}
		if err := s.releaseHold(ctx, tx, b); err != nil {
			return err
		}
		bookings[i].Status = StatusRejected
		if err := closeIfEnded(ctx, q, co, bookings, now); err != nil {
			return err
		}
		return s.enqueueEmail(ctx, tx, mailer.Message{
			To:      b.ContactEmail,
			Subject: "Yêu cầu đặt chỗ " + b.Code + " đã bị từ chối",
			Text: "Xin chào " + b.ContactName + ",\n\n" +
				"Rất tiếc, đối tác đã từ chối yêu cầu đặt chỗ " + b.Code + " của bạn.\n" +
				"Lý do: " + reason + "\n" +
				"Bạn chưa thanh toán nên không phát sinh khoản phí nào.\n",
		})
	})
	if err != nil {
		return BookingView{}, err
	}
	return s.GetForPartner(ctx, partnerID, bookingID)
}

// withdrawRequest is CancelByCustomer of an unpaid Request Booking
// (requested or awaiting_payment as read unlocked by the caller, which
// checked the customer owns it). Under the locks of AcceptRequest the booking
// must still be withdrawable (else 409 invalid_booking_state: it was just
// paid, and is then cancelled by policy). It becomes cancelled
// (customer_withdraw, by the customer), its capacity is released, every
// pending payment of the checkout is cancelled, the checkout closed once
// every booking ended, the history and the partner email written. Nothing
// was paid: no refund, no ledger posting. After commit the cancelled
// payments' links are cancelled at the provider; money arriving on them
// later is refunded in full (the booking is cancelled).
func (s *Service) withdrawRequest(ctx context.Context, customerID, checkoutID, bookingID uuid.UUID) error {
	var codes []int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		co, bookings, i, err := s.lockRequest(ctx, tx, checkoutID, bookingID)
		if err != nil {
			return err
		}
		b := bookings[i]
		if !requestWithdrawable(b) {
			return errWithdrawStateChanged
		}
		now := s.clock.Now()
		if err := q.CancelBooking(ctx, store.CancelBookingParams{
			Now: now, CancelKind: cancelKindCustomerWithdraw, CancelledBy: &customerID, ID: b.ID,
		}); err != nil {
			return fmt.Errorf("cancel booking: %w", err)
		}
		if err := insertHistory(ctx, q, b, StatusCancelled, actorCustomer, &customerID, nil, now); err != nil {
			return err
		}
		if err := s.releaseHold(ctx, tx, b); err != nil {
			return err
		}
		bookings[i].Status = StatusCancelled
		pays, err := s.payments.LockCheckoutPayments(ctx, tx, co.ID)
		if err != nil {
			return err
		}
		for _, p := range pays {
			if p.Status != payment.StatusPending {
				continue
			}
			if err := s.payments.CancelPending(ctx, tx, p.ID); err != nil {
				return err
			}
			codes = append(codes, p.OrderCode)
		}
		if err := closeIfEnded(ctx, q, co, bookings, now); err != nil {
			return err
		}
		return s.emailPartner(ctx, tx, b.PartnerID, "Khách đã rút yêu cầu đặt chỗ "+b.Code,
			"Xin chào,\n\n"+
				"Khách hàng đã rút yêu cầu đặt chỗ "+b.Code+" (sử dụng lúc "+vnTime(b.ServiceStartAt)+").\n"+
				"Bạn không cần làm gì thêm; chỗ/xe của đơn đã được trả lại.\n")
	})
	if err != nil {
		return err
	}
	for _, oc := range codes {
		s.cancelLink(ctx, oc)
	}
	return nil
}

// ExpireRequestsArgs is the periodic request expiry job.
type ExpireRequestsArgs struct{}

func (ExpireRequestsArgs) Kind() string { return "booking_expire_requests" }

// ExpireRequestsWorker runs Service.ExpireRequests.
type ExpireRequestsWorker struct {
	river.WorkerDefaults[ExpireRequestsArgs]
	Service *Service
}

func (w *ExpireRequestsWorker) Work(ctx context.Context, _ *river.Job[ExpireRequestsArgs]) error {
	_, err := w.Service.ExpireRequests(ctx)
	return err
}

// ExpireRequests expires the requests the partner did not answer by
// response_due_at (<= now) and returns how many it expired. Candidates are
// read unlocked; each checkout is handled in its own tx under the locks of
// AcceptRequest, so it serializes with a concurrent accept, reject or
// withdraw, and re-checked: each still-requested booking past its deadline
// becomes expired (history "đối tác không phản hồi đúng hạn", actor system),
// releases its capacity and its customer is emailed; the checkout is closed
// once every booking ended. A failing checkout backs off (requestBackoff).
func (s *Service) ExpireRequests(ctx context.Context) (int, error) {
	now := s.clock.Now()
	rows, err := store.New(s.pool).ListRequestsToExpire(ctx, store.ListRequestsToExpireParams{
		Now: now, Lim: s.batch(), Skip: s.requestBackoff.skip(now),
	})
	if err != nil {
		return 0, fmt.Errorf("list requests to expire: %w", err)
	}
	var checkouts []uuid.UUID
	for _, r := range rows {
		if !slices.Contains(checkouts, r.CheckoutID) {
			checkouts = append(checkouts, r.CheckoutID)
		}
	}
	total := 0
	for _, id := range checkouts {
		var n int
		err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			if s.beforeItem != nil {
				if err := s.beforeItem(id); err != nil {
					return err
				}
			}
			var err error
			n, err = s.expireRequestCheckout(ctx, tx, id, now)
			return err
		})
		if err != nil {
			s.itemFailed(&s.requestBackoff, "booking: expire requests failed", "checkout_id", id, now, err)
			continue
		}
		s.requestBackoff.ok(id)
		total += n
	}
	return total, nil
}

// expireRequestCheckout is one checkout of ExpireRequests.
func (s *Service) expireRequestCheckout(ctx context.Context, tx pgx.Tx, checkoutID uuid.UUID, now time.Time) (int, error) {
	q := store.New(tx)
	if err := s.lockCheckoutCapacity(ctx, tx, checkoutID); err != nil {
		return 0, err
	}
	co, err := q.LockCheckout(ctx, checkoutID)
	if err != nil {
		return 0, fmt.Errorf("lock checkout: %w", err)
	}
	bookings, err := q.LockCheckoutBookings(ctx, checkoutID)
	if err != nil {
		return 0, fmt.Errorf("lock checkout bookings: %w", err)
	}
	reason := reasonRequestExpired
	n := 0
	for i, b := range bookings {
		if b.Status != StatusRequested || b.ResponseDueAt == nil || b.ResponseDueAt.After(now) {
			continue
		}
		if err := q.ExpireBooking(ctx, store.ExpireBookingParams{Now: now, ID: b.ID}); err != nil {
			return 0, fmt.Errorf("expire booking: %w", err)
		}
		if err := insertHistory(ctx, q, b, StatusExpired, actorSystem, nil, &reason, now); err != nil {
			return 0, err
		}
		if err := s.releaseHold(ctx, tx, b); err != nil {
			return 0, err
		}
		bookings[i].Status = StatusExpired
		if err := s.enqueueEmail(ctx, tx, mailer.Message{
			To:      b.ContactEmail,
			Subject: "Yêu cầu đặt chỗ " + b.Code + " đã hết hạn",
			Text: "Xin chào " + b.ContactName + ",\n\n" +
				"Đối tác không phản hồi yêu cầu đặt chỗ " + b.Code + " trước " + vnTime(*b.ResponseDueAt) +
				" nên yêu cầu đã hết hạn.\n" +
				"Bạn chưa thanh toán nên không phát sinh khoản phí nào. Bạn có thể gửi yêu cầu khác.\n",
		}); err != nil {
			return 0, err
		}
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, closeIfEnded(ctx, q, co, bookings, now)
}
