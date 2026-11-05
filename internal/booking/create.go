// Package booking owns checkouts, bookings and their lifecycle (creation,
// payment link, views; confirmation, check-in, cancellation and jobs in
// later files).
package booking

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/money"
	"github.com/bangbc-luna/lunain-travel/internal/platform/validate"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

// Booking statuses and other column values.
const (
	StatusPendingPayment = "pending_payment"
	StatusConfirmed      = "confirmed"
	StatusCompleted      = "completed"
	StatusCheckedIn      = "checked_in"

	serviceTypeAttraction = "attraction_ticket"
	lineKindTicket        = "ticket"
	actorCustomer         = "customer"

	dateLayout = "2006-01-02"

	maxTicketLines     = 10
	maxLineQuantity    = 50
	maxTotalTickets    = 100
	maxContactName     = 100
	maxNoteRunes       = 1000
	maxIdemKeyLen      = 255
	codeInsertTries    = 5
	endpointAttraction = "POST /v1/bookings/attraction"
	endpointRide       = "POST /v1/bookings/ride"
)

var (
	ErrIdempotencyKeyRequired = httpx.NewError(http.StatusBadRequest, "idempotency_key_required", "Thiếu hoặc sai header Idempotency-Key")
	ErrIdempotencyKeyReused   = httpx.NewError(http.StatusUnprocessableEntity, "idempotency_key_reused", "Idempotency-Key này đã được dùng cho một yêu cầu khác")
	ErrInvalidBooking         = httpx.NewError(http.StatusUnprocessableEntity, "invalid_booking", "Thông tin đặt chỗ không hợp lệ")
	ErrVisitDatePast          = httpx.NewError(http.StatusUnprocessableEntity, "visit_date_past", "Ngày tham quan đã qua")
	ErrSlotUnavailable        = httpx.NewError(http.StatusUnprocessableEntity, "slot_unavailable", "Khung giờ này không mở cửa vào ngày đã chọn")
	ErrSlotStarted            = httpx.NewError(http.StatusUnprocessableEntity, "slot_started", "Khung giờ này đã bắt đầu, vui lòng chọn khung giờ khác")
	ErrBookingNotFound        = httpx.NewError(http.StatusNotFound, "not_found", "Không tìm thấy đơn đặt chỗ")
	ErrBookingNotPayable      = httpx.NewError(http.StatusConflict, "booking_not_payable", "Đơn đặt chỗ này không ở trạng thái chờ thanh toán")
	ErrHoldExpired            = httpx.NewError(http.StatusConflict, "hold_expired", "Đã hết thời gian giữ chỗ cho đơn này")
	// errIdempotencyInFlight: a stored key without response (cannot happen:
	// the key and its response are written in the same tx).
	errIdempotencyInFlight = httpx.NewError(http.StatusConflict, "idempotency_in_progress", "Yêu cầu với Idempotency-Key này đang được xử lý")
)

func invalidBooking(msg string) error {
	return httpx.NewError(ErrInvalidBooking.Status, ErrInvalidBooking.Code, msg)
}

// Deps are the collaborators of Service.
type Deps struct {
	Pool     *pgxpool.Pool
	Clock    clock.Clock
	Listings *listing.Service
	Catalog  *catalog.Service
	Settings *settings.Service
	Identity *identity.Service
	Payments *payment.Service
	Ledger   *ledger.Service
	// Jobs queues customer emails (outbox). May be nil in tests that send
	// no email: emails are then only logged.
	Jobs *river.Client[pgx.Tx]
	Log  *slog.Logger
}

// Service implements the booking use cases.
type Service struct {
	pool     *pgxpool.Pool
	clock    clock.Clock
	listings *listing.Service
	catalog  *catalog.Service
	settings *settings.Service
	identity *identity.Service
	payments *payment.Service
	ledger   *ledger.Service
	jobs     *river.Client[pgx.Tx]
	log      *slog.Logger

	// Periodic jobs: rows backing off after a failure, one tracker per job.
	holdBackoff, noShowBackoff, completeBackoff, releaseBackoff jobBackoff
	// batchSize overrides jobBatchSize when > 0 (tests).
	batchSize int32
	// beforeItem, when set (tests), runs inside each job item's transaction
	// before any work; an error fails the item.
	beforeItem func(id uuid.UUID) error
}

// NewService builds a Service.
func NewService(d Deps) *Service {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		pool: d.Pool, clock: d.Clock, listings: d.Listings, catalog: d.Catalog, settings: d.Settings,
		identity: d.Identity, payments: d.Payments, ledger: d.Ledger, jobs: d.Jobs, log: log,
	}
}

// TicketLine is one ticket type and how many of it.
type TicketLine struct {
	TicketTypeID uuid.UUID `json:"ticket_type_id"`
	Quantity     int       `json:"quantity"`
}

// CreateAttractionInput is the POST /v1/bookings/attraction body.
type CreateAttractionInput struct {
	ListingID    uuid.UUID    `json:"listing_id"`
	VisitDate    string       `json:"visit_date"`
	TimeSlotID   uuid.UUID    `json:"time_slot_id"`
	Items        []TicketLine `json:"items"`
	ContactName  string       `json:"contact_name"`
	ContactPhone string       `json:"contact_phone"`
	ContactEmail string       `json:"contact_email"`
	Note         *string      `json:"note"`
}

// attractionRequest is a validated, normalized CreateAttractionInput.
type attractionRequest struct {
	in                CreateAttractionInput
	visitDate         time.Time // 00:00 of the visit date in clock.VN
	name, phone, mail string
	note              *string
	ticketIDs         []uuid.UUID
}

func (in CreateAttractionInput) normalize() (attractionRequest, error) {
	r := attractionRequest{in: in}
	d, err := time.ParseInLocation(dateLayout, in.VisitDate, clock.VN)
	if err != nil {
		return r, invalidBooking("visit_date phải có dạng YYYY-MM-DD")
	}
	r.visitDate = d
	if len(in.Items) < 1 || len(in.Items) > maxTicketLines {
		return r, invalidBooking(fmt.Sprintf("Cần từ 1 đến %d loại vé", maxTicketLines))
	}
	total := 0
	seen := make(map[uuid.UUID]bool, len(in.Items))
	for _, it := range in.Items {
		if it.Quantity < 1 || it.Quantity > maxLineQuantity {
			return r, invalidBooking(fmt.Sprintf("Số lượng mỗi loại vé phải từ 1 đến %d", maxLineQuantity))
		}
		if seen[it.TicketTypeID] {
			return r, invalidBooking("Mỗi loại vé chỉ được xuất hiện một lần")
		}
		seen[it.TicketTypeID] = true
		total += it.Quantity
		r.ticketIDs = append(r.ticketIDs, it.TicketTypeID)
	}
	if total > maxTotalTickets {
		return r, invalidBooking(fmt.Sprintf("Tổng số vé tối đa là %d", maxTotalTickets))
	}
	c, err := normalizeContact(in.ContactName, in.ContactPhone, in.ContactEmail, in.Note)
	if err != nil {
		return r, err
	}
	r.name, r.phone, r.mail, r.note = c.name, c.phone, c.mail, c.note
	return r, nil
}

// contact is a validated, normalized booking contact and note.
type contact struct {
	name, phone, mail string
	note              *string
}

// normalizeContact validates and normalizes the contact fields shared by
// every booking request: a name of 1..100 characters without control
// characters, a Vietnamese phone, an email and an optional note (blank is
// none).
func normalizeContact(name, phone, mail string, note *string) (contact, error) {
	var c contact
	c.name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(c.name); n < 1 || n > maxContactName || strings.ContainsFunc(c.name, unicode.IsControl) {
		return c, invalidBooking(fmt.Sprintf("Tên liên hệ phải có từ 1 đến %d ký tự", maxContactName))
	}
	p, ok := validate.NormalizePhone(phone)
	if !ok {
		return c, invalidBooking("Số điện thoại liên hệ không hợp lệ")
	}
	c.phone = p
	m, err := identity.NormalizeEmail(mail)
	if err != nil {
		return c, invalidBooking("Email liên hệ không hợp lệ")
	}
	c.mail = m
	if note != nil {
		n := strings.TrimSpace(*note)
		if utf8.RuneCountInString(n) > maxNoteRunes {
			return c, invalidBooking(fmt.Sprintf("Ghi chú tối đa %d ký tự", maxNoteRunes))
		}
		if n != "" {
			c.note = &n
		}
	}
	return c, nil
}

// requestHash identifies the request body of an Idempotency-Key. The input
// structs marshal deterministically (fixed field order).
func requestHash(endpoint string, in any) ([]byte, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	h := sha256.New()
	h.Write([]byte(endpoint))
	h.Write([]byte{0})
	h.Write(b)
	return h.Sum(nil), nil
}

func validIdemKey(k string) bool {
	if strings.TrimSpace(k) == "" || len(k) > maxIdemKeyLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// randomCode is prefix + 8 Crockford base32 characters from crypto/rand
// (256 is a multiple of 32, so b%32 is unbiased).
func randomCode(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random code: %w", err)
	}
	out := make([]byte, 0, len(prefix)+len(b))
	out = append(out, prefix...)
	for _, c := range b {
		out = append(out, crockfordAlphabet[c%32])
	}
	return string(out), nil
}

// randomToken is 32 random bytes, base64url (the booking QR token).
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func audienceLabel(a string) string {
	if a == "child" {
		return "Trẻ em"
	}
	return "Người lớn"
}

// createdRef is the response body stored with an Idempotency-Key.
type createdRef struct {
	ID   uuid.UUID `json:"id"`
	Code string    `json:"code"`
}

// CreateAttraction books attraction tickets for customerID: one checkout,
// one pending_payment booking holding the slot, and one pending payment, all
// in one tx; the PayOS link is created after commit (a provider failure
// still returns the booking, with payment.checkout_url null; the customer
// retries with Pay).
//
// Idempotency: (customer, idemKey) is reserved in the same tx. The same key
// with the same body returns the booking created first (re-read now, so the
// payment link and status are current); with another body it is
// ErrIdempotencyKeyReused. A request that failed leaves no key behind.
//
// Lock order (DB §1.1): the customer's own rows (idempotency key, profile)
// first, then BookableListing (partners, listings FOR SHARE), then the new
// checkout, booking and its children, then the payment.
func (s *Service) CreateAttraction(ctx context.Context, customerID uuid.UUID, idemKey string, in CreateAttractionInput) (BookingView, error) {
	if !validIdemKey(idemKey) {
		return BookingView{}, ErrIdempotencyKeyRequired
	}
	hash, err := requestHash(endpointAttraction, in)
	if err != nil {
		return BookingView{}, err
	}
	req, err := in.normalize()
	if err != nil {
		return BookingView{}, err
	}
	now := s.clock.Now()

	var bookingID, paymentID uuid.UUID
	replay := false
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.ReserveIdempotencyKey(ctx, store.ReserveIdempotencyKeyParams{
			UserID: customerID, Key: idemKey, Endpoint: endpointAttraction, RequestHash: hash, Now: now,
		})
		if err != nil {
			return fmt.Errorf("reserve idempotency key: %w", err)
		}
		if n == 0 {
			body, err := replayed(ctx, q, customerID, idemKey, endpointAttraction, hash)
			if err != nil {
				return err
			}
			var ref createdRef
			if err := json.Unmarshal(body, &ref); err != nil {
				return fmt.Errorf("decode idempotent response: %w", err)
			}
			bookingID, replay = ref.ID, true
			return nil
		}
		if err := s.identity.FillProfileIfEmpty(ctx, tx, customerID, req.name, req.phone); err != nil {
			return fmt.Errorf("fill profile: %w", err)
		}
		ref, payID, err := s.createAttractionTx(ctx, tx, customerID, req, now)
		if err != nil {
			return err
		}
		bookingID, paymentID = ref.ID, payID
		return storeIdempotentResponse(ctx, q, customerID, idemKey, ref, now)
	})
	if err != nil {
		return BookingView{}, err
	}
	// Outside the tx: never call the provider with a tx open. A failure
	// leaves the payment pending without URL (POST /pay or a replay retries).
	if replay {
		s.activateMissingLink(ctx, bookingID)
	} else if _, err := s.payments.ActivateLink(context.WithoutCancel(ctx), paymentID); err != nil {
		s.log.Warn("booking: payment link not created", "booking_id", bookingID, "err", err)
	}
	return s.GetMine(ctx, customerID, bookingID)
}

// activateMissingLink retries the payment link of a replayed booking that is
// still pending_payment and whose pending payment has no link yet (best
// effort: the replay answers with the booking either way).
func (s *Service) activateMissingLink(ctx context.Context, bookingID uuid.UUID) {
	b, err := store.New(s.pool).GetBooking(ctx, bookingID)
	if err != nil || b.Status != StatusPendingPayment {
		return
	}
	s.activateCheckoutLink(ctx, b.CheckoutID)
}

// activateCheckoutLink creates the link of a checkout's pending payment that
// has none yet (best effort, outside any tx).
func (s *Service) activateCheckoutLink(ctx context.Context, checkoutID uuid.UUID) {
	p, err := s.payments.PendingForCheckout(ctx, checkoutID)
	if err != nil || p == nil || p.CheckoutURL != nil {
		return
	}
	if _, err := s.payments.ActivateLink(context.WithoutCancel(ctx), p.ID); err != nil {
		s.log.Warn("booking: payment link not created on replay", "checkout_id", checkoutID, "err", err)
	}
}

// replayed resolves an already used Idempotency-Key of endpoint to the
// response body stored with it. A key used on another endpoint or with
// another body is ErrIdempotencyKeyReused.
func replayed(ctx context.Context, q *store.Queries, customerID uuid.UUID, key, endpoint string, hash []byte) (json.RawMessage, error) {
	row, err := q.GetIdempotencyKey(ctx, store.GetIdempotencyKeyParams{UserID: customerID, Key: key})
	if err != nil {
		return nil, fmt.Errorf("get idempotency key: %w", err)
	}
	if row.Endpoint != endpoint || string(row.RequestHash) != string(hash) {
		return nil, ErrIdempotencyKeyReused
	}
	if row.ResponseBody == nil {
		return nil, errIdempotencyInFlight
	}
	return json.RawMessage(row.ResponseBody), nil
}

// storeIdempotentResponse stores the 201 response body of a reserved key.
// Only a successful creation is stored: a failed request rolls the key back
// with everything else, so the client may simply retry.
func storeIdempotentResponse(ctx context.Context, q *store.Queries, customerID uuid.UUID, key string, ref any, now time.Time) error {
	body, err := json.Marshal(ref)
	if err != nil {
		return fmt.Errorf("marshal idempotent response: %w", err)
	}
	status := int32(http.StatusCreated)
	if err := q.StoreIdempotencyResponse(ctx, store.StoreIdempotencyResponseParams{
		ResponseStatus: &status, ResponseBody: body, Now: now, UserID: customerID, Key: key,
	}); err != nil {
		return fmt.Errorf("store idempotency response: %w", err)
	}
	return nil
}

func startOfDayVN(t time.Time) time.Time {
	y, m, d := t.In(clock.VN).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, clock.VN)
}

// attractionServiceEnd is the service_end_at of an attraction ticket for
// visitDate: 00:00 (Vietnam) of the next day, end-exclusive. A ticket is
// usable the whole visit day, not only within its slot (spec §6.4: no-show
// only once the visit date has passed); the slot's hours are kept in
// booking_attraction_details.
func attractionServiceEnd(visitDate time.Time) time.Time {
	return startOfDayVN(visitDate).AddDate(0, 0, 1)
}

// createAttractionTx does the in-tx part of CreateAttraction after the
// idempotency key was reserved; it returns the booking and the payment id.
func (s *Service) createAttractionTx(ctx context.Context, tx pgx.Tx, customerID uuid.UUID, req attractionRequest, now time.Time) (createdRef, uuid.UUID, error) {
	in := req.in
	// Checked after the key reservation so a replay is answered even after
	// the visit date passed.
	if req.visitDate.Before(startOfDayVN(now)) {
		return createdRef{}, uuid.Nil, ErrVisitDatePast
	}
	l, err := s.listings.BookableListing(ctx, tx, in.ListingID)
	if err != nil {
		return createdRef{}, uuid.Nil, err
	}
	if l.ServiceType != serviceTypeAttraction {
		return createdRef{}, uuid.Nil, listing.ErrWrongServiceType
	}
	slots, err := s.listings.SlotsForDate(ctx, tx, l.ID, req.visitDate)
	if err != nil {
		return createdRef{}, uuid.Nil, err
	}
	var slot *listing.TimeSlot
	for i := range slots {
		if slots[i].ID == in.TimeSlotID {
			slot = &slots[i]
		}
	}
	if slot == nil {
		return createdRef{}, uuid.Nil, ErrSlotUnavailable
	}
	startAt, err := slot.StartAt(req.visitDate)
	if err != nil {
		return createdRef{}, uuid.Nil, err
	}
	endAt := attractionServiceEnd(req.visitDate)
	if !startAt.After(now) {
		return createdRef{}, uuid.Nil, ErrSlotStarted
	}
	types, err := s.listings.TicketTypesForBooking(ctx, tx, l.ID, req.ticketIDs)
	if err != nil {
		return createdRef{}, uuid.Nil, err
	}
	rateBps, err := s.catalog.CurrentCommissionRate(ctx, tx, l.ServiceType, now)
	if err != nil {
		return createdRef{}, uuid.Nil, err
	}
	cfg, err := s.settings.GetTx(ctx, tx)
	if err != nil {
		return createdRef{}, uuid.Nil, err
	}
	policy, err := s.listings.CancellationPolicy(ctx, tx, l.ID)
	if err != nil {
		return createdRef{}, uuid.Nil, err
	}
	snapshot, err := json.Marshal(policy)
	if err != nil {
		return createdRef{}, uuid.Nil, fmt.Errorf("marshal policy snapshot: %w", err)
	}

	type line struct {
		tt    listing.TicketType
		qty   int
		total int64
	}
	lines := make([]line, len(in.Items))
	var gross int64
	for i, it := range in.Items {
		tt := types[it.TicketTypeID]
		lines[i] = line{tt: tt, qty: it.Quantity, total: tt.Price * int64(it.Quantity)}
		gross += lines[i].total
	}
	commission := money.MulBps(gross, rateBps)
	holdExpires := now.Add(time.Duration(cfg.HoldMinutes) * time.Minute)
	holdRelease := holdExpires.Add(time.Duration(cfg.HoldGraceMinutes) * time.Minute)

	q := store.New(tx)
	checkoutID, err := s.insertCheckout(ctx, q, customerID, gross, now)
	if err != nil {
		return createdRef{}, uuid.Nil, err
	}
	qrToken, err := randomToken()
	if err != nil {
		return createdRef{}, uuid.Nil, err
	}
	bookingID, code, err := insertPendingBooking(ctx, q, store.InsertPendingBookingParams{
		CheckoutID: checkoutID, CustomerID: customerID,
		PartnerID: l.PartnerID, ListingID: l.ID, ServiceType: l.ServiceType,
		ServiceStartAt: startAt, ServiceEndAt: endAt,
		GrossAmount: gross, CommissionRateBps: int32(rateBps), CommissionAmount: commission,
		CancellationPolicySnapshot: snapshot,
		ContactName:                req.name, ContactPhone: req.phone, ContactEmail: req.mail, CustomerNote: req.note,
		HoldExpiresAt: &holdExpires, HoldReleaseAt: &holdRelease, QrToken: qrToken, Now: now,
	})
	if err != nil {
		return createdRef{}, uuid.Nil, err
	}
	for _, ln := range lines {
		itemID, err := uuid.NewV7()
		if err != nil {
			return createdRef{}, uuid.Nil, err
		}
		if err := q.InsertBookingItem(ctx, store.InsertBookingItemParams{
			ID: itemID, BookingID: bookingID, LineKind: lineKindTicket,
			Description: ln.tt.Name + " (" + audienceLabel(ln.tt.Audience) + ")",
			UnitPrice:   ln.tt.Price, Quantity: int32(ln.qty), LineTotal: ln.total, Now: now,
		}); err != nil {
			return createdRef{}, uuid.Nil, fmt.Errorf("insert booking item: %w", err)
		}
		if err := q.InsertTicketLine(ctx, store.InsertTicketLineParams{
			BookingItemID: itemID, BookingID: bookingID, ListingID: l.ID, TicketTypeID: ln.tt.ID, Now: now,
		}); err != nil {
			return createdRef{}, uuid.Nil, fmt.Errorf("insert ticket line: %w", err)
		}
	}
	if err := q.InsertAttractionDetail(ctx, store.InsertAttractionDetailParams{
		BookingID: bookingID, ListingID: l.ID, VisitDate: req.visitDate.Format(dateLayout),
		TimeSlotID: slot.ID, SlotStart: slot.StartTime, SlotEnd: slot.EndTime, Now: now,
	}); err != nil {
		return createdRef{}, uuid.Nil, fmt.Errorf("insert attraction detail: %w", err)
	}
	histID, err := uuid.NewV7()
	if err != nil {
		return createdRef{}, uuid.Nil, err
	}
	if err := q.InsertStatusHistory(ctx, store.InsertStatusHistoryParams{
		ID: histID, BookingID: bookingID, ToStatus: StatusPendingPayment,
		ActorKind: actorCustomer, ActorUserID: &customerID, Now: now,
	}); err != nil {
		return createdRef{}, uuid.Nil, fmt.Errorf("insert status history: %w", err)
	}
	pay, err := s.payments.ReservePayment(ctx, tx, checkoutID, gross, holdExpires)
	if err != nil {
		return createdRef{}, uuid.Nil, err
	}
	return createdRef{ID: bookingID, Code: code}, pay.ID, nil
}

// insertPendingBooking inserts a pending_payment booking with a fresh id and
// LT code (p.ID and p.Code are set here) and returns both, retrying on a
// code collision (ON CONFLICT DO NOTHING keeps the tx usable).
func insertPendingBooking(ctx context.Context, q *store.Queries, p store.InsertPendingBookingParams) (uuid.UUID, string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, "", err
	}
	p.ID = id
	for range codeInsertTries {
		if p.Code, err = randomCode("LT"); err != nil {
			return uuid.Nil, "", err
		}
		n, err := q.InsertPendingBooking(ctx, p)
		if err != nil {
			return uuid.Nil, "", fmt.Errorf("insert booking: %w", err)
		}
		if n == 1 {
			return id, p.Code, nil
		}
	}
	return uuid.Nil, "", errors.New("booking code: too many collisions")
}

// insertCheckout inserts an open checkout with a fresh CO code, retrying on
// a code collision (ON CONFLICT DO NOTHING keeps the tx usable).
func (s *Service) insertCheckout(ctx context.Context, q *store.Queries, customerID uuid.UUID, total int64, now time.Time) (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	for range codeInsertTries {
		code, err := randomCode("CO")
		if err != nil {
			return uuid.Nil, err
		}
		n, err := q.InsertCheckout(ctx, store.InsertCheckoutParams{ID: id, Code: code, CustomerID: customerID, TotalAmount: total, Now: now})
		if err != nil {
			return uuid.Nil, fmt.Errorf("insert checkout: %w", err)
		}
		if n == 1 {
			return id, nil
		}
	}
	return uuid.Nil, errors.New("checkout code: too many collisions")
}

// Pay (re)creates the payment link of the customer's pending_payment
// booking and returns the booking. Without a pending payment (a refunded
// underpayment) payment.ReissueAfterMismatch reserves a new one first.
// Another customer's booking is ErrBookingNotFound. No DB tx is open while the provider is called.
func (s *Service) Pay(ctx context.Context, customerID, id uuid.UUID) (BookingView, error) {
	b, err := store.New(s.pool).GetBooking(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && b.CustomerID != customerID) {
		return BookingView{}, ErrBookingNotFound
	}
	if err != nil {
		return BookingView{}, fmt.Errorf("get booking: %w", err)
	}
	if b.Status != StatusPendingPayment {
		return BookingView{}, ErrBookingNotPayable
	}
	if b.HoldExpiresAt == nil || !s.clock.Now().Before(*b.HoldExpiresAt) {
		return BookingView{}, ErrHoldExpired
	}
	_, err = s.payments.StartPayment(ctx, b.CheckoutID)
	if errors.Is(err, payment.ErrPaymentNotFound) {
		// No pending payment: after a refunded underpayment a new one is
		// reserved with the same deadline (M-4), then its link created.
		if _, err = s.payments.ReissueAfterMismatch(ctx, b.CheckoutID); err == nil {
			_, err = s.payments.StartPayment(ctx, b.CheckoutID)
		}
	}
	if err != nil {
		if errors.Is(err, payment.ErrPaymentExpired) {
			return BookingView{}, ErrHoldExpired
		}
		if errors.Is(err, payment.ErrPaymentNotFound) || errors.Is(err, payment.ErrPaymentNotPending) {
			return BookingView{}, ErrBookingNotPayable
		}
		return BookingView{}, err
	}
	return s.GetMine(ctx, customerID, id)
}
