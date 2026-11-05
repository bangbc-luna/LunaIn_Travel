package listing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	rentalServiceType = "self_drive_rental"

	maxRentalTerm         = 2000
	maxPickupPointName    = 200
	maxPickupPointAddress = 500
	maxDeliveryFee        = 5_000_000

	// maxRentalDays and rentalHorizonDays bound a rental period; the partner
	// needs minPartnerResponse to answer a request after the payment window.
	maxRentalDays      = 30
	rentalHorizonDays  = 180
	minPartnerResponse = time.Hour
	rentalDayLength    = 24 * time.Hour

	actionRentalStore       = "rental_store"
	actionRentalPickupPoint = "rental_pickup_point"
)

var (
	ErrNotRentalListing    = httpx.NewError(http.StatusUnprocessableEntity, "wrong_service_type", "Thao tác này chỉ áp dụng cho Listing thuê xe tự lái")
	ErrInvalidRental       = httpx.NewError(http.StatusUnprocessableEntity, "invalid_rental", "Thông tin cửa hàng thuê xe không hợp lệ")
	ErrStoreRequired       = httpx.NewError(http.StatusUnprocessableEntity, "rental_store_required", "Cần khai báo thông tin cửa hàng trước")
	ErrPickupPointNotFound = httpx.NewError(http.StatusNotFound, "not_found", "Không tìm thấy điểm nhận xe")

	ErrInvalidRentalPeriod  = httpx.NewError(http.StatusUnprocessableEntity, "invalid_rental_period", "Thời gian thuê không hợp lệ: giờ nhận và giờ trả phải tròn phút, thuê từ 1 đến 30 ngày và nhận xe trong vòng 180 ngày tới")
	ErrPickupTooSoon        = httpx.NewError(http.StatusUnprocessableEntity, "pickup_too_soon", "Giờ nhận xe quá gần, cần để đủ thời gian đối tác phản hồi và bạn thanh toán")
	ErrOutsideHandoverHours = httpx.NewError(http.StatusUnprocessableEntity, "outside_handover_hours", "Giờ nhận hoặc giờ trả xe nằm ngoài giờ giao nhận của cửa hàng")
)

func invalidRental(msg string) error { return withMessage(ErrInvalidRental, msg) }

// asInvalidRental relabels a validation error of the shared helpers
// (normalizeText, minutes) as ErrInvalidRental, keeping its message.
func asInvalidRental(err error) error {
	var fe *fieldError
	if errors.As(err, &fe) {
		return invalidRental(fe.app.Message)
	}
	return err
}

// RentalDays is the number of rental days of [pickup, ret): 24-hour blocks
// rounded up, at least 1.
func RentalDays(pickup, ret time.Time) int {
	d := ret.Sub(pickup) // saturates for periods beyond ~292 years
	days := int(d / rentalDayLength)
	if d%rentalDayLength != 0 {
		days++
	}
	return max(days, 1)
}

// CheckRentalPeriod validates a rental period at now and returns its number
// of days. Both times must be whole minutes, ret after pickup, at most 30
// days and the pickup at most 180 days ahead (ErrInvalidRentalPeriod). The
// pickup must also be at least paymentHours plus one hour ahead, so the
// partner can answer and the customer pay (ErrPickupTooSoon).
func CheckRentalPeriod(pickup, ret, now time.Time, paymentHours int) (int, error) {
	if !pickup.Equal(pickup.Truncate(time.Minute)) || !ret.Equal(ret.Truncate(time.Minute)) || !ret.After(pickup) {
		return 0, ErrInvalidRentalPeriod
	}
	// Bounds are time comparisons: a Duration would saturate for far dates.
	if pickup.After(now.AddDate(0, 0, rentalHorizonDays)) || ret.After(pickup.Add(maxRentalDays*rentalDayLength)) {
		return 0, ErrInvalidRentalPeriod
	}
	days := RentalDays(pickup, ret)
	if pickup.Before(now.Add(time.Duration(paymentHours)*time.Hour + minPartnerResponse)) {
		return 0, ErrPickupTooSoon
	}
	return days, nil
}

// RentalStoreInput is the rental terms and the handover window of a store.
// HandoverOpen and HandoverClose are "HH:MM" in Vietnam time. Each term is at
// most 2000 characters; an empty term is stored as NULL.
type RentalStoreInput struct {
	HandoverOpen      string  `json:"handover_open"`
	HandoverClose     string  `json:"handover_close"`
	DeliveryAvailable bool    `json:"delivery_available"`
	DepositTerms      *string `json:"deposit_terms"`
	DocumentsTerms    *string `json:"documents_terms"`
	LateFeeTerms      *string `json:"late_fee_terms"`
	AllowedAreaTerms  *string `json:"allowed_area_terms"`
	OtherTerms        *string `json:"other_terms"`
}

// RentalStore is a stored store. Its location is the listing's own.
type RentalStore struct {
	ListingID uuid.UUID `json:"listing_id"`
	RentalStoreInput
}

// CoversHandover reports whether the Vietnam wall-clock minute of t lies in
// [HandoverOpen, HandoverClose], both ends included.
func (st RentalStore) CoversHandover(t time.Time) bool {
	open, err1 := minutes(st.HandoverOpen, "")
	closeAt, err2 := minutes(st.HandoverClose, "")
	if err1 != nil || err2 != nil {
		return false
	}
	vn := t.In(clock.VN)
	m := vn.Hour()*60 + vn.Minute()
	return m >= open && m <= closeAt
}

// PickupPoint is a place where customers take over and return the vehicle.
type PickupPoint struct {
	ID           uuid.UUID `json:"id"`
	ListingID    uuid.UUID `json:"listing_id"`
	Name         string    `json:"name"`
	Address      string    `json:"address"`
	ProvinceCode string    `json:"province_code"`
	WardCode     string    `json:"ward_code"`
	DeliveryFee  int64     `json:"delivery_fee"`
	Active       bool      `json:"active"`
}

// PickupPointInput creates a pickup point (always active).
type PickupPointInput struct {
	Name         string `json:"name"`
	Address      string `json:"address"`
	ProvinceCode string `json:"province_code"`
	WardCode     string `json:"ward_code"`
	DeliveryFee  int64  `json:"delivery_fee"`
}

// PickupPointPatch updates a pickup point; nil fields are left unchanged.
// province_code and ward_code cannot change (create a new point instead) and
// are rejected with 422 when present in the JSON body.
type PickupPointPatch struct {
	Name        *string
	Address     *string
	DeliveryFee *int64
	Active      *bool

	// immutable lists the fixed fields present in the decoded JSON body.
	immutable []string
}

// UnmarshalJSON decodes the PATCH body; unknown fields are an error (400).
func (p *PickupPointPatch) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name         *string         `json:"name"`
		Address      *string         `json:"address"`
		DeliveryFee  *int64          `json:"delivery_fee"`
		Active       *bool           `json:"active"`
		ProvinceCode json.RawMessage `json:"province_code"`
		WardCode     json.RawMessage `json:"ward_code"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	out := PickupPointPatch{Name: raw.Name, Address: raw.Address, DeliveryFee: raw.DeliveryFee, Active: raw.Active}
	if len(raw.ProvinceCode) > 0 {
		out.immutable = append(out.immutable, "province_code")
	}
	if len(raw.WardCode) > 0 {
		out.immutable = append(out.immutable, "ward_code")
	}
	*p = out
	return nil
}

// RentalDetail is the store (nil until set), the vehicle models and the
// pickup points of a listing.
type RentalDetail struct {
	Store        *RentalStore  `json:"store"`
	Models       []RentalModel `json:"models"`
	PickupPoints []PickupPoint `json:"pickup_points"`
}

func storeFromRow(r store.RentalStore) RentalStore {
	return RentalStore{ListingID: r.ListingID, RentalStoreInput: RentalStoreInput{
		HandoverOpen: clockText(r.HandoverOpen), HandoverClose: clockText(r.HandoverClose),
		DeliveryAvailable: r.DeliveryAvailable,
		DepositTerms:      r.DepositTerms, DocumentsTerms: r.DocumentsTerms, LateFeeTerms: r.LateFeeTerms,
		AllowedAreaTerms: r.AllowedAreaTerms, OtherTerms: r.OtherTerms,
	}}
}

func pointFromRow(r store.RentalPickupPoint) PickupPoint {
	return PickupPoint{
		ID: r.ID, ListingID: r.ListingID, Name: r.Name, Address: r.Address,
		ProvinceCode: r.ProvinceCode, WardCode: r.WardCode, DeliveryFee: r.DeliveryFee, Active: r.Active,
	}
}

// lockRental runs the checks every rental write starts with, in the lock
// order of DB §1.1: partner (FOR SHARE) -> listing (FOR NO KEY UPDATE). The
// listing must be a self_drive_rental.
func (s *Service) lockRental(ctx context.Context, tx pgx.Tx, partnerID, id uuid.UUID) error {
	if err := s.requireActivePartner(ctx, tx, partnerID); err != nil {
		return err
	}
	l, err := s.lockOwned(ctx, tx, partnerID, id)
	if err != nil {
		return err
	}
	if l.ServiceType != rentalServiceType {
		return ErrNotRentalListing
	}
	return nil
}

// normalize trims and validates the input: close after open, terms 0..2000
// characters ("" becomes nil).
func (in RentalStoreInput) normalize() (RentalStoreInput, error) {
	in.HandoverOpen = strings.TrimSpace(in.HandoverOpen)
	in.HandoverClose = strings.TrimSpace(in.HandoverClose)
	open, err := minutes(in.HandoverOpen, "Giờ mở giao nhận")
	if err != nil {
		return in, asInvalidRental(err)
	}
	closeAt, err := minutes(in.HandoverClose, "Giờ đóng giao nhận")
	if err != nil {
		return in, asInvalidRental(err)
	}
	if closeAt <= open {
		return in, invalidRental("Giờ đóng giao nhận phải sau giờ mở")
	}
	for _, t := range []struct {
		p     **string
		label string
	}{
		{&in.DepositTerms, "Điều khoản đặt cọc"},
		{&in.DocumentsTerms, "Điều khoản giấy tờ"},
		{&in.LateFeeTerms, "Điều khoản phí trả trễ"},
		{&in.AllowedAreaTerms, "Điều khoản khu vực được đi"},
		{&in.OtherTerms, "Điều khoản khác"},
	} {
		v, err := optionalText(*t.p, t.label, maxRentalTerm)
		if err != nil {
			return in, asInvalidRental(err)
		}
		*t.p = v
	}
	return in, nil
}

func storeFields(in *RentalStoreInput) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	return map[string]any{
		"handover_open": in.HandoverOpen, "handover_close": in.HandoverClose,
		"delivery_available": in.DeliveryAvailable,
		"deposit_terms":      val(in.DepositTerms), "documents_terms": val(in.DocumentsTerms),
		"late_fee_terms": val(in.LateFeeTerms), "allowed_area_terms": val(in.AllowedAreaTerms),
		"other_terms": val(in.OtherTerms),
	}
}

// SetRentalStore creates or replaces the store of a rental listing and audits
// the change. Existing bookings are unaffected by a new handover window.
func (s *Service) SetRentalStore(ctx context.Context, partnerID, actor, listingID uuid.UUID, in RentalStoreInput) (RentalStore, error) {
	now := s.clock.Now()
	var out RentalStore
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockRental(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		n, err := in.normalize()
		if err != nil {
			return err
		}
		open, _ := minutes(n.HandoverOpen, "")
		closeAt, _ := minutes(n.HandoverClose, "")
		q := store.New(tx)
		var old *RentalStoreInput
		oldRow, err := q.GetRentalStore(ctx, listingID)
		switch {
		case err == nil:
			o := storeFromRow(oldRow).RentalStoreInput
			old = &o
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("get rental store: %w", err)
		}
		changes := fieldDiff(storeFields(old), storeFields(&n))
		if old != nil && len(changes) == 0 {
			out = RentalStore{ListingID: listingID, RentalStoreInput: n}
			return nil
		}
		row, err := q.UpsertRentalStore(ctx, store.UpsertRentalStoreParams{
			ListingID: listingID, HandoverOpen: pgTime(open), HandoverClose: pgTime(closeAt),
			DeliveryAvailable: n.DeliveryAvailable,
			DepositTerms:      n.DepositTerms, DocumentsTerms: n.DocumentsTerms, LateFeeTerms: n.LateFeeTerms,
			AllowedAreaTerms: n.AllowedAreaTerms, OtherTerms: n.OtherTerms, Now: now,
		})
		if err != nil {
			return fmt.Errorf("upsert rental store: %w", err)
		}
		out = storeFromRow(row)
		return s.logChange(ctx, tx, listingID, actor, actionRentalStore, changes, now)
	})
	if err != nil {
		return RentalStore{}, err
	}
	return out, nil
}

// GetRental returns the store, every vehicle model and every pickup point
// (inactive included) of the partner's rental listing.
func (s *Service) GetRental(ctx context.Context, partnerID, listingID uuid.UUID) (RentalDetail, error) {
	l, err := s.Get(ctx, partnerID, listingID)
	if err != nil {
		return RentalDetail{}, err
	}
	if l.ServiceType != rentalServiceType {
		return RentalDetail{}, ErrNotRentalListing
	}
	return s.rentalDetail(ctx, s.pool, listingID, true)
}

// RentalDetail reads the store, the vehicle models and the pickup points
// (oldest first) of a listing; inactive models and points only when
// includeInactive is set.
func (s *Service) RentalDetail(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, includeInactive bool) (RentalDetail, error) {
	return s.rentalDetail(ctx, tx, listingID, includeInactive)
}

func (s *Service) rentalDetail(ctx context.Context, dbtx store.DBTX, listingID uuid.UUID, includeInactive bool) (RentalDetail, error) {
	q := store.New(dbtx)
	out := RentalDetail{PickupPoints: []PickupPoint{}}
	r, err := q.GetRentalStore(ctx, listingID)
	switch {
	case err == nil:
		st := storeFromRow(r)
		out.Store = &st
	case !errors.Is(err, pgx.ErrNoRows):
		return out, fmt.Errorf("get rental store: %w", err)
	}
	if out.Models, err = listRentalModels(ctx, dbtx, listingID, includeInactive); err != nil {
		return out, err
	}
	rows, err := q.ListRentalPickupPoints(ctx, store.ListRentalPickupPointsParams{ListingID: listingID, IncludeInactive: includeInactive})
	if err != nil {
		return out, fmt.Errorf("list rental pickup points: %w", err)
	}
	for _, row := range rows {
		out.PickupPoints = append(out.PickupPoints, pointFromRow(row))
	}
	return out, nil
}

func (in PickupPointInput) normalize() (PickupPointInput, error) {
	var err error
	if in.Name, err = normalizeText(in.Name, "Tên điểm nhận xe", true, maxPickupPointName); err != nil {
		return in, asInvalidRental(err)
	}
	if in.Address, err = normalizeText(in.Address, "Địa chỉ điểm nhận xe", true, maxPickupPointAddress); err != nil {
		return in, asInvalidRental(err)
	}
	in.ProvinceCode = strings.TrimSpace(in.ProvinceCode)
	in.WardCode = strings.TrimSpace(in.WardCode)
	if in.ProvinceCode == "" || in.WardCode == "" {
		return in, invalidRental("Cần chọn tỉnh/thành và phường/xã của điểm nhận xe")
	}
	if err := checkDeliveryFee(in.DeliveryFee); err != nil {
		return in, err
	}
	return in, nil
}

func checkDeliveryFee(fee int64) error {
	if fee < 0 || fee > maxDeliveryFee {
		return invalidRental(fmt.Sprintf("Phí giao xe phải từ 0 đến %d VND", maxDeliveryFee))
	}
	return nil
}

// CreatePickupPoint adds an active pickup point to a listing that has a store
// (ErrStoreRequired otherwise). The name is 1..200 characters, the address
// 1..500, the ward must belong to the province and the delivery fee is
// 0..5,000,000 VND.
func (s *Service) CreatePickupPoint(ctx context.Context, partnerID, actor, listingID uuid.UUID, in PickupPointInput) (PickupPoint, error) {
	now := s.clock.Now()
	var out PickupPoint
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
		n, err := in.normalize()
		if err != nil {
			return err
		}
		if err := s.geo.ValidateLocation(ctx, tx, n.ProvinceCode, n.WardCode); err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("pickup point id: %w", err)
		}
		row, err := q.InsertRentalPickupPoint(ctx, store.InsertRentalPickupPointParams{
			ID: id, ListingID: listingID, Name: n.Name, Address: n.Address,
			ProvinceCode: n.ProvinceCode, WardCode: n.WardCode, DeliveryFee: n.DeliveryFee, Now: now,
		})
		if err != nil {
			return fmt.Errorf("insert rental pickup point: %w", err)
		}
		out = pointFromRow(row)
		changes := fieldDiff(map[string]any{}, pointFields(out))
		changes["id"] = change{nil, id.String()}
		return s.logChange(ctx, tx, listingID, actor, actionRentalPickupPoint, changes, now)
	})
	if err != nil {
		return PickupPoint{}, err
	}
	return out, nil
}

func pointFields(p PickupPoint) map[string]any {
	return map[string]any{
		"name": p.Name, "address": p.Address, "province_code": p.ProvinceCode, "ward_code": p.WardCode,
		"delivery_fee": p.DeliveryFee, "active": p.Active,
	}
}

// UpdatePickupPoint applies a patch. The location cannot change (create a new
// point instead). Deactivating a point or changing its fee leaves existing
// bookings alone: a booking snapshots the name, address and fee.
func (s *Service) UpdatePickupPoint(ctx context.Context, partnerID, actor, listingID, pointID uuid.UUID, in PickupPointPatch) (PickupPoint, error) {
	now := s.clock.Now()
	var out PickupPoint
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockRental(ctx, tx, partnerID, listingID); err != nil {
			return err
		}
		q := store.New(tx)
		cur, err := q.LockRentalPickupPoint(ctx, store.LockRentalPickupPointParams{ID: pointID, ListingID: listingID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPickupPointNotFound
		}
		if err != nil {
			return fmt.Errorf("lock rental pickup point: %w", err)
		}
		if len(in.immutable) > 0 {
			return invalidRental("Không thể đổi " + strings.Join(in.immutable, ", ") + " của điểm nhận xe; hãy tạo điểm mới")
		}
		old := pointFromRow(cur)
		f := old
		if in.Name != nil {
			if f.Name, err = normalizeText(*in.Name, "Tên điểm nhận xe", true, maxPickupPointName); err != nil {
				return asInvalidRental(err)
			}
		}
		if in.Address != nil {
			if f.Address, err = normalizeText(*in.Address, "Địa chỉ điểm nhận xe", true, maxPickupPointAddress); err != nil {
				return asInvalidRental(err)
			}
		}
		if in.DeliveryFee != nil {
			if err := checkDeliveryFee(*in.DeliveryFee); err != nil {
				return err
			}
			f.DeliveryFee = *in.DeliveryFee
		}
		if in.Active != nil {
			f.Active = *in.Active
		}
		changes := fieldDiff(pointFields(old), pointFields(f))
		if len(changes) == 0 {
			out = old
			return nil
		}
		row, err := q.UpdateRentalPickupPoint(ctx, store.UpdateRentalPickupPointParams{
			ID: pointID, Name: f.Name, Address: f.Address, DeliveryFee: f.DeliveryFee, Active: f.Active, Now: now,
		})
		if err != nil {
			return fmt.Errorf("update rental pickup point: %w", err)
		}
		out = pointFromRow(row)
		changes["id"] = change{nil, pointID.String()}
		return s.logChange(ctx, tx, listingID, actor, actionRentalPickupPoint, changes, now)
	})
	if err != nil {
		return PickupPoint{}, err
	}
	return out, nil
}

// RentalStoreOf returns the store of a listing inside the caller's tx
// (ErrStoreRequired when none is set). Used by booking.
func (s *Service) RentalStoreOf(ctx context.Context, tx pgx.Tx, listingID uuid.UUID) (RentalStore, error) {
	r, err := store.New(tx).GetRentalStore(ctx, listingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RentalStore{}, ErrStoreRequired
	}
	if err != nil {
		return RentalStore{}, fmt.Errorf("get rental store: %w", err)
	}
	return storeFromRow(r), nil
}

// PickupPointForBooking returns an active pickup point of the listing inside
// the caller's tx; an inactive point or one of another listing is
// ErrPickupPointNotFound. Used by booking.
func (s *Service) PickupPointForBooking(ctx context.Context, tx pgx.Tx, listingID, pointID uuid.UUID) (PickupPoint, error) {
	r, err := store.New(tx).GetRentalPickupPoint(ctx, store.GetRentalPickupPointParams{ID: pointID, ListingID: listingID})
	if errors.Is(err, pgx.ErrNoRows) {
		return PickupPoint{}, ErrPickupPointNotFound
	}
	if err != nil {
		return PickupPoint{}, fmt.Errorf("get rental pickup point: %w", err)
	}
	if !r.Active {
		return PickupPoint{}, ErrPickupPointNotFound
	}
	return pointFromRow(r), nil
}
