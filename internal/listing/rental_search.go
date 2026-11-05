package listing

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// maxRentalCandidates caps the models examined by one search.
const maxRentalCandidates = 1000

// RentalSearchFilter selects the vehicle models a customer can rent in
// [PickupAt, ReturnAt) in one province. VehicleKind nil is any kind; MinSeats
// is 0..50. Limit defaults to 20 and is capped at 50; Offset is at most 1000.
type RentalSearchFilter struct {
	ProvinceCode       string
	PickupAt, ReturnAt time.Time
	VehicleKind        *string
	MinSeats           int
	Limit, Offset      int
}

// RentalVehicleCard is a vehicle model as shown in public rental search.
// RentalTotal is price x days, the delivery fee of the pickup point not
// included.
type RentalVehicleCard struct {
	ModelID       uuid.UUID  `json:"model_id"`
	Listing       ListingRef `json:"listing"`
	VehicleKind   string     `json:"vehicle_kind"`
	Name          string     `json:"name"`
	ModelYear     int        `json:"model_year"`
	Seats         int        `json:"seats"`
	Tags          []string   `json:"tags"`
	CoverURL      *string    `json:"cover_url"`
	PricePerDay   int64      `json:"price_per_day"`
	Days          int        `json:"days"`
	RentalTotal   int64      `json:"rental_total"`
	UnitsLeft     int        `json:"units_left"`
	StoreAddress  string     `json:"store_address"`
	HandoverOpen  string     `json:"handover_open"`
	HandoverClose string     `json:"handover_close"`
}

// SearchRentalVehicles returns the models with at least one vehicle free
// during the whole of [PickupAt, ReturnAt): UnitsLeft = quantity - the peak
// number of vehicles held at once in the period (BookingGuard.RentalPeakHeld).
// Only models of publicly visible listings with an active pickup point whose
// store is open at both the pickup and the return time (Vietnam time) are
// offered. Cheapest per day first, then by model id. Any bad parameter,
// including a period CheckRentalPeriod rejects, is httpx.ErrInvalidRequest.
func (s *Service) SearchRentalVehicles(ctx context.Context, f RentalSearchFilter) ([]RentalVehicleCard, error) {
	province := strings.TrimSpace(f.ProvinceCode)
	if province == "" || f.MinSeats < 0 || f.MinSeats > maxModelSeats ||
		f.Limit < 0 || f.Offset < 0 || f.Offset > maxSearchOffset {
		return nil, httpx.ErrInvalidRequest
	}
	kind := blankToNil(f.VehicleKind)
	if kind != nil && *kind != "car" && *kind != "motorbike" {
		return nil, httpx.ErrInvalidRequest
	}
	limit := f.Limit
	if limit == 0 {
		limit = defaultSearchLimit
	}
	limit = min(limit, maxSearchLimit)
	now := s.clock.Now()
	out := []RentalVehicleCard{}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		paymentHours, err := q.RequestPaymentHours(ctx)
		if err != nil {
			return fmt.Errorf("read request_payment_hours: %w", err)
		}
		days, err := CheckRentalPeriod(f.PickupAt, f.ReturnAt, now, int(paymentHours))
		if err != nil {
			return httpx.ErrInvalidRequest
		}
		rows, err := q.ListRentalCandidates(ctx, store.ListRentalCandidatesParams{
			ProvinceCode: &province, MinSeats: int32(f.MinSeats), VehicleKind: kind,
			PickupAt: f.PickupAt, ReturnAt: f.ReturnAt, Cap: maxRentalCandidates,
		})
		if err != nil {
			return fmt.Errorf("list rental candidates: %w", err)
		}
		ids := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.RentalVehicleModel.ID
		}
		var peak map[uuid.UUID]int
		if s.bookings != nil && len(ids) > 0 {
			ret := f.ReturnAt
			if peak, err = s.bookings.RentalPeakHeld(ctx, tx, ids, f.PickupAt, &ret, now); err != nil {
				return err
			}
		}
		var free []store.ListRentalCandidatesRow
		left := map[uuid.UUID]int{}
		for _, r := range rows {
			m := r.RentalVehicleModel
			if n := int(m.Quantity) - peak[m.ID]; n >= 1 {
				free = append(free, r)
				left[m.ID] = n
			}
		}
		if f.Offset >= len(free) {
			return nil
		}
		for _, r := range free[f.Offset:min(len(free), f.Offset+limit)] {
			m := r.RentalVehicleModel
			c := RentalVehicleCard{
				ModelID: m.ID, Listing: ListingRef{ID: m.ListingID, Slug: r.ListingSlug, Title: r.ListingTitle},
				VehicleKind: m.VehicleKind, Name: m.Name, ModelYear: int(m.ModelYear), Seats: int(m.Seats),
				Tags: slices.Clone(m.Tags), PricePerDay: m.PricePerDay, Days: days,
				RentalTotal: m.PricePerDay * int64(days), UnitsLeft: left[m.ID],
				HandoverOpen: clockText(r.HandoverOpen), HandoverClose: clockText(r.HandoverClose),
			}
			if c.Tags == nil {
				c.Tags = []string{}
			}
			if r.ListingAddress != nil {
				c.StoreAddress = *r.ListingAddress
			}
			cover := m.CoverFileID
			if cover == nil {
				cover = r.ListingCoverFileID
			}
			if cover != nil {
				u, err := s.files.PublicURL(ctx, tx, *cover)
				if err != nil {
					return fmt.Errorf("cover url of model %s: %w", m.ID, err)
				}
				c.CoverURL = &u
			}
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
