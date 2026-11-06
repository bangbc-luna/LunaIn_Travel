package listing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	maxModelName     = 200
	minModelYear     = 1990
	maxModelSeats    = 50
	maxIncludedItems = 1000
	maxModelTags     = 10
	maxModelTag      = 50
	minPricePerDay   = 10_000
	maxPricePerDay   = 100_000_000
	maxModelQuantity = 1000

	actionRentalModel = "rental_model"
)

var (
	ErrRentalModelNotFound = httpx.NewError(http.StatusNotFound, "not_found", "Không tìm thấy mẫu xe")
	ErrQuantityBelowHeld   = httpx.NewError(http.StatusConflict, "quantity_below_held", "Số lượng mới nhỏ hơn số xe đang được giữ cho các đơn")
)

// RentalModel is a vehicle model of a rental store: Quantity identical
// vehicles rented at PricePerDay.
type RentalModel struct {
	ID            uuid.UUID  `json:"id"`
	ListingID     uuid.UUID  `json:"listing_id"`
	VehicleKind   string     `json:"vehicle_kind"`
	Name          string     `json:"name"`
	ModelYear     int        `json:"model_year"`
	Seats         int        `json:"seats"`
	IncludedItems *string    `json:"included_items"`
	Tags          []string   `json:"tags"`
	PricePerDay   int64      `json:"price_per_day"`
	Quantity      int        `json:"quantity"`
	CoverFileID   *uuid.UUID `json:"cover_file_id"`
	Active        bool       `json:"active"`
}

// RentalModelInput creates a vehicle model (always active). VehicleKind is
// car or motorbike, the name 1..200 characters, the model year 1990..next
// year (Vietnam), seats 1..50, included items at most 1000 characters ("" is
// none), at most 10 tags of 1..50 characters each (trimmed, duplicates
// dropped), the price 10,000..100,000,000 VND per day and the quantity
// 0..1000. The cover must be a listing_image uploaded by the caller.
type RentalModelInput struct {
	VehicleKind   string     `json:"vehicle_kind"`
	Name          string     `json:"name"`
	ModelYear     int        `json:"model_year"`
	Seats         int        `json:"seats"`
	IncludedItems *string    `json:"included_items"`
	Tags          []string   `json:"tags"`
	PricePerDay   int64      `json:"price_per_day"`
	Quantity      int        `json:"quantity"`
	CoverFileID   *uuid.UUID `json:"cover_file_id"`
}

// RentalModelPatch updates a vehicle model; nil fields are left unchanged
// (included_items "" clears it). vehicle_kind cannot change (create a new
// model instead) and is rejected with 422 when present in the JSON body. A
// new price applies to new requests only: bookings snapshot their price.
type RentalModelPatch struct {
	Name          *string
	ModelYear     *int
	Seats         *int
	IncludedItems *string
	Tags          *[]string
	PricePerDay   *int64
	Quantity      *int
	CoverFileID   *uuid.UUID
	Active        *bool

	// immutable lists the fixed fields present in the decoded JSON body.
	immutable []string
}

// UnmarshalJSON decodes the PATCH body; unknown fields are an error (400).
func (p *RentalModelPatch) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name          *string         `json:"name"`
		ModelYear     *int            `json:"model_year"`
		Seats         *int            `json:"seats"`
		IncludedItems *string         `json:"included_items"`
		Tags          *[]string       `json:"tags"`
		PricePerDay   *int64          `json:"price_per_day"`
		Quantity      *int            `json:"quantity"`
		CoverFileID   *uuid.UUID      `json:"cover_file_id"`
		Active        *bool           `json:"active"`
		VehicleKind   json.RawMessage `json:"vehicle_kind"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	out := RentalModelPatch{
		Name: raw.Name, ModelYear: raw.ModelYear, Seats: raw.Seats, IncludedItems: raw.IncludedItems,
		Tags: raw.Tags, PricePerDay: raw.PricePerDay, Quantity: raw.Quantity, CoverFileID: raw.CoverFileID,
		Active: raw.Active,
	}
	if len(raw.VehicleKind) > 0 {
		out.immutable = append(out.immutable, "vehicle_kind")
	}
	*p = out
	return nil
}

func modelFromRow(r store.RentalVehicleModel) RentalModel {
	tags := r.Tags
	if tags == nil {
		tags = []string{}
	}
	return RentalModel{
		ID: r.ID, ListingID: r.ListingID, VehicleKind: r.VehicleKind, Name: r.Name,
		ModelYear: int(r.ModelYear), Seats: int(r.Seats), IncludedItems: r.IncludedItems, Tags: tags,
		PricePerDay: r.PricePerDay, Quantity: int(r.Quantity), CoverFileID: r.CoverFileID, Active: r.Active,
	}
}

func (m RentalModel) input() RentalModelInput {
	return RentalModelInput{
		VehicleKind: m.VehicleKind, Name: m.Name, ModelYear: m.ModelYear, Seats: m.Seats,
		IncludedItems: m.IncludedItems, Tags: slices.Clone(m.Tags), PricePerDay: m.PricePerDay,
		Quantity: m.Quantity, CoverFileID: m.CoverFileID,
	}
}

// normalize trims and validates the input; maxYear is next year in Vietnam.
func (in RentalModelInput) normalize(maxYear int) (RentalModelInput, error) {
	var err error
	in.VehicleKind = strings.TrimSpace(in.VehicleKind)
	if in.VehicleKind != "car" && in.VehicleKind != "motorbike" {
		return in, invalidRental("Loại xe phải là car hoặc motorbike")
	}
	if in.Name, err = normalizeText(in.Name, "Tên mẫu xe", true, maxModelName); err != nil {
		return in, asInvalidRental(err)
	}
	if in.ModelYear < minModelYear || in.ModelYear > maxYear {
		return in, invalidRental(fmt.Sprintf("Năm sản xuất phải từ %d đến %d", minModelYear, maxYear))
	}
	if in.Seats < 1 || in.Seats > maxModelSeats {
		return in, invalidRental(fmt.Sprintf("Số chỗ phải từ 1 đến %d", maxModelSeats))
	}
	if in.IncludedItems, err = optionalText(in.IncludedItems, "Đồ dùng kèm theo", maxIncludedItems); err != nil {
		return in, asInvalidRental(err)
	}
	tags := make([]string, 0, len(in.Tags))
	for _, t := range in.Tags {
		t = strings.TrimSpace(t)
		if t == "" || utf8.RuneCountInString(t) > maxModelTag {
			return in, invalidRental(fmt.Sprintf("Mỗi nhãn phải từ 1 đến %d ký tự", maxModelTag))
		}
		if !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	if len(tags) > maxModelTags {
		return in, invalidRental(fmt.Sprintf("Tối đa %d nhãn", maxModelTags))
	}
	in.Tags = tags
	if in.PricePerDay < minPricePerDay || in.PricePerDay > maxPricePerDay {
		return in, invalidRental(fmt.Sprintf("Giá thuê mỗi ngày phải từ %d đến %d VND", minPricePerDay, maxPricePerDay))
	}
	if in.Quantity < 0 || in.Quantity > maxModelQuantity {
		return in, invalidRental(fmt.Sprintf("Số lượng xe phải từ 0 đến %d", maxModelQuantity))
	}
	return in, nil
}

// modelFields are the audited scalar fields; tags are compared separately.
func modelFields(m RentalModel) map[string]any {
	return map[string]any{
		"vehicle_kind": m.VehicleKind, "name": m.Name, "model_year": m.ModelYear, "seats": m.Seats,
		"included_items": val(m.IncludedItems), "price_per_day": m.PricePerDay, "quantity": m.Quantity,
		"cover_file_id": val(m.CoverFileID), "active": m.Active,
	}
}

func modelChanges(old *RentalModel, cur RentalModel) map[string]change {
	var oldFields map[string]any
	var oldTags any
	if old != nil {
		oldFields = modelFields(*old)
		oldTags = old.Tags
	} else {
		oldFields = map[string]any{}
	}
	changes := fieldDiff(oldFields, modelFields(cur))
	if old == nil || !slices.Equal(old.Tags, cur.Tags) {
		changes["tags"] = change{oldTags, cur.Tags}
	}
	return changes
}

// maxModelYear is next year on the Vietnam calendar.
func maxModelYear(now time.Time) int { return now.In(clock.VN).Year() + 1 }

// CreateRentalModel adds an active vehicle model to a rental listing that has
// a store (ErrStoreRequired otherwise). See RentalModelInput for the rules.
func (s *Service) CreateRentalModel(ctx context.Context, partnerID, actor, listingID uuid.UUID, in RentalModelInput) (RentalModel, error) {
	now := s.clock.Now()
	var out RentalModel
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockRental(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		q := store.New(tx)
		if _, err := q.GetRentalStore(ctx, listingID); errors.Is(err, pgx.ErrNoRows) {
			return ErrStoreRequired
		} else if err != nil {
			return fmt.Errorf("get rental store: %w", err)
		}
		n, err := in.normalize(maxModelYear(now))
		if err != nil {
			return err
		}
		if n.CoverFileID != nil {
			if err := s.files.Owned(ctx, tx, *n.CoverFileID, actor, imagePurpose); err != nil {
				return err
			}
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("rental model id: %w", err)
		}
		row, err := q.InsertRentalModel(ctx, store.InsertRentalModelParams{
			ID: id, ListingID: listingID, VehicleKind: n.VehicleKind, Name: n.Name,
			ModelYear: int16(n.ModelYear), Seats: int32(n.Seats), IncludedItems: n.IncludedItems, Tags: n.Tags,
			PricePerDay: n.PricePerDay, Quantity: int32(n.Quantity), CoverFileID: n.CoverFileID, Now: now,
		})
		if err != nil {
			return fmt.Errorf("insert rental model: %w", err)
		}
		out = modelFromRow(row)
		changes := modelChanges(nil, out)
		changes["id"] = change{nil, id.String()}
		return s.logChange(ctx, tx, listingID, actor, actionRentalModel, changes, now)
	})
	if err != nil {
		return RentalModel{}, err
	}
	return out, nil
}

// UpdateRentalModel applies a patch under the locks partner (FOR SHARE) ->
// listing (FOR NO KEY UPDATE) -> model (FOR UPDATE). Lowering the quantity
// below the peak number of vehicles held at once from now on is
// ErrQuantityBelowHeld: every booking path counts holds under the same model
// lock, so the two never interleave. Deactivating is always allowed (it only
// stops new requests).
func (s *Service) UpdateRentalModel(ctx context.Context, partnerID, actor, listingID, modelID uuid.UUID, in RentalModelPatch) (RentalModel, error) {
	now := s.clock.Now()
	var out RentalModel
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockRental(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		q := store.New(tx)
		cur, err := q.LockRentalModel(ctx, store.LockRentalModelParams{ID: modelID, ListingID: listingID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRentalModelNotFound
		}
		if err != nil {
			return fmt.Errorf("lock rental model: %w", err)
		}
		if len(in.immutable) > 0 {
			return invalidRental("Không thể đổi loại xe của mẫu xe; hãy tạo mẫu xe mới")
		}
		old := modelFromRow(cur)
		m := old.input()
		if in.Name != nil {
			m.Name = *in.Name
		}
		if in.ModelYear != nil {
			m.ModelYear = *in.ModelYear
		}
		if in.Seats != nil {
			m.Seats = *in.Seats
		}
		if in.IncludedItems != nil {
			m.IncludedItems = in.IncludedItems
		}
		if in.Tags != nil {
			m.Tags = *in.Tags
		}
		if in.PricePerDay != nil {
			m.PricePerDay = *in.PricePerDay
		}
		if in.Quantity != nil {
			m.Quantity = *in.Quantity
		}
		if in.CoverFileID != nil {
			m.CoverFileID = in.CoverFileID
		}
		n, err := m.normalize(maxModelYear(now))
		if err != nil {
			return err
		}
		if n.CoverFileID != nil && !eq(n.CoverFileID, old.CoverFileID) {
			if err := s.files.Owned(ctx, tx, *n.CoverFileID, actor, imagePurpose); err != nil {
				return err
			}
		}
		if n.Quantity < old.Quantity {
			if err := s.guardRentalQuantity(ctx, tx, modelID, n.Quantity, now); err != nil {
				return err
			}
		}
		f := RentalModel{
			ID: old.ID, ListingID: old.ListingID, VehicleKind: old.VehicleKind, Name: n.Name,
			ModelYear: n.ModelYear, Seats: n.Seats, IncludedItems: n.IncludedItems, Tags: n.Tags,
			PricePerDay: n.PricePerDay, Quantity: n.Quantity, CoverFileID: n.CoverFileID, Active: old.Active,
		}
		if in.Active != nil {
			f.Active = *in.Active
		}
		changes := modelChanges(&old, f)
		if len(changes) == 0 {
			out = old
			return nil
		}
		row, err := q.UpdateRentalModel(ctx, store.UpdateRentalModelParams{
			ID: modelID, Name: f.Name, ModelYear: int16(f.ModelYear), Seats: int32(f.Seats),
			IncludedItems: f.IncludedItems, Tags: f.Tags, PricePerDay: f.PricePerDay, Quantity: int32(f.Quantity),
			CoverFileID: f.CoverFileID, Active: f.Active, Now: now,
		})
		if err != nil {
			return fmt.Errorf("update rental model: %w", err)
		}
		out = modelFromRow(row)
		changes["id"] = change{nil, modelID.String()}
		return s.logChange(ctx, tx, listingID, actor, actionRentalModel, changes, now)
	})
	if err != nil {
		return RentalModel{}, err
	}
	return out, nil
}

// guardRentalQuantity rejects a quantity below the peak number of vehicles of
// modelID held at once in [now, ∞). The caller holds the model FOR UPDATE.
// With a nil guard the check is skipped.
func (s *Service) guardRentalQuantity(ctx context.Context, tx pgx.Tx, modelID uuid.UUID, quantity int, now time.Time) error {
	if s.bookings == nil {
		return nil
	}
	peak, err := s.bookings.RentalPeakHeld(ctx, tx, []uuid.UUID{modelID}, now, nil, now)
	if err != nil {
		return err
	}
	if peak[modelID] > quantity {
		return ErrQuantityBelowHeld
	}
	return nil
}

// listRentalModels reads the models of a listing (oldest first); inactive
// ones only when includeInactive is set.
func listRentalModels(ctx context.Context, dbtx store.DBTX, listingID uuid.UUID, includeInactive bool) ([]RentalModel, error) {
	rows, err := store.New(dbtx).ListRentalModels(ctx, store.ListRentalModelsParams{ListingID: listingID, IncludeInactive: includeInactive})
	if err != nil {
		return nil, fmt.Errorf("list rental models: %w", err)
	}
	out := make([]RentalModel, 0, len(rows))
	for _, r := range rows {
		out = append(out, modelFromRow(r))
	}
	return out, nil
}

// RentalModelListingID reads the listing of a model inside the caller's tx
// without locking (ErrRentalModelNotFound if none). Used by booking to find
// the listing before taking the locks in order.
func (s *Service) RentalModelListingID(ctx context.Context, tx pgx.Tx, modelID uuid.UUID) (uuid.UUID, error) {
	id, err := store.New(tx).GetRentalModelListingID(ctx, modelID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrRentalModelNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("get rental model listing: %w", err)
	}
	return id, nil
}

// LockRentalModels locks the given models FOR UPDATE in id order (DB §1.1,
// after partners and listings) inside the caller's tx and returns them in
// that order. Duplicate ids are dropped; an unknown id is
// ErrRentalModelNotFound. Inactive models are returned too: the caller
// decides. Used by booking.
func (s *Service) LockRentalModels(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) ([]RentalModel, error) {
	uniq := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if !slices.Contains(uniq, id) {
			uniq = append(uniq, id)
		}
	}
	rows, err := store.New(tx).LockRentalModels(ctx, uniq)
	if err != nil {
		return nil, fmt.Errorf("lock rental models: %w", err)
	}
	if len(rows) != len(uniq) {
		return nil, ErrRentalModelNotFound
	}
	out := make([]RentalModel, len(rows))
	for i, r := range rows {
		out[i] = modelFromRow(r)
	}
	return out, nil
}
