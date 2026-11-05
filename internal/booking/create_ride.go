package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/money"
)

const (
	serviceTypeRide = "scheduled_ride"
	lineKindSeat    = "seat"
	tripScheduled   = "scheduled"

	directionOutbound = "outbound"
	directionReturn   = "return"

	// seatTimeLayout formats the departure in a seat line (Vietnam time).
	seatTimeLayout = "15:04 02/01/2006"
)

var (
	ErrTripUnavailable   = httpx.NewError(http.StatusUnprocessableEntity, "trip_unavailable", "Chuyến này không còn nhận đặt chỗ")
	ErrNotEnoughSeats    = httpx.NewError(http.StatusConflict, "not_enough_seats", "Chuyến không còn đủ ghế trống")
	ErrInvalidReturnTrip = httpx.NewError(http.StatusUnprocessableEntity, "invalid_return_trip", "Chuyến về phải là tuyến ngược của cùng nhà xe và khởi hành sau khi chuyến đi tới nơi")
)

// CreateRideInput is the POST /v1/bookings/ride body: one way, or a round
// trip when ReturnTripID is set. Both legs have Seats seats.
type CreateRideInput struct {
	OutboundTripID uuid.UUID  `json:"outbound_trip_id"`
	ReturnTripID   *uuid.UUID `json:"return_trip_id"`
	Seats          int        `json:"seats"`
	ContactName    string     `json:"contact_name"`
	ContactPhone   string     `json:"contact_phone"`
	ContactEmail   string     `json:"contact_email"`
	Note           *string    `json:"note"`
}

// CheckoutView is a checkout with its bookings (outbound, then return).
type CheckoutView struct {
	ID          uuid.UUID     `json:"id"`
	Code        string        `json:"code"`
	Status      string        `json:"status"`
	TotalAmount int64         `json:"total_amount"`
	Bookings    []BookingView `json:"bookings"`
}

// rideRef is the response body stored with a ride Idempotency-Key.
type rideRef struct {
	CheckoutID uuid.UUID   `json:"checkout_id"`
	BookingIDs []uuid.UUID `json:"booking_ids"`
}

// rideRequest is a validated, normalized CreateRideInput.
type rideRequest struct {
	in      CreateRideInput
	contact contact
	tripIDs []uuid.UUID // outbound, then return
}

// normalize validates the input. Seats has no business cap (the trip's free
// seats are the limit) but is bounded by listing.MaxSeatsPerOperation, an
// overflow guard (Ruling 2), before any lock.
func (in CreateRideInput) normalize() (rideRequest, error) {
	r := rideRequest{in: in, tripIDs: []uuid.UUID{in.OutboundTripID}}
	if in.Seats < 1 || in.Seats > listing.MaxSeatsPerOperation {
		return r, invalidBooking(fmt.Sprintf("Số ghế phải từ 1 đến %d", listing.MaxSeatsPerOperation))
	}
	if in.ReturnTripID != nil {
		if *in.ReturnTripID == in.OutboundTripID {
			return r, ErrInvalidReturnTrip
		}
		r.tripIDs = append(r.tripIDs, *in.ReturnTripID)
	}
	c, err := normalizeContact(in.ContactName, in.ContactPhone, in.ContactEmail, in.Note)
	if err != nil {
		return r, err
	}
	r.contact = c
	return r, nil
}

// CreateRide books seats on a scheduled ride trip for customerID, or on two
// trips for a round trip: one checkout, one pending_payment booking per leg
// holding its seats and one pending payment of the total, all in one tx; the
// PayOS link is created after commit.
//
// Inside the tx, in this order: the Idempotency-Key is reserved and the
// profile filled; the listings of the trips are taken FOR SHARE (id order)
// and must be bookable scheduled_ride listings; a return leg must be a
// listing of the same partner whose route is the reverse by province; the
// trips are locked (id order) and overdue holds on them released
// (releaseOverdueOnTrips); each trip must be scheduled and depart after
// now + max(ride_booking_cutoff_minutes, hold_minutes + hold_grace_minutes),
// and the return no earlier than the outbound's arrival; the seats are
// reserved (ErrNotEnoughSeats when a trip has too few left); then the
// checkout, bookings and payment are inserted.
//
// Idempotency works as for CreateAttraction; a replay answers the checkout
// re-read now.
func (s *Service) CreateRide(ctx context.Context, customerID uuid.UUID, idemKey string, in CreateRideInput) (CheckoutView, error) {
	if !validIdemKey(idemKey) {
		return CheckoutView{}, ErrIdempotencyKeyRequired
	}
	hash, err := requestHash(endpointRide, in)
	if err != nil {
		return CheckoutView{}, err
	}
	req, err := in.normalize()
	if err != nil {
		return CheckoutView{}, err
	}
	now := s.clock.Now()

	var (
		ref       rideRef
		paymentID uuid.UUID
		replay    bool
		codes     []int64
	)
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.ReserveIdempotencyKey(ctx, store.ReserveIdempotencyKeyParams{
			UserID: customerID, Key: idemKey, Endpoint: endpointRide, RequestHash: hash, Now: now,
		})
		if err != nil {
			return fmt.Errorf("reserve idempotency key: %w", err)
		}
		if n == 0 {
			body, err := replayed(ctx, q, customerID, idemKey, endpointRide, hash)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(body, &ref); err != nil {
				return fmt.Errorf("decode idempotent response: %w", err)
			}
			replay = true
			return nil
		}
		if err := s.identity.FillProfileIfEmpty(ctx, tx, customerID, req.contact.name, req.contact.phone); err != nil {
			return fmt.Errorf("fill profile: %w", err)
		}
		ref, paymentID, codes, err = s.createRideTx(ctx, tx, customerID, req, now)
		if err != nil {
			return err
		}
		return storeIdempotentResponse(ctx, q, customerID, idemKey, ref, now)
	})
	if err != nil {
		return CheckoutView{}, err
	}
	// Outside the tx: never call the provider with a tx open.
	for _, oc := range codes {
		s.cancelLink(ctx, oc)
	}
	if replay {
		s.activateCheckoutLink(ctx, ref.CheckoutID)
	} else if _, err := s.payments.ActivateLink(context.WithoutCancel(ctx), paymentID); err != nil {
		s.log.Warn("booking: payment link not created", "checkout_id", ref.CheckoutID, "err", err)
	}
	return s.GetMyCheckout(ctx, customerID, ref.CheckoutID)
}

// rideLeg is one leg of a ride request once its trip is locked.
type rideLeg struct {
	direction string
	trip      listing.Trip
	listing   listing.Listing
	route     listing.RideRoute
}

func (l rideLeg) arriveAt() time.Time {
	return l.trip.DepartureAt.Add(time.Duration(l.route.DurationMinutes) * time.Minute)
}

// createRideTx is the in-tx part of CreateRide after the idempotency key was
// reserved. It returns the stored reference, the payment id and the order
// codes of payments expired by releaseOverdueOnTrips (links to cancel after
// commit).
func (s *Service) createRideTx(ctx context.Context, tx pgx.Tx, customerID uuid.UUID, req rideRequest, now time.Time) (rideRef, uuid.UUID, []int64, error) {
	listingOf, err := s.listings.TripListingIDs(ctx, tx, req.tripIDs)
	if errors.Is(err, listing.ErrTripNotFound) {
		return rideRef{}, uuid.Nil, nil, ErrTripUnavailable
	}
	if err != nil {
		return rideRef{}, uuid.Nil, nil, err
	}
	// partners, listings FOR SHARE in listing id order.
	listingIDs := make([]uuid.UUID, 0, len(req.tripIDs))
	for _, id := range req.tripIDs {
		listingIDs = append(listingIDs, listingOf[id])
	}
	listings := make(map[uuid.UUID]listing.Listing, len(listingIDs))
	routes := make(map[uuid.UUID]listing.RideRoute, len(listingIDs))
	for _, id := range sortedIDs(listingIDs) {
		l, err := s.listings.BookableListing(ctx, tx, id)
		if err != nil {
			return rideRef{}, uuid.Nil, nil, err
		}
		if l.ServiceType != serviceTypeRide {
			return rideRef{}, uuid.Nil, nil, listing.ErrWrongServiceType
		}
		r, err := s.listings.RideRoute(ctx, tx, id)
		if err != nil {
			return rideRef{}, uuid.Nil, nil, err
		}
		listings[id], routes[id] = l, r
	}
	legs := make([]rideLeg, len(req.tripIDs))
	for i, id := range req.tripIDs {
		lid := listingOf[id]
		legs[i] = rideLeg{direction: directionOutbound, listing: listings[lid], route: routes[lid]}
	}
	if len(legs) == 2 {
		out, ret := legs[0], legs[1]
		legs[1].direction = directionReturn
		if ret.listing.ID == out.listing.ID || ret.listing.PartnerID != out.listing.PartnerID ||
			ret.route.Origin.ProvinceCode != out.route.Dest.ProvinceCode ||
			ret.route.Dest.ProvinceCode != out.route.Origin.ProvinceCode {
			return rideRef{}, uuid.Nil, nil, ErrInvalidReturnTrip
		}
	}

	// ride_trips FOR UPDATE in id order, then the overdue holds on them.
	trips, err := s.listings.LockTrips(ctx, tx, req.tripIDs)
	if errors.Is(err, listing.ErrTripNotFound) {
		return rideRef{}, uuid.Nil, nil, ErrTripUnavailable
	}
	if err != nil {
		return rideRef{}, uuid.Nil, nil, err
	}
	for i, id := range req.tripIDs {
		j := slices.IndexFunc(trips, func(t listing.Trip) bool { return t.ID == id })
		legs[i].trip = trips[j]
	}
	codes, err := s.releaseOverdueOnTrips(ctx, tx, req.tripIDs, uuid.Nil, now)
	if err != nil {
		return rideRef{}, uuid.Nil, nil, err
	}

	cfg, err := s.settings.GetTx(ctx, tx)
	if err != nil {
		return rideRef{}, uuid.Nil, nil, err
	}
	cutoff := now.Add(time.Duration(max(cfg.RideBookingCutoffMinutes, cfg.HoldMinutes+cfg.HoldGraceMinutes)) * time.Minute)
	for _, l := range legs {
		if l.trip.Status != tripScheduled || !l.trip.DepartureAt.After(cutoff) {
			return rideRef{}, uuid.Nil, nil, ErrTripUnavailable
		}
	}
	if len(legs) == 2 && legs[1].trip.DepartureAt.Before(legs[0].arriveAt()) {
		return rideRef{}, uuid.Nil, nil, ErrInvalidReturnTrip
	}
	for _, l := range legs {
		ok, err := s.listings.ReserveSeats(ctx, tx, l.trip.ID, req.in.Seats, now)
		if err != nil {
			return rideRef{}, uuid.Nil, nil, err
		}
		if !ok {
			return rideRef{}, uuid.Nil, nil, ErrNotEnoughSeats
		}
	}

	rateBps, err := s.catalog.CurrentCommissionRate(ctx, tx, serviceTypeRide, now)
	if err != nil {
		return rideRef{}, uuid.Nil, nil, err
	}
	var total int64
	for _, l := range legs {
		total += l.trip.PricePerSeat * int64(req.in.Seats)
	}
	holdExpires := now.Add(time.Duration(cfg.HoldMinutes) * time.Minute)
	holdRelease := holdExpires.Add(time.Duration(cfg.HoldGraceMinutes) * time.Minute)

	q := store.New(tx)
	checkoutID, err := s.insertCheckout(ctx, q, customerID, total, now)
	if err != nil {
		return rideRef{}, uuid.Nil, nil, err
	}
	ref := rideRef{CheckoutID: checkoutID}
	for _, l := range legs {
		id, err := s.insertRideBooking(ctx, q, tx, customerID, checkoutID, req, l, rateBps, holdExpires, holdRelease, now)
		if err != nil {
			return rideRef{}, uuid.Nil, nil, err
		}
		ref.BookingIDs = append(ref.BookingIDs, id)
	}
	pay, err := s.payments.ReservePayment(ctx, tx, checkoutID, total, holdExpires)
	if err != nil {
		return rideRef{}, uuid.Nil, nil, err
	}
	return ref, pay.ID, codes, nil
}

// insertRideBooking inserts the pending_payment booking of one leg with its
// seat line, ride details and first history row.
func (s *Service) insertRideBooking(ctx context.Context, q *store.Queries, tx pgx.Tx, customerID, checkoutID uuid.UUID,
	req rideRequest, l rideLeg, rateBps int, holdExpires, holdRelease, now time.Time) (uuid.UUID, error) {
	seats := req.in.Seats
	gross := l.trip.PricePerSeat * int64(seats)
	policy, err := s.listings.CancellationPolicy(ctx, tx, l.listing.ID)
	if err != nil {
		return uuid.Nil, err
	}
	snapshot, err := json.Marshal(policy)
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal policy snapshot: %w", err)
	}
	qrToken, err := randomToken()
	if err != nil {
		return uuid.Nil, err
	}
	c := req.contact
	bookingID, _, err := insertPendingBooking(ctx, q, store.InsertPendingBookingParams{
		CheckoutID: checkoutID, CustomerID: customerID,
		PartnerID: l.listing.PartnerID, ListingID: l.listing.ID, ServiceType: serviceTypeRide,
		ServiceStartAt: l.trip.DepartureAt, ServiceEndAt: l.arriveAt(),
		GrossAmount: gross, CommissionRateBps: int32(rateBps), CommissionAmount: money.MulBps(gross, rateBps),
		CancellationPolicySnapshot: snapshot,
		ContactName:                c.name, ContactPhone: c.phone, ContactEmail: c.mail, CustomerNote: c.note,
		HoldExpiresAt: &holdExpires, HoldReleaseAt: &holdRelease, QrToken: qrToken, Now: now,
	})
	if err != nil {
		return uuid.Nil, err
	}
	itemID, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	desc := fmt.Sprintf("Ghế %s → %s, %s", l.route.Origin.Name, l.route.Dest.Name, l.trip.DepartureAt.In(clock.VN).Format(seatTimeLayout))
	if err := q.InsertBookingItem(ctx, store.InsertBookingItemParams{
		ID: itemID, BookingID: bookingID, LineKind: lineKindSeat, Description: desc,
		UnitPrice: l.trip.PricePerSeat, Quantity: int32(seats), LineTotal: gross, Now: now,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("insert booking item: %w", err)
	}
	if err := q.InsertRideDetail(ctx, store.InsertRideDetailParams{
		BookingID: bookingID, ListingID: l.listing.ID, TripID: l.trip.ID, Seats: int32(seats),
		Direction: l.direction, OriginName: l.route.Origin.Name, DestName: l.route.Dest.Name, Now: now,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("insert ride detail: %w", err)
	}
	histID, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	if err := q.InsertStatusHistory(ctx, store.InsertStatusHistoryParams{
		ID: histID, BookingID: bookingID, ToStatus: StatusPendingPayment,
		ActorKind: actorCustomer, ActorUserID: &customerID, Now: now,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("insert status history: %w", err)
	}
	return bookingID, nil
}
