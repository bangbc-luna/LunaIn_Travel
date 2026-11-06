package listing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

const (
	maxCharterClassName = 100
	maxCharterSeats     = 50
	maxBaseFare         = 50_000_000
	maxPricePerKm       = 1_000_000
	maxFixedSurcharge   = 5_000_000
	maxPercentBps       = 10_000

	// Surcharge kinds and how an amount is read.
	SurchargeNight            = "night"
	SurchargeWeekend          = "weekend"
	SurchargeHoliday          = "holiday"
	SurchargeRoundTripWaiting = "round_trip_waiting"
	CalcFixed                 = "fixed"
	CalcPercent               = "percent"

	actionCharterClass      = "charter_class"
	actionCharterSurcharges = "charter_surcharges"
)

// surchargeKinds is the order surcharges are listed and priced in.
var surchargeKinds = []string{SurchargeNight, SurchargeWeekend, SurchargeHoliday, SurchargeRoundTripWaiting}

// Surcharge is one surcharge of a vehicle class. Amount is VND when Calc is
// fixed and basis points when percent. A surcharge applies only while Active
// and Amount > 0.
type Surcharge struct {
	Kind   string `json:"kind"`
	Calc   string `json:"calc"`
	Amount int64  `json:"amount"`
	Active bool   `json:"active"`
}

// SurchargeInput is one entry of the surcharge list a partner sends.
type SurchargeInput = Surcharge

// UnmarshalJSON decodes a surcharge; active defaults to true when omitted
// and unknown fields are an error (400).
func (s *Surcharge) UnmarshalJSON(b []byte) error {
	raw := struct {
		Kind   string `json:"kind"`
		Calc   string `json:"calc"`
		Amount int64  `json:"amount"`
		Active *bool  `json:"active"`
	}{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	*s = Surcharge{Kind: raw.Kind, Calc: raw.Calc, Amount: raw.Amount, Active: raw.Active == nil || *raw.Active}
	return nil
}

// CharterClass is a vehicle class of a charter listing: the fare is
// BaseFare + km × PricePerKm plus the surcharges.
type CharterClass struct {
	ID         uuid.UUID   `json:"id"`
	ListingID  uuid.UUID   `json:"listing_id"`
	Name       string      `json:"name"`
	Seats      int         `json:"seats"`
	BaseFare   int64       `json:"base_fare"`
	PricePerKm int64       `json:"price_per_km"`
	Active     bool        `json:"active"`
	Surcharges []Surcharge `json:"surcharges"`
}

// CharterClassInput creates a vehicle class (always active): name 1..100
// characters, seats 1..50, base_fare 0..50,000,000 and price_per_km
// 1..1,000,000 VND (a trip is never free).
type CharterClassInput struct {
	Name       string `json:"name"`
	Seats      int    `json:"seats"`
	BaseFare   int64  `json:"base_fare"`
	PricePerKm int64  `json:"price_per_km"`
}

// CharterClassPatch updates a vehicle class; nil fields are left unchanged. A
// new price applies to new quotes only: bookings snapshot their price.
type CharterClassPatch struct {
	Name       *string `json:"name"`
	Seats      *int    `json:"seats"`
	BaseFare   *int64  `json:"base_fare"`
	PricePerKm *int64  `json:"price_per_km"`
	Active     *bool   `json:"active"`
}

func classFromRow(r store.CharterVehicleClass) CharterClass {
	return CharterClass{
		ID: r.ID, ListingID: r.ListingID, Name: r.Name, Seats: int(r.Seats),
		BaseFare: r.BaseFare, PricePerKm: r.PricePerKm, Active: r.Active, Surcharges: []Surcharge{},
	}
}

func surchargeFromRow(r store.CharterSurcharge) Surcharge {
	return Surcharge{Kind: r.Kind, Calc: r.Calc, Amount: r.Amount, Active: r.Active}
}

func (in CharterClassInput) normalize() (CharterClassInput, error) {
	var err error
	if in.Name, err = normalizeText(in.Name, "Tên loại xe", true, maxCharterClassName); err != nil {
		return in, asInvalidCharter(err)
	}
	if in.Seats < 1 || in.Seats > maxCharterSeats {
		return in, invalidCharter(fmt.Sprintf("Số chỗ phải từ 1 đến %d", maxCharterSeats))
	}
	if in.BaseFare < 0 || in.BaseFare > maxBaseFare {
		return in, invalidCharter(fmt.Sprintf("Giá mở cửa phải từ 0 đến %d VND", maxBaseFare))
	}
	if in.PricePerKm <= 0 {
		return in, invalidCharter("Giá mỗi km phải lớn hơn 0")
	}
	if in.PricePerKm > maxPricePerKm {
		return in, invalidCharter(fmt.Sprintf("Giá mỗi km phải từ 1 đến %d VND", maxPricePerKm))
	}
	return in, nil
}

func classFields(c CharterClass) map[string]any {
	return map[string]any{
		"name": c.Name, "seats": c.Seats, "base_fare": c.BaseFare, "price_per_km": c.PricePerKm, "active": c.Active,
	}
}

// CreateCharterClass adds an active vehicle class to a charter listing that
// has a service (ErrCharterServiceRequired otherwise).
func (s *Service) CreateCharterClass(ctx context.Context, partnerID, actor, listingID uuid.UUID, in CharterClassInput) (CharterClass, error) {
	now := s.clock.Now()
	var out CharterClass
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockCharter(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		q := store.New(tx)
		if _, err := q.GetCharterService(ctx, listingID); errors.Is(err, pgx.ErrNoRows) {
			return ErrCharterServiceRequired
		} else if err != nil {
			return fmt.Errorf("get charter service: %w", err)
		}
		n, err := in.normalize()
		if err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("charter class id: %w", err)
		}
		row, err := q.InsertCharterClass(ctx, store.InsertCharterClassParams{
			ID: id, ListingID: listingID, Name: n.Name, Seats: int32(n.Seats),
			BaseFare: n.BaseFare, PricePerKm: n.PricePerKm, Now: now,
		})
		if err != nil {
			return fmt.Errorf("insert charter class: %w", err)
		}
		out = classFromRow(row)
		changes := fieldDiff(map[string]any{}, classFields(out))
		changes["id"] = change{nil, id.String()}
		return s.logChange(ctx, tx, listingID, actor, actionCharterClass, changes, now)
	})
	if err != nil {
		return CharterClass{}, err
	}
	return out, nil
}

// UpdateCharterClass applies a patch under the locks partner (FOR SHARE) ->
// listing (FOR NO KEY UPDATE) -> class (FOR UPDATE).
func (s *Service) UpdateCharterClass(ctx context.Context, partnerID, actor, listingID, classID uuid.UUID, in CharterClassPatch) (CharterClass, error) {
	now := s.clock.Now()
	var out CharterClass
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockCharter(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		q := store.New(tx)
		cur, err := q.LockCharterClass(ctx, store.LockCharterClassParams{ID: classID, ListingID: listingID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCharterClassNotFound
		}
		if err != nil {
			return fmt.Errorf("lock charter class: %w", err)
		}
		old := classFromRow(cur)
		m := CharterClassInput{Name: old.Name, Seats: old.Seats, BaseFare: old.BaseFare, PricePerKm: old.PricePerKm}
		if in.Name != nil {
			m.Name = *in.Name
		}
		if in.Seats != nil {
			m.Seats = *in.Seats
		}
		if in.BaseFare != nil {
			m.BaseFare = *in.BaseFare
		}
		if in.PricePerKm != nil {
			m.PricePerKm = *in.PricePerKm
		}
		n, err := m.normalize()
		if err != nil {
			return err
		}
		f := CharterClass{
			ID: old.ID, ListingID: old.ListingID, Name: n.Name, Seats: n.Seats,
			BaseFare: n.BaseFare, PricePerKm: n.PricePerKm, Active: old.Active,
		}
		if in.Active != nil {
			f.Active = *in.Active
		}
		changes := fieldDiff(classFields(old), classFields(f))
		if len(changes) == 0 {
			out, err = s.withSurcharges(ctx, q, old)
			return err
		}
		row, err := q.UpdateCharterClass(ctx, store.UpdateCharterClassParams{
			ID: classID, Name: f.Name, Seats: int32(f.Seats), BaseFare: f.BaseFare, PricePerKm: f.PricePerKm,
			Active: f.Active, Now: now,
		})
		if err != nil {
			return fmt.Errorf("update charter class: %w", err)
		}
		if out, err = s.withSurcharges(ctx, q, classFromRow(row)); err != nil {
			return err
		}
		changes["id"] = change{nil, classID.String()}
		return s.logChange(ctx, tx, listingID, actor, actionCharterClass, changes, now)
	})
	if err != nil {
		return CharterClass{}, err
	}
	return out, nil
}

func (s *Service) withSurcharges(ctx context.Context, q *store.Queries, c CharterClass) (CharterClass, error) {
	rows, err := q.ListCharterSurchargesOfClass(ctx, c.ID)
	if err != nil {
		return c, fmt.Errorf("list charter surcharges: %w", err)
	}
	c.Surcharges = make([]Surcharge, 0, len(rows))
	for _, r := range rows {
		c.Surcharges = append(c.Surcharges, surchargeFromRow(r))
	}
	return c, nil
}

// validate checks one surcharge like the table CHECKs do: a known kind and
// calc, night and round_trip_waiting fixed only, fixed 0..5,000,000 VND and
// percent 0..10000 basis points.
func (sc Surcharge) validate() error {
	if !slices.Contains(surchargeKinds, sc.Kind) {
		return invalidCharter("Loại phụ phí phải là night, weekend, holiday hoặc round_trip_waiting")
	}
	switch sc.Calc {
	case CalcFixed:
		if sc.Amount < 0 || sc.Amount > maxFixedSurcharge {
			return invalidCharter(fmt.Sprintf("Phụ phí cố định phải từ 0 đến %d VND", maxFixedSurcharge))
		}
	case CalcPercent:
		if sc.Kind != SurchargeWeekend && sc.Kind != SurchargeHoliday {
			return invalidCharter("Phụ phí " + sc.Kind + " chỉ được tính cố định")
		}
		if sc.Amount < 0 || sc.Amount > maxPercentBps {
			return invalidCharter(fmt.Sprintf("Phụ phí theo phần trăm phải từ 0 đến %d điểm cơ bản", maxPercentBps))
		}
	default:
		return invalidCharter("Cách tính phụ phí phải là fixed hoặc percent")
	}
	return nil
}

func surchargeFields(rows []store.CharterSurcharge) map[string]any {
	out := map[string]any{}
	for _, r := range rows {
		out[r.Kind+".calc"] = r.Calc
		out[r.Kind+".amount"] = r.Amount
		out[r.Kind+".active"] = r.Active
	}
	return out
}

// SetCharterSurcharges replaces the surcharges of a class: each kind may
// appear once, a kind in the list is upserted by (class, kind) and a kind
// that is not listed becomes inactive (never deleted, so its amount is kept).
func (s *Service) SetCharterSurcharges(ctx context.Context, partnerID, actor, listingID, classID uuid.UUID, in []SurchargeInput) (CharterClass, error) {
	now := s.clock.Now()
	in = slices.Clone(in)
	var out CharterClass
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockCharter(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		q := store.New(tx)
		cur, err := q.LockCharterClass(ctx, store.LockCharterClassParams{ID: classID, ListingID: listingID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCharterClassNotFound
		}
		if err != nil {
			return fmt.Errorf("lock charter class: %w", err)
		}
		kinds := make([]string, 0, len(in))
		for i, sc := range in {
			in[i].Kind = strings.TrimSpace(sc.Kind)
			in[i].Calc = strings.TrimSpace(sc.Calc)
			if err := in[i].validate(); err != nil {
				return err
			}
			if slices.Contains(kinds, in[i].Kind) {
				return invalidCharter("Mỗi loại phụ phí chỉ được khai báo một lần")
			}
			kinds = append(kinds, in[i].Kind)
		}
		before, err := q.ListCharterSurchargesOfClass(ctx, classID)
		if err != nil {
			return fmt.Errorf("list charter surcharges: %w", err)
		}
		for _, sc := range in {
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("charter surcharge id: %w", err)
			}
			if err := q.UpsertCharterSurcharge(ctx, store.UpsertCharterSurchargeParams{
				ID: id, VehicleClassID: classID, Kind: sc.Kind, Calc: sc.Calc, Amount: sc.Amount, Active: sc.Active, Now: now,
			}); err != nil {
				return fmt.Errorf("upsert charter surcharge: %w", err)
			}
		}
		if err := q.DeactivateCharterSurchargesNotIn(ctx, store.DeactivateCharterSurchargesNotInParams{
			VehicleClassID: classID, Kinds: kinds, Now: now,
		}); err != nil {
			return fmt.Errorf("deactivate charter surcharges: %w", err)
		}
		after, err := q.ListCharterSurchargesOfClass(ctx, classID)
		if err != nil {
			return fmt.Errorf("list charter surcharges: %w", err)
		}
		out = classFromRow(cur)
		out.Surcharges = make([]Surcharge, 0, len(after))
		for _, r := range after {
			out.Surcharges = append(out.Surcharges, surchargeFromRow(r))
		}
		changes := fieldDiff(surchargeFields(before), surchargeFields(after))
		if len(changes) == 0 {
			return nil
		}
		changes["id"] = change{nil, classID.String()}
		return s.logChange(ctx, tx, listingID, actor, actionCharterSurcharges, changes, now)
	})
	if err != nil {
		return CharterClass{}, err
	}
	return out, nil
}
