package listing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// maxSearchDays is how far ahead (VN calendar days) a trip search may look.
const maxSearchDays = 365

// TripSearchFilter selects bookable trips of one route on one day. Date is a
// Vietnam calendar date (YYYY-MM-DD). Seats is the number of passengers: 0
// means 1, at most MaxSeatsPerOperation. Blank wards are no filter. Limit
// defaults to 20 and is capped at 50; Offset is at most 1000.
type TripSearchFilter struct {
	OriginProvince, DestProvince string
	OriginWard, DestWard         *string
	Date                         string
	Seats, Limit, Offset         int
}

// ListingRef names the listing a trip belongs to.
type ListingRef struct {
	ID    uuid.UUID `json:"id"`
	Slug  string    `json:"slug"`
	Title string    `json:"title"`
}

// TripCard is a trip as shown in public trip search.
type TripCard struct {
	TripID             uuid.UUID  `json:"trip_id"`
	Listing            ListingRef `json:"listing"`
	Origin             RidePoint  `json:"origin"`
	Dest               RidePoint  `json:"dest"`
	DepartureAt        time.Time  `json:"departure_at"`
	ArriveAt           time.Time  `json:"arrive_at"`
	DurationMinutes    int        `json:"duration_minutes"`
	VehicleDescription string     `json:"vehicle_description"`
	PricePerSeat       int64      `json:"price_per_seat"`
	SeatsLeft          int        `json:"seats_left"`
}

// SearchTrips returns the trips of a route departing on f.Date (Vietnam
// time) that a customer can still book for f.Seats passengers: scheduled, with
// enough free seats, departing later than now plus the booking cutoff
// (max(ride_booking_cutoff_minutes, hold_minutes + hold_grace_minutes), read
// from platform_settings in the same statement), of a publicly visible
// listing. Earliest departure first, then cheapest. A bad parameter is
// httpx.ErrInvalidRequest.
func (s *Service) SearchTrips(ctx context.Context, f TripSearchFilter) ([]TripCard, error) {
	origin, dest := strings.TrimSpace(f.OriginProvince), strings.TrimSpace(f.DestProvince)
	if origin == "" || dest == "" {
		return nil, httpx.ErrInvalidRequest
	}
	if f.Limit < 0 || f.Offset < 0 || f.Offset > maxSearchOffset || f.Seats < 0 || f.Seats > MaxSeatsPerOperation {
		return nil, httpx.ErrInvalidRequest
	}
	seats := f.Seats
	if seats == 0 {
		seats = 1
	}
	limit := f.Limit
	if limit == 0 {
		limit = defaultSearchLimit
	}
	limit = min(limit, maxSearchLimit)
	now := s.clock.Now()
	day, err := time.ParseInLocation(dateLayout, f.Date, clock.VN)
	if err != nil {
		return nil, httpx.ErrInvalidRequest
	}
	n := now.In(clock.VN)
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, clock.VN)
	if day.Before(today) || day.After(today.AddDate(0, 0, maxSearchDays)) {
		return nil, httpx.ErrInvalidRequest
	}
	out := []TripCard{}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := store.New(tx).SearchRideTrips(ctx, store.SearchRideTripsParams{
			OriginProvince: origin, DestProvince: dest,
			OriginWard: blankToNil(f.OriginWard), DestWard: blankToNil(f.DestWard),
			DayStart: day, DayEnd: day.AddDate(0, 0, 1), Now: now, Seats: int32(seats),
			Lim: int32(limit), Off: int32(f.Offset),
		})
		if err != nil {
			return fmt.Errorf("search ride trips: %w", err)
		}
		for _, r := range rows {
			out = append(out, TripCard{
				TripID:             r.TripID,
				Listing:            ListingRef{ID: r.ListingID, Slug: r.Slug, Title: r.Title},
				Origin:             RidePoint{ProvinceCode: r.OriginProvinceCode, WardCode: r.OriginWardCode, Name: r.OriginName, Address: r.OriginAddress},
				Dest:               RidePoint{ProvinceCode: r.DestProvinceCode, WardCode: r.DestWardCode, Name: r.DestName, Address: r.DestAddress},
				DepartureAt:        r.DepartureAt,
				ArriveAt:           r.DepartureAt.Add(time.Duration(r.DurationMinutes) * time.Minute),
				DurationMinutes:    int(r.DurationMinutes),
				VehicleDescription: r.VehicleDescription,
				PricePerSeat:       r.PricePerSeat,
				SeatsLeft:          int(r.SeatsLeft),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
