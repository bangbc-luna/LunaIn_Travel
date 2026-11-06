package listing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

const (
	charterServiceType = "private_charter"

	maxCharterProvinces = 34
	minCharterKm        = 1.0
	maxCharterKm        = 2000.0

	actionCharterService = "charter_service"
)

func invalidCharter(msg string) error { return withMessage(ErrInvalidCharter, msg) }

// asInvalidCharter relabels a validation error of the shared helpers
// (normalizeText, minutes) as ErrInvalidCharter, keeping its message.
func asInvalidCharter(err error) error {
	var fe *fieldError
	if errors.As(err, &fe) {
		return invalidCharter(fe.app.Message)
	}
	return err
}

// CharterServiceInput is the charter service of a listing. ProvinceCodes are
// the provinces where pickup is served (1..34 real ones, duplicates dropped).
// MaxDistanceKm limits the one-way distance (1.0..2000.0 with at most one
// decimal; nil = no limit). NightStart and NightEnd are "HH:MM" in Vietnam
// time, both or neither, different; the window crosses midnight when
// NightStart is after NightEnd.
type CharterServiceInput struct {
	ProvinceCodes []string `json:"province_codes"`
	MaxDistanceKm *float64 `json:"max_distance_km"`
	NightStart    *string  `json:"night_start"`
	NightEnd      *string  `json:"night_end"`
}

// CharterService is a stored charter service. The service area applies to the
// pickup province only; MaxDistanceTenths (tenths of a km) is checked against
// the one-way distance when quoting.
type CharterService struct {
	ListingID         uuid.UUID
	ProvinceCodes     []string
	MaxDistanceTenths *int64
	NightStart        *string
	NightEnd          *string
}

// MarshalJSON writes max_distance_km in km.
func (c CharterService) MarshalJSON() ([]byte, error) {
	var km *float64
	if c.MaxDistanceTenths != nil {
		v := float64(*c.MaxDistanceTenths) / 10
		km = &v
	}
	codes := c.ProvinceCodes
	if codes == nil {
		codes = []string{}
	}
	return json.Marshal(struct {
		ListingID     uuid.UUID `json:"listing_id"`
		ProvinceCodes []string  `json:"province_codes"`
		MaxDistanceKm *float64  `json:"max_distance_km"`
		NightStart    *string   `json:"night_start"`
		NightEnd      *string   `json:"night_end"`
	}{c.ListingID, codes, km, c.NightStart, c.NightEnd})
}

// CharterDetail is the service (nil until set) and the vehicle classes of a
// listing.
type CharterDetail struct {
	Service *CharterService `json:"service"`
	Classes []CharterClass  `json:"classes"`
}

// charterSettings is a validated CharterServiceInput.
type charterSettings struct {
	codes      []string // sorted, unique
	tenths     *int64
	start, end *string
}

func (s charterSettings) fields() map[string]any {
	var km any
	if s.tenths != nil {
		km = float64(*s.tenths) / 10
	}
	return map[string]any{
		"province_codes": strings.Join(s.codes, ","), "max_distance_km": km,
		"night_start": val(s.start), "night_end": val(s.end),
	}
}

func settingsOf(c CharterService) charterSettings {
	return charterSettings{codes: c.ProvinceCodes, tenths: c.MaxDistanceTenths, start: c.NightStart, end: c.NightEnd}
}

// normalize validates the input shape; the provinces are checked against the
// table by the caller.
func (in CharterServiceInput) normalize() (charterSettings, error) {
	var out charterSettings
	for _, c := range in.ProvinceCodes {
		c = strings.TrimSpace(c)
		if c == "" {
			return out, invalidCharter("Mã tỉnh/thành không được để trống")
		}
		if !slices.Contains(out.codes, c) {
			out.codes = append(out.codes, c)
		}
	}
	if len(out.codes) == 0 || len(out.codes) > maxCharterProvinces {
		return out, invalidCharter(fmt.Sprintf("Khu vực phục vụ phải có từ 1 đến %d tỉnh/thành", maxCharterProvinces))
	}
	slices.Sort(out.codes)
	if in.MaxDistanceKm != nil {
		km := *in.MaxDistanceKm
		tenths := math.Round(km * 10)
		if math.IsNaN(km) || math.IsInf(km, 0) || km < minCharterKm || km > maxCharterKm || math.Abs(km*10-tenths) > 1e-6 {
			return out, invalidCharter("Quãng đường tối đa phải từ 1,0 đến 2000,0 km và tối đa 1 chữ số thập phân")
		}
		t := int64(tenths)
		out.tenths = &t
	}
	if (in.NightStart == nil) != (in.NightEnd == nil) {
		return out, invalidCharter("Khung giờ đêm cần có cả giờ bắt đầu và giờ kết thúc")
	}
	if in.NightStart != nil {
		start, end := strings.TrimSpace(*in.NightStart), strings.TrimSpace(*in.NightEnd)
		a, err := minutes(start, "Giờ bắt đầu ban đêm")
		if err != nil {
			return out, asInvalidCharter(err)
		}
		b, err := minutes(end, "Giờ kết thúc ban đêm")
		if err != nil {
			return out, asInvalidCharter(err)
		}
		if a == b {
			return out, invalidCharter("Giờ bắt đầu và giờ kết thúc ban đêm phải khác nhau")
		}
		out.start, out.end = &start, &end
	}
	return out, nil
}

// lockCharter runs the checks every charter write starts with, in the lock
// order of DB §1.1: partner (FOR SHARE) -> listing (FOR NO KEY UPDATE). The
// listing must be a private_charter.
func (s *Service) lockCharter(ctx context.Context, tx pgx.Tx, partnerID, id uuid.UUID) error {
	if err := s.requireActivePartner(ctx, tx, partnerID); err != nil {
		return err
	}
	l, err := s.lockOwned(ctx, tx, partnerID, id)
	if err != nil {
		return err
	}
	if l.ServiceType != charterServiceType {
		return ErrNotCharterListing
	}
	return nil
}

func serviceFromRow(r store.GetCharterServiceRow, codes []string) CharterService {
	out := CharterService{ListingID: r.ListingID, ProvinceCodes: codes}
	if r.MaxDistanceTenths > 0 {
		t := r.MaxDistanceTenths
		out.MaxDistanceTenths = &t
	}
	if r.NightStart != "" && r.NightEnd != "" {
		a, b := r.NightStart, r.NightEnd
		out.NightStart, out.NightEnd = &a, &b
	}
	return out
}

// readCharterService reads the service and its areas; (nil, nil) if unset.
func readCharterService(ctx context.Context, dbtx store.DBTX, listingID uuid.UUID) (*CharterService, error) {
	q := store.New(dbtx)
	row, err := q.GetCharterService(ctx, listingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get charter service: %w", err)
	}
	codes, err := q.ListCharterServiceAreas(ctx, listingID)
	if err != nil {
		return nil, fmt.Errorf("list charter areas: %w", err)
	}
	if codes == nil {
		codes = []string{}
	}
	out := serviceFromRow(row, codes)
	return &out, nil
}

// SetCharterService creates or replaces the charter service of a charter
// listing: the areas are replaced as a whole and the change is audited. New
// settings apply to new quotes only.
func (s *Service) SetCharterService(ctx context.Context, partnerID, actor, listingID uuid.UUID, in CharterServiceInput) (CharterService, error) {
	now := s.clock.Now()
	var out CharterService
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockCharter(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		n, err := in.normalize()
		if err != nil {
			return err
		}
		q := store.New(tx)
		found, err := q.CountProvincesIn(ctx, n.codes)
		if err != nil {
			return fmt.Errorf("count provinces: %w", err)
		}
		if int(found) != len(n.codes) {
			return invalidCharter("Có tỉnh/thành không tồn tại trong khu vực phục vụ")
		}
		old, err := readCharterService(ctx, tx, listingID)
		if err != nil {
			return err
		}
		var oldFields map[string]any
		if old != nil {
			oldFields = settingsOf(*old).fields()
		}
		changes := fieldDiff(oldFields, n.fields())
		if old != nil && len(changes) == 0 {
			out = *old
			return nil
		}
		if err := q.UpsertCharterService(ctx, store.UpsertCharterServiceParams{
			ListingID: listingID, MaxDistanceTenths: n.tenths, NightStart: n.start, NightEnd: n.end, Now: now,
		}); err != nil {
			return fmt.Errorf("upsert charter service: %w", err)
		}
		if err := q.DeleteCharterAreasNotIn(ctx, store.DeleteCharterAreasNotInParams{ListingID: listingID, Codes: n.codes}); err != nil {
			return fmt.Errorf("delete charter areas: %w", err)
		}
		if err := q.InsertCharterAreas(ctx, store.InsertCharterAreasParams{ListingID: listingID, Codes: n.codes}); err != nil {
			return fmt.Errorf("insert charter areas: %w", err)
		}
		cur, err := readCharterService(ctx, tx, listingID)
		if err != nil {
			return err
		}
		out = *cur
		return s.logChange(ctx, tx, listingID, actor, actionCharterService, changes, now)
	})
	if err != nil {
		return CharterService{}, err
	}
	return out, nil
}

// GetCharter returns the service and every vehicle class (inactive classes
// and surcharges included) of the partner's charter listing.
func (s *Service) GetCharter(ctx context.Context, partnerID, listingID uuid.UUID) (CharterDetail, error) {
	l, err := s.Get(ctx, partnerID, listingID)
	if err != nil {
		return CharterDetail{}, err
	}
	if l.ServiceType != charterServiceType {
		return CharterDetail{}, ErrNotCharterListing
	}
	return s.charterDetail(ctx, s.pool, listingID, true)
}

// charterDetail reads the service and the vehicle classes (oldest first) of a
// listing. Without includeInactive: active classes only, each with the active
// surcharges that are more than 0.
func (s *Service) charterDetail(ctx context.Context, dbtx store.DBTX, listingID uuid.UUID, includeInactive bool) (CharterDetail, error) {
	out := CharterDetail{Classes: []CharterClass{}}
	var err error
	if out.Service, err = readCharterService(ctx, dbtx, listingID); err != nil {
		return out, err
	}
	q := store.New(dbtx)
	classes, err := q.ListCharterVehicleClasses(ctx, store.ListCharterVehicleClassesParams{ListingID: listingID, IncludeInactive: includeInactive})
	if err != nil {
		return out, fmt.Errorf("list charter classes: %w", err)
	}
	surcharges, err := q.ListCharterSurcharges(ctx, store.ListCharterSurchargesParams{ListingID: listingID, IncludeInactive: includeInactive})
	if err != nil {
		return out, fmt.Errorf("list charter surcharges: %w", err)
	}
	for _, c := range classes {
		cc := classFromRow(c)
		for _, sr := range surcharges {
			if sr.VehicleClassID == c.ID {
				cc.Surcharges = append(cc.Surcharges, surchargeFromRow(sr))
			}
		}
		out.Classes = append(out.Classes, cc)
	}
	return out, nil
}
