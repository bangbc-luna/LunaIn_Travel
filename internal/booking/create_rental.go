package booking

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
	"github.com/bangbc-luna/lunain-travel/internal/platform/money"
)

const (
	serviceTypeRental   = "self_drive_rental"
	lineKindRentalDay   = "rental_day"
	lineKindDeliveryFee = "delivery_fee"

	// emailTimeLayout formats a time in emails (Vietnam time).
	emailTimeLayout = "15:04 ngày 02/01/2006"
)

var (
	ErrVehicleUnavailable = httpx.NewError(http.StatusConflict, "vehicle_unavailable", "Mẫu xe này đã hết xe trống trong khoảng thời gian đã chọn")
	ErrInvalidDestination = httpx.NewError(http.StatusUnprocessableEntity, "invalid_destination", "Tỉnh/thành nơi đến không hợp lệ")
)

// CreateRentalInput is the POST /v1/bookings/rental body: one vehicle of
// VehicleModelID taken over and returned at PickupPointID, over
// [PickupAt, ReturnAt). DestinationProvinceCode is where the customer is
// going (shown to the partner only).
type CreateRentalInput struct {
	ListingID               uuid.UUID `json:"listing_id"`
	VehicleModelID          uuid.UUID `json:"vehicle_model_id"`
	PickupPointID           uuid.UUID `json:"pickup_point_id"`
	PickupAt                time.Time `json:"pickup_at"`
	ReturnAt                time.Time `json:"return_at"`
	DestinationProvinceCode string    `json:"destination_province_code"`
	ContactName             string    `json:"contact_name"`
	ContactPhone            string    `json:"contact_phone"`
	ContactEmail            string    `json:"contact_email"`
	Note                    *string   `json:"note"`
}

// rentalRequest is a validated, normalized CreateRentalInput.
type rentalRequest struct {
	in          CreateRentalInput
	destination string
	contact     contact
}

func (in CreateRentalInput) normalize() (rentalRequest, error) {
	r := rentalRequest{in: in, destination: strings.TrimSpace(in.DestinationProvinceCode)}
	if r.destination == "" {
		return r, ErrInvalidDestination
	}
	c, err := normalizeContact(in.ContactName, in.ContactPhone, in.ContactEmail, in.Note)
	if err != nil {
		return r, err
	}
	r.contact = c
	return r, nil
}

// CreateRental sends a self-drive rental request for customerID: one open
// checkout of the gross and one requested Request Booking that holds a
// vehicle of the model from now until the partner answers
// (hold_until = response_due_at). No payment is made yet: the customer pays
// once the partner accepts. Everything happens in one tx, in this order:
//
//  1. The Idempotency-Key is reserved and the profile filled.
//  2. The model must belong to in.ListingID (else ErrRentalModelNotFound);
//     BookableListing takes partners and the listing FOR SHARE, and the
//     listing must be a self_drive_rental.
//  3. The period is checked (listing.CheckRentalPeriod: whole minutes, 1..30
//     days, pickup within 180 days and at least request_payment_hours + 1h
//     ahead), both ends must lie in the store's handover window (Vietnam
//     time), the pickup point must be active, the destination a province.
//  4. The model is locked FOR UPDATE (DB §1.1: after listings, before
//     checkouts); it must be active and its peak of concurrent holds over
//     the period plus this one must not exceed its quantity
//     (ErrVehicleUnavailable).
//  5. The checkout, booking (service_start_at = pickup_at, service_end_at =
//     return_at), its items (rental_day × days, then the pickup point's
//     delivery_fee once if any; Σ items = gross), rental details and first
//     history row are inserted; the customer and the partner owner are
//     emailed (outbox).
//
// Idempotency works as for CreateAttraction; a replay answers the booking
// re-read now.
func (s *Service) CreateRental(ctx context.Context, customerID uuid.UUID, idemKey string, in CreateRentalInput) (BookingView, error) {
	if !validIdemKey(idemKey) {
		return BookingView{}, ErrIdempotencyKeyRequired
	}
	hash, err := requestHash(endpointRental, in)
	if err != nil {
		return BookingView{}, err
	}
	req, err := in.normalize()
	if err != nil {
		return BookingView{}, err
	}
	now := s.clock.Now()

	var bookingID uuid.UUID
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.ReserveIdempotencyKey(ctx, store.ReserveIdempotencyKeyParams{
			UserID: customerID, Key: idemKey, Endpoint: endpointRental, RequestHash: hash, Now: now,
		})
		if err != nil {
			return fmt.Errorf("reserve idempotency key: %w", err)
		}
		if n == 0 {
			body, err := replayed(ctx, q, customerID, idemKey, endpointRental, hash)
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
		ref, err := s.createRentalTx(ctx, tx, customerID, req, now)
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

// createRentalTx is the in-tx part of CreateRental after the idempotency key
// was reserved.
func (s *Service) createRentalTx(ctx context.Context, tx pgx.Tx, customerID uuid.UUID, req rentalRequest, now time.Time) (createdRef, error) {
	in := req.in
	listingID, err := s.listings.RentalModelListingID(ctx, tx, in.VehicleModelID)
	if err != nil {
		return createdRef{}, err
	}
	if listingID != in.ListingID {
		return createdRef{}, listing.ErrRentalModelNotFound
	}
	l, err := s.listings.BookableListing(ctx, tx, in.ListingID)
	if err != nil {
		return createdRef{}, err
	}
	if l.ServiceType != serviceTypeRental {
		return createdRef{}, listing.ErrWrongServiceType
	}

	cfg, err := s.settings.GetTx(ctx, tx)
	if err != nil {
		return createdRef{}, err
	}
	days, err := listing.CheckRentalPeriod(in.PickupAt, in.ReturnAt, now, cfg.RequestPaymentHours)
	if err != nil {
		return createdRef{}, err
	}
	st, err := s.listings.RentalStoreOf(ctx, tx, l.ID)
	if err != nil {
		return createdRef{}, err
	}
	if !st.CoversHandover(in.PickupAt) || !st.CoversHandover(in.ReturnAt) {
		return createdRef{}, listing.ErrOutsideHandoverHours
	}
	point, err := s.listings.PickupPointForBooking(ctx, tx, l.ID, in.PickupPointID)
	if err != nil {
		return createdRef{}, err
	}
	q := store.New(tx)
	ok, err := q.ProvinceExists(ctx, req.destination)
	if err != nil {
		return createdRef{}, fmt.Errorf("province exists: %w", err)
	}
	if !ok {
		return createdRef{}, ErrInvalidDestination
	}

	// rental_vehicle_models FOR UPDATE, then the availability under the lock.
	models, err := s.listings.LockRentalModels(ctx, tx, []uuid.UUID{in.VehicleModelID})
	if err != nil {
		return createdRef{}, err
	}
	model := models[0]
	if err := s.rentalAvailable(ctx, q, model, in.PickupAt, in.ReturnAt, now, uuid.Nil); err != nil {
		return createdRef{}, err
	}

	rateBps, err := s.catalog.CurrentCommissionRate(ctx, tx, serviceTypeRental, now)
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
	dayTotal := model.PricePerDay * int64(days)
	gross := dayTotal + point.DeliveryFee
	due := requestResponseDue(cfg, now, in.PickupAt)

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
		PartnerID: l.PartnerID, ListingID: l.ID, ServiceType: serviceTypeRental,
		ServiceStartAt: in.PickupAt, ServiceEndAt: in.ReturnAt,
		GrossAmount: gross, CommissionRateBps: int32(rateBps), CommissionAmount: money.MulBps(gross, rateBps),
		CancellationPolicySnapshot: snapshot,
		ContactName:                c.name, ContactPhone: c.phone, ContactEmail: c.mail, CustomerNote: c.note,
		ResponseDueAt: &due, QrToken: qrToken, Now: now,
	})
	if err != nil {
		return createdRef{}, err
	}
	if err := insertItem(ctx, q, bookingID, lineKindRentalDay,
		fmt.Sprintf("Thuê %s %d ngày", model.Name, days), model.PricePerDay, days, dayTotal, now); err != nil {
		return createdRef{}, err
	}
	if point.DeliveryFee > 0 {
		if err := insertItem(ctx, q, bookingID, lineKindDeliveryFee,
			"Phí giao xe tại "+point.Name, point.DeliveryFee, 1, point.DeliveryFee, now); err != nil {
			return createdRef{}, err
		}
	}
	if err := q.InsertRentalDetail(ctx, store.InsertRentalDetailParams{
		BookingID: bookingID, ListingID: l.ID, VehicleModelID: model.ID, PickupPointID: point.ID,
		PickupAt: in.PickupAt, ReturnAt: in.ReturnAt, Days: int32(days),
		ModelName: model.Name, PickupPointName: point.Name, PickupAddress: point.Address,
		DestinationProvinceCode: req.destination, HoldUntil: &due, Now: now,
	}); err != nil {
		return createdRef{}, fmt.Errorf("insert rental detail: %w", err)
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

	r := rentalMail{
		code: code, contactName: c.name, model: model.Name, point: point,
		pickup: in.PickupAt, ret: in.ReturnAt, days: days, gross: gross, due: due,
		destination: req.destination,
	}
	if err := s.enqueueEmail(ctx, tx, r.customerRequested(c.mail)); err != nil {
		return createdRef{}, err
	}
	subject, text := r.partnerRequested(l.Title)
	if err := s.emailPartner(ctx, tx, l.PartnerID, subject, text); err != nil {
		return createdRef{}, err
	}
	return createdRef{ID: bookingID, Code: code}, nil
}

// rentalAvailable checks, under the model's FOR UPDATE lock, that one more
// vehicle of model can be held over [from, to): the model is active and the
// peak of concurrent holds (peakRentalHolds, except one booking) plus one
// does not exceed its quantity. Otherwise it is ErrVehicleUnavailable.
func (s *Service) rentalAvailable(ctx context.Context, q *store.Queries, model listing.RentalModel, from, to, now time.Time, except uuid.UUID) error {
	if !model.Active {
		return ErrVehicleUnavailable
	}
	peak, err := peakRentalHolds(ctx, q, []uuid.UUID{model.ID}, from, &to, now, except)
	if err != nil {
		return err
	}
	if peak[model.ID]+1 > model.Quantity {
		return ErrVehicleUnavailable
	}
	return nil
}

// insertItem inserts one booking line.
func insertItem(ctx context.Context, q *store.Queries, bookingID uuid.UUID, kind, desc string, unit int64, qty int, total int64, now time.Time) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if err := q.InsertBookingItem(ctx, store.InsertBookingItemParams{
		ID: id, BookingID: bookingID, LineKind: kind, Description: desc,
		UnitPrice: unit, Quantity: int32(qty), LineTotal: total, Now: now,
	}); err != nil {
		return fmt.Errorf("insert booking item: %w", err)
	}
	return nil
}

// rentalMail is what the rental request emails say.
type rentalMail struct {
	code, contactName, model, destination string
	point                                 listing.PickupPoint
	pickup, ret, due                      time.Time
	days                                  int
	gross                                 int64
}

func vnTime(t time.Time) string { return t.In(clock.VN).Format(emailTimeLayout) }

func (r rentalMail) summary() string {
	return "Mã đặt chỗ: " + r.code + "\n" +
		"Mẫu xe: " + r.model + "\n" +
		"Nhận xe: " + vnTime(r.pickup) + " tại " + r.point.Name + " (" + r.point.Address + ")\n" +
		"Trả xe: " + vnTime(r.ret) + " tại cùng điểm nhận\n" +
		fmt.Sprintf("Số ngày thuê: %d\n", r.days) +
		"Tổng tiền: " + formatVND(r.gross) + "\n"
}

func (r rentalMail) customerRequested(to string) mailer.Message {
	return mailer.Message{
		To:      to,
		Subject: "Đã gửi yêu cầu thuê xe " + r.code,
		Text: "Xin chào " + r.contactName + ",\n\n" +
			"LunaIn đã gửi yêu cầu thuê xe của bạn tới đối tác, đang chờ đối tác xác nhận trước " + vnTime(r.due) + ".\n" +
			"Xe được giữ cho bạn trong thời gian chờ. Bạn chỉ cần thanh toán sau khi đối tác chấp nhận.\n\n" +
			r.summary(),
	}
}

func (r rentalMail) partnerRequested(listingTitle string) (string, string) {
	return "Có yêu cầu thuê xe mới " + r.code,
		"Xin chào,\n\n" +
			"Dịch vụ \"" + listingTitle + "\" có yêu cầu thuê xe mới.\n" +
			"Hạn phản hồi: " + vnTime(r.due) + ". Quá hạn mà chưa chấp nhận hoặc từ chối, yêu cầu sẽ tự hết hạn.\n\n" +
			r.summary() +
			"Tỉnh/thành khách đến: " + r.destination + "\n"
}
