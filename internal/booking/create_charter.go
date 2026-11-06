package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
	"github.com/bangbc-luna/lunain-travel/internal/platform/money"
)

const (
	serviceTypeCharter      = "private_charter"
	lineKindCharterBase     = "charter_base"
	lineKindCharterDistance = "charter_distance"
	lineKindSurcharge       = "surcharge"
	endpointCharter         = "POST /v1/bookings/charter"
)

// ErrQuoteChanged: the price computed now differs from the quote the
// customer saw (quoted_total); nothing was created.
var ErrQuoteChanged = httpx.NewError(http.StatusConflict, "quote_changed", "Giá chuyến đi đã thay đổi, vui lòng xem lại báo giá")

// CreateCharterInput is the POST /v1/bookings/charter body: one vehicle of
// VehicleClassID from Pickup to Dropoff at PickupAt (and back at ReturnAt for
// a round trip). QuotedTotal is the quote the customer saw; the server
// prices the trip again and refuses a different total (ErrQuoteChanged).
type CreateCharterInput struct {
	ListingID      uuid.UUID            `json:"listing_id"`
	VehicleClassID uuid.UUID            `json:"vehicle_class_id"`
	Pickup         listing.CharterPlace `json:"pickup"`
	Dropoff        listing.CharterPlace `json:"dropoff"`
	PickupAt       time.Time            `json:"pickup_at"`
	IsRoundTrip    bool                 `json:"is_round_trip"`
	ReturnAt       *time.Time           `json:"return_at"`
	QuotedTotal    int64                `json:"quoted_total"`
	ContactName    string               `json:"contact_name"`
	ContactPhone   string               `json:"contact_phone"`
	ContactEmail   string               `json:"contact_email"`
	Note           *string              `json:"note"`
}

// charterRequest is a validated, normalized CreateCharterInput.
type charterRequest struct {
	in      CreateCharterInput
	trip    listing.CharterTrip
	contact contact
}

func (in CreateCharterInput) normalize() (charterRequest, error) {
	r := charterRequest{in: in}
	c, err := normalizeContact(in.ContactName, in.ContactPhone, in.ContactEmail, in.Note)
	if err != nil {
		return r, err
	}
	r.contact = c
	r.trip, err = listing.CharterTrip{
		Pickup: in.Pickup, Dropoff: in.Dropoff, PickupAt: in.PickupAt, RoundTrip: in.IsRoundTrip, ReturnAt: in.ReturnAt,
	}.Normalize()
	if err != nil {
		return r, err
	}
	if in.QuotedTotal <= 0 {
		return r, invalidBooking("Cần gửi tổng tiền của báo giá đã xem (quoted_total)")
	}
	return r, nil
}

// CreateCharter sends a private charter request for customerID: one open
// checkout of the gross and one requested Request Booking. A charter holds
// no capacity and no payment is made yet: the customer pays once the partner
// accepts. The map provider is never called inside a transaction:
//
//  1. The Idempotency-Key and the body are checked before any I/O.
//  2. A response already stored under the key (read unlocked) is replayed
//     without calling the map; the same key with another body is
//     ErrIdempotencyKeyReused.
//  3. The checks that do not need the route run in a short tx (listing
//     bookable and a private_charter, times, pickup province in the service
//     area, class active and of the listing), so a doomed request never
//     costs a map call.
//  4. The route is asked outside any tx; a map failure (503) or no route
//     (422) returns before the key is reserved.
//  5. One tx reserves the key (a concurrent duplicate replays), fills the
//     profile, takes BookableListing (partners, listings FOR SHARE), checks
//     the times against the route, prices the class again
//     (CharterQuoteTx) and refuses a total other than in.QuotedTotal
//     (ErrQuoteChanged, nothing kept). Then the checkout, the booking
//     (service_start_at = pickup_at, service_end_at = pickup_at + driving
//     time, or return_at for a round trip), its items (charter_base,
//     charter_distance in tenths of a km, then one surcharge line each;
//     Σ items = gross), the charter details with the quote snapshot and the
//     first history row are inserted, and the customer and the partner owner
//     are emailed (outbox).
func (s *Service) CreateCharter(ctx context.Context, customerID uuid.UUID, idemKey string, in CreateCharterInput) (BookingView, error) {
	if !validIdemKey(idemKey) {
		return BookingView{}, ErrIdempotencyKeyRequired
	}
	hash, err := requestHash(endpointCharter, in)
	if err != nil {
		return BookingView{}, err
	}
	req, err := in.normalize()
	if err != nil {
		return BookingView{}, err
	}

	id, ok, err := s.storedResponse(ctx, customerID, idemKey, endpointCharter, hash)
	if err != nil {
		return BookingView{}, err
	}
	if ok {
		return s.GetMine(ctx, customerID, id)
	}

	now := s.clock.Now()
	if err := s.precheckCharter(ctx, req, now); err != nil {
		return BookingView{}, err
	}
	route, err := s.listings.CharterRoute(ctx, req.trip.Pickup, req.trip.Dropoff)
	if err != nil {
		return BookingView{}, err
	}

	var bookingID uuid.UUID
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.ReserveIdempotencyKey(ctx, store.ReserveIdempotencyKeyParams{
			UserID: customerID, Key: idemKey, Endpoint: endpointCharter, RequestHash: hash, Now: now,
		})
		if err != nil {
			return fmt.Errorf("reserve idempotency key: %w", err)
		}
		if n == 0 {
			body, err := replayed(ctx, q, customerID, idemKey, endpointCharter, hash)
			if err != nil {
				return err
			}
			var ref createdRef
			if err := json.Unmarshal(body, &ref); err != nil {
				return fmt.Errorf("decode idempotent response: %w", err)
			}
			bookingID = ref.ID
			return nil
		}
		if err := s.identity.FillProfileIfEmpty(ctx, tx, customerID, req.contact.name, req.contact.phone); err != nil {
			return fmt.Errorf("fill profile: %w", err)
		}
		ref, err := s.createCharterTx(ctx, tx, customerID, req, route, now)
		if err != nil {
			return err
		}
		bookingID = ref.ID
		return storeIdempotentResponse(ctx, q, customerID, idemKey, ref, now)
	})
	if err != nil {
		return BookingView{}, err
	}
	return s.GetMine(ctx, customerID, bookingID)
}

// storedResponse reads, unlocked, the booking stored under an already
// answered Idempotency-Key of endpoint (ok). A key used on another endpoint
// or with another body is ErrIdempotencyKeyReused. An unknown key, or one
// still in flight, is not ok: the creation tx reserves it (and waits for or
// replays the in-flight one).
func (s *Service) storedResponse(ctx context.Context, customerID uuid.UUID, key, endpoint string, hash []byte) (uuid.UUID, bool, error) {
	row, err := store.New(s.pool).GetIdempotencyKey(ctx, store.GetIdempotencyKeyParams{UserID: customerID, Key: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("get idempotency key: %w", err)
	}
	if row.Endpoint != endpoint || string(row.RequestHash) != string(hash) {
		return uuid.Nil, false, ErrIdempotencyKeyReused
	}
	if row.ResponseBody == nil {
		return uuid.Nil, false, nil
	}
	var ref createdRef
	if err := json.Unmarshal(row.ResponseBody, &ref); err != nil {
		return uuid.Nil, false, fmt.Errorf("decode idempotent response: %w", err)
	}
	return ref.ID, true, nil
}

// precheckCharter runs, in a short tx before the paid map call, the checks
// of a request that do not need the route: the listing is bookable
// (BookableListing) and a private charter, and listing.CharterPrecheck
// passes for the requested class. createCharterTx repeats them all under the
// creation tx's locks.
func (s *Service) precheckCharter(ctx context.Context, req charterRequest, now time.Time) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		l, err := s.listings.BookableListing(ctx, tx, req.in.ListingID)
		if err != nil {
			return err
		}
		if l.ServiceType != serviceTypeCharter {
			return listing.ErrWrongServiceType
		}
		cfg, err := s.settings.GetTx(ctx, tx)
		if err != nil {
			return err
		}
		classID := req.in.VehicleClassID
		_, err = s.listings.CharterPrecheck(ctx, tx, l.ID, &classID, req.trip, now, cfg.RequestPaymentHours)
		return err
	})
}

// createCharterTx is the in-tx part of CreateCharter after the idempotency
// key was reserved.
func (s *Service) createCharterTx(ctx context.Context, tx pgx.Tx, customerID uuid.UUID, req charterRequest, route listing.CharterRoute, now time.Time) (createdRef, error) {
	in, trip := req.in, req.trip
	l, err := s.listings.BookableListing(ctx, tx, in.ListingID)
	if err != nil {
		return createdRef{}, err
	}
	if l.ServiceType != serviceTypeCharter {
		return createdRef{}, listing.ErrWrongServiceType
	}
	cfg, err := s.settings.GetTx(ctx, tx)
	if err != nil {
		return createdRef{}, err
	}
	serviceEnd, err := listing.CheckCharterTime(trip.PickupAt, trip.RoundTrip, trip.ReturnAt, route.DurationMinutes, now, cfg.RequestPaymentHours)
	if err != nil {
		return createdRef{}, err
	}
	classID := in.VehicleClassID
	quotes, err := s.listings.CharterQuoteTx(ctx, tx, l.ID, &classID, trip, route)
	if err != nil {
		return createdRef{}, err
	}
	if len(quotes) != 1 {
		return createdRef{}, fmt.Errorf("charter quote: %d quotes for one class", len(quotes))
	}
	quote := quotes[0]
	if quote.Total != in.QuotedTotal {
		return createdRef{}, ErrQuoteChanged
	}
	lines, err := charterLines(quote)
	if err != nil {
		return createdRef{}, err
	}

	rateBps, err := s.catalog.CurrentCommissionRate(ctx, tx, serviceTypeCharter, now)
	if err != nil {
		return createdRef{}, err
	}
	policy, err := s.listings.CancellationPolicy(ctx, tx, l.ID)
	if err != nil {
		return createdRef{}, err
	}
	snapshot, err := json.Marshal(policy)
	if err != nil {
		return createdRef{}, fmt.Errorf("marshal policy snapshot: %w", err)
	}
	quoteJSON, err := json.Marshal(quote)
	if err != nil {
		return createdRef{}, fmt.Errorf("marshal quote snapshot: %w", err)
	}
	gross := quote.Total
	due := requestResponseDue(cfg, now, trip.PickupAt)

	q := store.New(tx)
	checkoutID, err := s.insertCheckout(ctx, q, customerID, gross, now)
	if err != nil {
		return createdRef{}, err
	}
	qrToken, err := randomToken()
	if err != nil {
		return createdRef{}, err
	}
	c := req.contact
	bookingID, code, err := insertRequestedBooking(ctx, q, store.InsertRequestedBookingParams{
		CheckoutID: checkoutID, CustomerID: customerID,
		PartnerID: l.PartnerID, ListingID: l.ID, ServiceType: serviceTypeCharter,
		ServiceStartAt: trip.PickupAt, ServiceEndAt: serviceEnd,
		GrossAmount: gross, CommissionRateBps: int32(rateBps), CommissionAmount: money.MulBps(gross, rateBps),
		CancellationPolicySnapshot: snapshot,
		ContactName:                c.name, ContactPhone: c.phone, ContactEmail: c.mail, CustomerNote: c.note,
		ResponseDueAt: &due, QrToken: qrToken, Now: now,
	})
	if err != nil {
		return createdRef{}, err
	}
	for _, ln := range lines {
		if err := insertItemTenths(ctx, q, bookingID, ln.kind, ln.desc, ln.unit, ln.tenths, ln.total, now); err != nil {
			return createdRef{}, err
		}
	}
	if err := q.InsertCharterDetail(ctx, store.InsertCharterDetailParams{
		BookingID: bookingID, ListingID: l.ID, VehicleClassID: quote.VehicleClassID, PickupAt: trip.PickupAt,
		PickupAddress: trip.Pickup.Address, PickupLat: trip.Pickup.Lat, PickupLng: trip.Pickup.Lng,
		DropoffAddress: trip.Dropoff.Address, DropoffLat: trip.Dropoff.Lat, DropoffLng: trip.Dropoff.Lng,
		DistanceTenths: int32(quote.DistanceTenths), IsRoundTrip: trip.RoundTrip, ReturnAt: trip.ReturnAt,
		Quote: quoteJSON, PickupProvinceCode: trip.Pickup.ProvinceCode, DurationMinutes: int32(quote.DurationMinutes),
		VehicleClassName: quote.ClassName, Seats: int32(quote.Seats), Now: now,
	}); err != nil {
		return createdRef{}, fmt.Errorf("insert charter detail: %w", err)
	}
	histID, err := uuid.NewV7()
	if err != nil {
		return createdRef{}, err
	}
	if err := q.InsertStatusHistory(ctx, store.InsertStatusHistoryParams{
		ID: histID, BookingID: bookingID, ToStatus: StatusRequested,
		ActorKind: actorCustomer, ActorUserID: &customerID, Now: now,
	}); err != nil {
		return createdRef{}, fmt.Errorf("insert status history: %w", err)
	}

	m := charterMail{code: code, contactName: c.name, trip: trip, quote: quote, gross: gross, due: due}
	if err := s.enqueueEmail(ctx, tx, m.customerRequested(c.mail)); err != nil {
		return createdRef{}, err
	}
	subject, text := m.partnerRequested(l.Title)
	if err := s.emailPartner(ctx, tx, l.PartnerID, subject, text); err != nil {
		return createdRef{}, err
	}
	return createdRef{ID: bookingID, Code: code}, nil
}

// charterLine is one booking_items line of a charter; tenths is the quantity
// in tenths (10 = 1).
type charterLine struct {
	kind, desc          string
	unit, tenths, total int64
}

// charterLines turns a quote into its booking lines, in order: charter_base
// (skipped when the base fare is 0), charter_distance (price per km × billed
// km), then one surcharge line per applied surcharge (the quote's order:
// night, weekend or holiday, round-trip waiting). The lines must add up to
// the quote total, or it is an error.
func charterLines(q listing.CharterQuote) ([]charterLine, error) {
	var out []charterLine
	if q.BaseFare > 0 {
		out = append(out, charterLine{lineKindCharterBase, "Giá mở cửa " + q.ClassName, q.BaseFare, 10, q.BaseFare})
	}
	dist := "Quãng đường " + kmText(q.DistanceTenths) + " km"
	if q.IsRoundTrip {
		dist = "Quãng đường khứ hồi 2 × " + kmText(q.DistanceTenths) + " km"
	}
	out = append(out, charterLine{lineKindCharterDistance, dist, q.PricePerKm, q.BilledTenths, q.DistanceTotal})
	for _, sc := range q.Surcharges {
		out = append(out, charterLine{lineKindSurcharge, surchargeText(sc), sc.Amount, 10, sc.Amount})
	}
	var sum int64
	for _, ln := range out {
		if ln.total != money.MulTenths(ln.unit, ln.tenths) {
			return nil, fmt.Errorf("charter line %q: %d × %d tenths != %d", ln.kind, ln.unit, ln.tenths, ln.total)
		}
		sum += ln.total
	}
	if sum != q.Total {
		return nil, fmt.Errorf("charter lines add up to %d, quote total %d", sum, q.Total)
	}
	return out, nil
}

// surchargeText describes a surcharge line; a percent one shows its rate.
func surchargeText(sc listing.SurchargeLine) string {
	var name string
	switch sc.Kind {
	case listing.SurchargeNight:
		name = "Phụ phí ban đêm"
	case listing.SurchargeWeekend:
		name = "Phụ phí cuối tuần"
	case listing.SurchargeHoliday:
		name = "Phụ phí ngày lễ"
	case listing.SurchargeRoundTripWaiting:
		name = "Phụ phí chờ khứ hồi"
	default:
		name = "Phụ phí"
	}
	if sc.Calc == listing.CalcPercent {
		name += " (" + percentText(sc.Rate) + ")"
	}
	return name
}

// percentText writes basis points as a Vietnamese percent: 1000 -> 10%,
// 1250 -> 12,5%, 1005 -> 10,05%.
func percentText(bps int64) string {
	s := strconv.FormatInt(bps/100, 10)
	if frac := bps % 100; frac != 0 {
		s += "," + strings.TrimRight(fmt.Sprintf("%02d", frac), "0")
	}
	return s + "%"
}

// kmText writes tenths of a km the Vietnamese way: 30,3.
func kmText(tenths int64) string {
	return strconv.FormatInt(tenths/10, 10) + "," + strconv.FormatInt(tenths%10, 10)
}

// insertItemTenths inserts one booking line whose quantity is given in
// tenths (charter_distance km; 10 for a quantity of 1).
func insertItemTenths(ctx context.Context, q *store.Queries, bookingID uuid.UUID, kind, desc string, unit, tenths, total int64, now time.Time) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if err := q.InsertBookingItemTenths(ctx, store.InsertBookingItemTenthsParams{
		ID: id, BookingID: bookingID, LineKind: kind, Description: desc,
		UnitPrice: unit, QuantityTenths: tenths, LineTotal: total, Now: now,
	}); err != nil {
		return fmt.Errorf("insert booking item: %w", err)
	}
	return nil
}

// charterMail is what the charter request emails say.
type charterMail struct {
	code, contactName string
	trip              listing.CharterTrip
	quote             listing.CharterQuote
	gross             int64
	due               time.Time
}

func (m charterMail) summary() string {
	s := "Mã đặt chỗ: " + m.code + "\n" +
		fmt.Sprintf("Loại xe: %s (%d chỗ)\n", m.quote.ClassName, m.quote.Seats) +
		"Điểm đón: " + m.trip.Pickup.Address + "\n" +
		"Điểm trả: " + m.trip.Dropoff.Address + "\n" +
		"Giờ đón: " + vnTime(m.trip.PickupAt) + "\n"
	if m.trip.RoundTrip && m.trip.ReturnAt != nil {
		s += "Giờ về (khứ hồi): " + vnTime(*m.trip.ReturnAt) + "\n"
	}
	s += "Quãng đường: " + kmText(m.quote.DistanceTenths) + " km một chiều"
	if m.trip.RoundTrip {
		s += ", tính tiền 2 × " + kmText(m.quote.DistanceTenths) + " km"
	}
	return s + "\n" + "Tổng tiền: " + formatVND(m.gross) + "\n"
}

func (m charterMail) customerRequested(to string) mailer.Message {
	return mailer.Message{
		To:      to,
		Subject: "Đã gửi yêu cầu xe bao chuyến " + m.code,
		Text: "Xin chào " + m.contactName + ",\n\n" +
			"LunaIn đã gửi yêu cầu xe bao chuyến của bạn tới nhà xe, đang chờ nhà xe xác nhận trước " + vnTime(m.due) + ".\n" +
			"Bạn chỉ cần thanh toán sau khi nhà xe chấp nhận.\n\n" +
			m.summary(),
	}
}

func (m charterMail) partnerRequested(listingTitle string) (string, string) {
	return "Có yêu cầu xe bao chuyến mới " + m.code,
		"Xin chào,\n\n" +
			"Dịch vụ \"" + listingTitle + "\" có yêu cầu xe bao chuyến mới.\n" +
			"Hạn phản hồi: " + vnTime(m.due) + ". Quá hạn mà chưa chấp nhận hoặc từ chối, yêu cầu sẽ tự hết hạn.\n\n" +
			m.summary()
}
