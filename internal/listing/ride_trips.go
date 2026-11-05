package listing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// TripCancelled is the status of a cancelled trip.
const TripCancelled = "cancelled"

const (
	tripScheduled       = "scheduled"
	maxTripAheadDays    = 365
	maxTripListDays     = 62
	maxTripCancelReason = 500
	actionRideTrip      = "ride_trip"
	// tripTimeUnique is the UNIQUE (listing_id, departure_at) of ride_trips.
	tripTimeUnique = "ride_trips_listing_id_departure_at_key"
)

var (
	ErrTripNotFound          = httpx.NewError(http.StatusNotFound, "not_found", "Không tìm thấy chuyến")
	ErrTripHasBookings       = httpx.NewError(http.StatusConflict, "trip_has_bookings", "Chuyến đã có ghế được giữ, không thể đổi giờ, đổi giá hoặc hủy")
	ErrCapacityBelowReserved = httpx.NewError(http.StatusConflict, "capacity_below_reserved", "Sức chứa mới không được nhỏ hơn số ghế đã được giữ")
	ErrTripTimeTaken         = httpx.NewError(http.StatusConflict, "trip_time_taken", "Tuyến đã có chuyến khởi hành vào giờ này")
	ErrTripNotScheduled      = httpx.NewError(http.StatusConflict, "trip_not_scheduled", "Chuyến không còn ở trạng thái chờ khởi hành")
)

// Trip is one departure of a ride listing. ScheduleID is nil for a one-off
// trip.
type Trip struct {
	ID              uuid.UUID  `json:"id"`
	ListingID       uuid.UUID  `json:"listing_id"`
	ScheduleID      *uuid.UUID `json:"schedule_id"`
	DepartureAt     time.Time  `json:"departure_at"`
	SeatCapacity    int        `json:"seat_capacity"`
	SeatsReserved   int        `json:"seats_reserved"`
	PricePerSeat    int64      `json:"price_per_seat"`
	Status          string     `json:"status"`
	CancelledReason *string    `json:"cancelled_reason"`
}

// SeatsLeft is the number of seats still free (never negative).
func (t Trip) SeatsLeft() int { return max(t.SeatCapacity-t.SeatsReserved, 0) }

// MarshalJSON adds seats_left.
func (t Trip) MarshalJSON() ([]byte, error) {
	type plain Trip
	return json.Marshal(struct {
		plain
		SeatsLeft int `json:"seats_left"`
	}{plain(t), t.SeatsLeft()})
}

// TripInput creates a one-off trip.
type TripInput struct {
	DepartureAt  time.Time `json:"departure_at"`
	SeatCapacity int       `json:"seat_capacity"`
	PricePerSeat int64     `json:"price_per_seat"`
}

// TripPatch updates a trip; nil fields are left unchanged.
type TripPatch struct {
	DepartureAt  *time.Time `json:"departure_at"`
	SeatCapacity *int       `json:"seat_capacity"`
	PricePerSeat *int64     `json:"price_per_seat"`
}

func tripFromRow(r store.RideTrip) Trip {
	return Trip{
		ID: r.ID, ListingID: r.ListingID, ScheduleID: r.ScheduleID, DepartureAt: r.DepartureAt,
		SeatCapacity: int(r.SeatCapacity), SeatsReserved: int(r.SeatsReserved), PricePerSeat: r.PricePerSeat,
		Status: r.Status, CancelledReason: r.CancelledReason,
	}
}

// tripFields is the editable state of a trip.
type tripFields struct {
	DepartureAt  time.Time
	SeatCapacity int
	PricePerSeat int64
}

// validate checks the bounds of a trip: seats 1..80, price
// 1,000..100,000,000 VND and, when checkTime is set, a departure in the
// future and at most 365 days ahead.
func (f tripFields) validate(now time.Time, checkTime bool) error {
	if checkTime && !f.DepartureAt.After(now) {
		return invalidRide("Giờ khởi hành phải ở tương lai")
	}
	if checkTime && f.DepartureAt.After(now.AddDate(0, 0, maxTripAheadDays)) {
		return invalidRide(fmt.Sprintf("Giờ khởi hành không được quá %d ngày tới", maxTripAheadDays))
	}
	if f.SeatCapacity < 1 || f.SeatCapacity > maxRideSeats {
		return invalidRide(fmt.Sprintf("Số ghế phải từ 1 đến %d", maxRideSeats))
	}
	if f.PricePerSeat < minRideSeatPrice || f.PricePerSeat > maxRideSeatPrice {
		return invalidRide(fmt.Sprintf("Giá mỗi ghế phải từ %d đến %d VND", minRideSeatPrice, maxRideSeatPrice))
	}
	return nil
}

func (f tripFields) text() map[string]any {
	return map[string]any{
		"departure_at":   f.DepartureAt.In(clock.VN).Format(time.RFC3339),
		"seat_capacity":  f.SeatCapacity,
		"price_per_seat": f.PricePerSeat,
	}
}

func isTripTimeTaken(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == tripTimeUnique
}

// checkTripRange bounds a trip listing window: from < to, at most 62 days.
func checkTripRange(from, to time.Time) error {
	if !from.Before(to) {
		return invalidRide("Thời điểm bắt đầu phải trước thời điểm kết thúc")
	}
	if to.Sub(from) > maxTripListDays*24*time.Hour {
		return invalidRide(fmt.Sprintf("Khoảng thời gian tối đa là %d ngày", maxTripListDays))
	}
	return nil
}

func listTrips(ctx context.Context, q *store.Queries, listingID uuid.UUID, from, to time.Time) ([]Trip, error) {
	rows, err := q.ListListingTrips(ctx, store.ListListingTripsParams{ListingID: listingID, FromAt: from, ToAt: to})
	if err != nil {
		return nil, fmt.Errorf("list ride trips: %w", err)
	}
	out := make([]Trip, 0, len(rows))
	for _, r := range rows {
		out = append(out, tripFromRow(r))
	}
	return out, nil
}

// ListTrips returns the trips (every status) of the partner's ride listing
// departing in [from, to), by departure. The window is at most 62 days.
func (s *Service) ListTrips(ctx context.Context, partnerID, listingID uuid.UUID, from, to time.Time) ([]Trip, error) {
	l, err := s.Get(ctx, partnerID, listingID)
	if err != nil {
		return nil, err
	}
	if l.ServiceType != rideServiceType {
		return nil, ErrNotRideListing
	}
	if err := checkTripRange(from, to); err != nil {
		return nil, err
	}
	return listTrips(ctx, store.New(s.pool), listingID, from, to)
}

// AdminListTrips is ListTrips for any listing (Super Admin).
func (s *Service) AdminListTrips(ctx context.Context, listingID uuid.UUID, from, to time.Time) ([]Trip, error) {
	q := store.New(s.pool)
	l, err := q.GetListing(ctx, listingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrListingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get listing: %w", err)
	}
	if l.ServiceType != rideServiceType {
		return nil, ErrNotRideListing
	}
	if err := checkTripRange(from, to); err != nil {
		return nil, err
	}
	return listTrips(ctx, q, listingID, from, to)
}

// CreateTrip adds a one-off trip (no schedule) to a ride listing that has a
// route. Another trip of the listing at the same instant, whatever its
// status, is ErrTripTimeTaken.
func (s *Service) CreateTrip(ctx context.Context, partnerID, actor, listingID uuid.UUID, in TripInput) (Trip, error) {
	now := s.clock.Now()
	var out Trip
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockRide(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		f := tripFields{DepartureAt: in.DepartureAt, SeatCapacity: in.SeatCapacity, PricePerSeat: in.PricePerSeat}
		if err := f.validate(now, true); err != nil {
			return err
		}
		q := store.New(tx)
		if _, err := q.GetRideRoute(ctx, listingID); errors.Is(err, pgx.ErrNoRows) {
			return ErrRouteRequired
		} else if err != nil {
			return fmt.Errorf("get ride route: %w", err)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("ride trip id: %w", err)
		}
		row, err := q.InsertOneOffTrip(ctx, store.InsertOneOffTripParams{
			ID: id, ListingID: listingID, DepartureAt: f.DepartureAt,
			SeatCapacity: int32(f.SeatCapacity), PricePerSeat: f.PricePerSeat, Now: now,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTripTimeTaken
		}
		if err != nil {
			return fmt.Errorf("insert ride trip: %w", err)
		}
		out = tripFromRow(row)
		changes := fieldDiff(map[string]any{}, f.text())
		changes["id"] = change{nil, id.String()}
		return s.logChange(ctx, tx, listingID, actor, actionRideTrip, changes, now)
	})
	if err != nil {
		return Trip{}, err
	}
	return out, nil
}

// lockPartnerTrip runs lockRide (partner FOR SHARE -> listing FOR NO KEY
// UPDATE) and then locks the trip FOR UPDATE. A trip of another listing is
// ErrTripNotFound. The trip must still be scheduled and in the future, or
// ErrTripNotScheduled.
func (s *Service) lockPartnerTrip(ctx context.Context, tx pgx.Tx, partnerID, listingID, tripID uuid.UUID, now time.Time) (store.RideTrip, error) {
	if err := s.lockRide(ctx, tx, partnerID, listingID); err != nil {
		return store.RideTrip{}, err
	}
	row, err := store.New(tx).LockListingTrip(ctx, store.LockListingTripParams{ID: tripID, ListingID: listingID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.RideTrip{}, ErrTripNotFound
	}
	if err != nil {
		return store.RideTrip{}, fmt.Errorf("lock ride trip: %w", err)
	}
	if row.Status != tripScheduled || !row.DepartureAt.After(now) {
		return store.RideTrip{}, ErrTripNotScheduled
	}
	return row, nil
}

// UpdateTrip edits a scheduled future trip. Its time or price only change
// while no seat is reserved (ErrTripHasBookings); its capacity never drops
// below the reserved seats (ErrCapacityBelowReserved). A trip moved to
// another time leaves its schedule and becomes a one-off, so the schedule
// neither edits nor cancels it any more.
func (s *Service) UpdateTrip(ctx context.Context, partnerID, actor, listingID, tripID uuid.UUID, in TripPatch) (Trip, error) {
	now := s.clock.Now()
	var out Trip
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := s.lockPartnerTrip(ctx, tx, partnerID, listingID, tripID, now)
		if err != nil {
			return err
		}
		old := tripFields{DepartureAt: cur.DepartureAt, SeatCapacity: int(cur.SeatCapacity), PricePerSeat: cur.PricePerSeat}
		f := old
		if in.DepartureAt != nil {
			f.DepartureAt = *in.DepartureAt
		}
		if in.SeatCapacity != nil {
			f.SeatCapacity = *in.SeatCapacity
		}
		if in.PricePerSeat != nil {
			f.PricePerSeat = *in.PricePerSeat
		}
		moved := !f.DepartureAt.Equal(old.DepartureAt)
		if (moved || f.PricePerSeat != old.PricePerSeat) && cur.SeatsReserved > 0 {
			return ErrTripHasBookings
		}
		if f.SeatCapacity < int(cur.SeatsReserved) {
			return ErrCapacityBelowReserved
		}
		// Only a new departure has to pass the time bounds.
		if err := f.validate(now, moved); err != nil {
			return err
		}
		changes := fieldDiff(old.text(), f.text())
		if len(changes) == 0 {
			out = tripFromRow(cur)
			return nil
		}
		scheduleID := cur.ScheduleID
		if moved {
			scheduleID = nil
		}
		row, err := store.New(tx).UpdateTripFields(ctx, store.UpdateTripFieldsParams{
			ID: tripID, DepartureAt: f.DepartureAt, SeatCapacity: int32(f.SeatCapacity),
			PricePerSeat: f.PricePerSeat, ScheduleID: scheduleID, Now: now,
		})
		if isTripTimeTaken(err) {
			return ErrTripTimeTaken
		}
		if err != nil {
			return fmt.Errorf("update ride trip: %w", err)
		}
		out = tripFromRow(row)
		changes["id"] = change{tripID.String(), tripID.String()}
		return s.logChange(ctx, tx, listingID, actor, actionRideTrip, changes, now)
	})
	if err != nil {
		return Trip{}, err
	}
	return out, nil
}

func normalizeCancelReason(reason string) (string, error) {
	r, err := normalizeText(reason, "Lý do hủy", true, maxTripCancelReason)
	if err != nil {
		return "", asInvalidRide(err)
	}
	return r, nil
}

// CancelTripByPartner cancels a scheduled future trip that has no reserved
// seat (ErrTripHasBookings otherwise), with cancel_source partner. reason is
// 1..500 characters.
func (s *Service) CancelTripByPartner(ctx context.Context, partnerID, actor, listingID, tripID uuid.UUID, reason string) (Trip, error) {
	now := s.clock.Now()
	var out Trip
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := s.lockPartnerTrip(ctx, tx, partnerID, listingID, tripID, now)
		if err != nil {
			return err
		}
		r, err := normalizeCancelReason(reason)
		if err != nil {
			return err
		}
		if cur.SeatsReserved > 0 {
			return ErrTripHasBookings
		}
		row, err := s.cancelTrip(ctx, tx, cur, actor, r, "partner", now)
		if err != nil {
			return err
		}
		out = tripFromRow(row)
		return nil
	})
	if err != nil {
		return Trip{}, err
	}
	return out, nil
}

// cancelTrip marks a locked scheduled trip cancelled and logs it on the
// listing.
func (s *Service) cancelTrip(ctx context.Context, tx pgx.Tx, cur store.RideTrip, actor uuid.UUID, reason, source string, now time.Time) (store.RideTrip, error) {
	row, err := store.New(tx).CancelTrip(ctx, store.CancelTripParams{
		ID: cur.ID, Reason: reason, Actor: actor, Source: source, Now: now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.RideTrip{}, ErrTripNotScheduled
	}
	if err != nil {
		return store.RideTrip{}, fmt.Errorf("cancel ride trip: %w", err)
	}
	changes := map[string]change{
		"id":            {cur.ID.String(), cur.ID.String()},
		"status":        {cur.Status, row.Status},
		"cancel_source": {nil, source},
		"reason":        {nil, reason},
	}
	if err := s.logChange(ctx, tx, cur.ListingID, actor, actionRideTrip, changes, now); err != nil {
		return store.RideTrip{}, err
	}
	return row, nil
}

// --- Seat primitives for booking. Each runs in the caller's tx. ---

// RideRoute returns the route of a ride listing; a listing without one is
// ErrListingNotFound.
func (s *Service) RideRoute(ctx context.Context, tx pgx.Tx, listingID uuid.UUID) (RideRoute, error) {
	r, err := store.New(tx).GetRideRoute(ctx, listingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RideRoute{}, ErrListingNotFound
	}
	if err != nil {
		return RideRoute{}, fmt.Errorf("get ride route: %w", err)
	}
	return routeFromRow(r), nil
}

func dedupeIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return slices.Compact(out)
}

// TripListingIDs maps each trip id to its listing id, without locking. A
// missing id is ErrTripNotFound.
func (s *Service) TripListingIDs(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	uniq := dedupeIDs(ids)
	rows, err := store.New(tx).TripListingIDs(ctx, uniq)
	if err != nil {
		return nil, fmt.Errorf("trip listing ids: %w", err)
	}
	if len(rows) != len(uniq) {
		return nil, ErrTripNotFound
	}
	out := make(map[uuid.UUID]uuid.UUID, len(rows))
	for _, r := range rows {
		out[r.ID] = r.ListingID
	}
	return out, nil
}

// TripStatus reads a trip's status without locking (ride check-in locks the
// booking, not the trip). A missing trip is ErrTripNotFound.
func (s *Service) TripStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID) (string, error) {
	row, err := store.New(tx).GetRideTrip(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTripNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get ride trip: %w", err)
	}
	return row.Status, nil
}

// LockTrips locks the trips FOR UPDATE in id order (DB §1.1), duplicates
// removed, and returns them in that order. A missing id is ErrTripNotFound.
func (s *Service) LockTrips(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) ([]Trip, error) {
	uniq := dedupeIDs(ids)
	rows, err := store.New(tx).LockTrips(ctx, uniq)
	if err != nil {
		return nil, fmt.Errorf("lock ride trips: %w", err)
	}
	if len(rows) != len(uniq) {
		return nil, ErrTripNotFound
	}
	out := make([]Trip, 0, len(rows))
	for _, r := range rows {
		out = append(out, tripFromRow(r))
	}
	return out, nil
}

// MaxSeatsPerOperation bounds n in ReserveSeats and ReleaseSeats (1..1000).
// It is an overflow guard, not a business cap: n is passed to the database
// as int32, so an unchecked huge n would wrap negative. Booking validates the
// requested seats against the same bound.
const MaxSeatsPerOperation = 1000

func validSeatCount(n int) bool { return n >= 1 && n <= MaxSeatsPerOperation }

// ReserveSeats adds n reserved seats to a trip that is scheduled, departs
// after now and still has n free seats, in one conditional UPDATE (spec §7).
// It reports false, changing nothing, when any condition fails or n is
// outside 1..MaxSeatsPerOperation.
func (s *Service) ReserveSeats(ctx context.Context, tx pgx.Tx, tripID uuid.UUID, n int, now time.Time) (bool, error) {
	if !validSeatCount(n) {
		return false, nil
	}
	rows, err := store.New(tx).ReserveSeats(ctx, store.ReserveSeatsParams{ID: tripID, N: int32(n), Now: now})
	if err != nil {
		return false, fmt.Errorf("reserve seats: %w", err)
	}
	return rows == 1, nil
}

// ReleaseSeats returns n reserved seats of a trip, whatever its status. The
// CHECK on seats_reserved fails the statement if it would go below zero. A
// missing trip is ErrTripNotFound; n outside 1..MaxSeatsPerOperation is an
// error and changes nothing.
func (s *Service) ReleaseSeats(ctx context.Context, tx pgx.Tx, tripID uuid.UUID, n int) error {
	if !validSeatCount(n) {
		return fmt.Errorf("release seats: n must be 1..%d, got %d", MaxSeatsPerOperation, n)
	}
	rows, err := store.New(tx).ReleaseSeats(ctx, store.ReleaseSeatsParams{ID: tripID, N: int32(n)})
	if err != nil {
		return fmt.Errorf("release seats: %w", err)
	}
	if rows == 0 {
		return ErrTripNotFound
	}
	return nil
}

// CancelTripByAdmin cancels a scheduled trip whatever its reserved seats,
// with cancel_source admin. The caller holds the trip lock (LockTrips) and
// cancels and refunds the bookings, releasing their seats, in the same tx. A
// trip that is no longer scheduled is ErrTripNotScheduled. reason is 1..500
// characters.
func (s *Service) CancelTripByAdmin(ctx context.Context, tx pgx.Tx, tripID, actor uuid.UUID, reason string, now time.Time) error {
	cur, err := store.New(tx).GetRideTrip(ctx, tripID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTripNotFound
	}
	if err != nil {
		return fmt.Errorf("get ride trip: %w", err)
	}
	if cur.Status != tripScheduled {
		return ErrTripNotScheduled
	}
	r, err := normalizeCancelReason(reason)
	if err != nil {
		return err
	}
	_, err = s.cancelTrip(ctx, tx, cur, actor, r, "admin", now)
	return err
}
