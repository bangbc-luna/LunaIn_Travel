package listing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/maps"
	"github.com/bangbc-luna/lunain-travel/internal/platform/money"
)

const (
	// charterHorizonDays bounds how far ahead a pickup may be;
	// maxCharterReturnSpan bounds a round trip's return after the pickup.
	charterHorizonDays   = 180
	maxCharterReturnSpan = 7 * 24 * time.Hour

	maxCharterAddress = 500

	// Vietnam bounding box for pickup and dropoff coordinates.
	minVNLat, maxVNLat = 8.0, 23.5
	minVNLng, maxVNLng = 102.0, 110.0
)

var (
	ErrMapUnavailable       = httpx.NewError(http.StatusServiceUnavailable, "map_unavailable", "Chưa tính được quãng đường lúc này, vui lòng thử lại sau ít phút")
	ErrNoRoute              = httpx.NewError(http.StatusUnprocessableEntity, "no_route", "Không tìm được đường đi giữa điểm đón và điểm trả")
	ErrOutsideServiceArea   = httpx.NewError(http.StatusUnprocessableEntity, "outside_service_area", "Điểm đón nằm ngoài khu vực phục vụ của nhà xe")
	ErrDistanceExceedsMax   = httpx.NewError(http.StatusUnprocessableEntity, "distance_exceeds_max", "Quãng đường vượt quá giới hạn phục vụ của nhà xe")
	ErrInvalidLocationPoint = httpx.NewError(http.StatusUnprocessableEntity, "invalid_location", "Tọa độ phải nằm trong lãnh thổ Việt Nam")
	ErrInvalidCharterTime   = httpx.NewError(http.StatusUnprocessableEntity, "invalid_charter_time", "Thời gian không hợp lệ: giờ đón và giờ về phải tròn phút, giờ đón trong vòng 180 ngày tới; chuyến khứ hồi cần giờ về sau khi xe tới nơi và trong vòng 7 ngày, chuyến một chiều không có giờ về")
	ErrCharterPickupTooSoon = httpx.NewError(http.StatusUnprocessableEntity, "pickup_too_soon", "Giờ đón quá gần, cần để đủ thời gian nhà xe phản hồi và bạn thanh toán")
)

// CharterPlace is a pickup or dropoff point picked on the map: the server
// never geocodes. ProvinceCode is used for the pickup only (the service area).
type CharterPlace struct {
	Address      string  `json:"address"`
	Lat          float64 `json:"lat"`
	Lng          float64 `json:"lng"`
	ProvinceCode string  `json:"province_code,omitempty"`
}

func (p CharterPlace) point() maps.Point { return maps.Point{Lat: p.Lat, Lng: p.Lng} }

// inVietnam reports whether the coordinates are finite and inside the
// Vietnam bounding box.
func (p CharterPlace) inVietnam() bool {
	ok := func(v, lo, hi float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= lo && v <= hi }
	return ok(p.Lat, minVNLat, maxVNLat) && ok(p.Lng, minVNLng, maxVNLng)
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// normalize trims the address (1..500 runes), checks the coordinates are in
// Vietnam and rounds them to 6 decimals (numeric(9,6)). With needProvince the
// province code is required (it is checked against the service area later);
// otherwise it is dropped. Every failure is ErrInvalidLocationPoint.
func (p CharterPlace) normalize(needProvince bool) (CharterPlace, error) {
	addr, err := normalizeText(p.Address, "Địa chỉ", true, maxCharterAddress)
	if err != nil {
		var fe *fieldError
		if errors.As(err, &fe) {
			return p, withMessage(ErrInvalidLocationPoint, fe.app.Message)
		}
		return p, err
	}
	if !p.inVietnam() {
		return p, ErrInvalidLocationPoint
	}
	out := CharterPlace{Address: addr, Lat: round6(p.Lat), Lng: round6(p.Lng)}
	if needProvince {
		out.ProvinceCode = strings.TrimSpace(p.ProvinceCode)
		if out.ProvinceCode == "" {
			return p, withMessage(ErrInvalidLocationPoint, "Cần chọn tỉnh/thành của điểm đón")
		}
	}
	return out, nil
}

// CharterRoute is the one-way route of a charter as priced: DistanceTenths is
// the distance rounded up to 0.1 km and DurationMinutes the driving time
// rounded up to the minute.
type CharterRoute struct {
	Provider        string
	DistanceMeters  int64
	DurationSeconds int64
	DistanceTenths  int64
	DurationMinutes int
}

// CharterRoute asks the map provider for the one-way route. It never opens a
// transaction and must not be called inside one. Coordinates outside Vietnam
// are ErrInvalidLocationPoint (the provider is not called); a provider
// failure or timeout is ErrMapUnavailable; no drivable route is ErrNoRoute.
func (s *Service) CharterRoute(ctx context.Context, pickup, dropoff CharterPlace) (CharterRoute, error) {
	if !pickup.inVietnam() || !dropoff.inVietnam() {
		return CharterRoute{}, ErrInvalidLocationPoint
	}
	if s.maps == nil {
		s.log.Error("charter route: no map provider configured")
		return CharterRoute{}, ErrMapUnavailable
	}
	r, err := s.maps.Route(ctx, pickup.point(), dropoff.point())
	if errors.Is(err, maps.ErrNoRoute) {
		return CharterRoute{}, ErrNoRoute
	}
	if err != nil {
		s.log.Warn("charter route: map provider failed", "provider", s.maps.Name(), "err", err)
		return CharterRoute{}, ErrMapUnavailable
	}
	if r.DistanceMeters <= 0 {
		return CharterRoute{}, ErrNoRoute
	}
	if r.DurationSeconds < 0 {
		s.log.Warn("charter route: negative duration", "provider", s.maps.Name(), "seconds", r.DurationSeconds)
		return CharterRoute{}, ErrMapUnavailable
	}
	return CharterRoute{
		Provider:        s.maps.Name(),
		DistanceMeters:  r.DistanceMeters,
		DurationSeconds: r.DurationSeconds,
		DistanceTenths:  (r.DistanceMeters + 99) / 100,
		// At least one minute: a route of some length always takes time
		// (booking_charter_details.duration_minutes > 0).
		DurationMinutes: max(1, int((r.DurationSeconds+59)/60)),
	}, nil
}

// CheckCharterTime validates the times of a charter at now and returns its
// service end. pickup and ret are whole minutes and the pickup at most 180
// days ahead. A one-way trip has no ret and ends durationMinutes after the
// pickup; a round trip needs ret in [pickup + durationMinutes, pickup + 7
// days] and ends at ret. Any of these is ErrInvalidCharterTime. The pickup
// must also be at least paymentHours plus one hour ahead, so the partner can
// answer and the customer pay (ErrCharterPickupTooSoon).
func CheckCharterTime(pickup time.Time, roundTrip bool, ret *time.Time, durationMinutes int, now time.Time, paymentHours int) (time.Time, error) {
	whole := func(t time.Time) bool { return !t.IsZero() && t.Equal(t.Truncate(time.Minute)) }
	if !whole(pickup) || pickup.After(now.AddDate(0, 0, charterHorizonDays)) {
		return time.Time{}, ErrInvalidCharterTime
	}
	var end time.Time
	if roundTrip {
		if ret == nil || !whole(*ret) ||
			ret.Before(pickup.Add(time.Duration(durationMinutes)*time.Minute)) || ret.After(pickup.Add(maxCharterReturnSpan)) {
			return time.Time{}, ErrInvalidCharterTime
		}
		end = *ret
	} else {
		if ret != nil {
			return time.Time{}, ErrInvalidCharterTime
		}
		end = pickup.Add(time.Duration(durationMinutes) * time.Minute)
	}
	if pickup.Before(now.Add(time.Duration(paymentHours)*time.Hour + minPartnerResponse)) {
		return time.Time{}, ErrCharterPickupTooSoon
	}
	return end, nil
}

// clockMinutes parses "HH:MM" into minutes after midnight.
func clockMinutes(s string) (int, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// InNightWindow reports whether t, read in Vietnam time, is inside
// [start, end) ("HH:MM"). The window crosses midnight when start is after end
// (22:00-05:00 holds 22:00 and 04:59, not 05:00). An unparsable or empty
// window (start == end) holds nothing.
func InNightWindow(start, end string, t time.Time) bool {
	a, ok1 := clockMinutes(start)
	b, ok2 := clockMinutes(end)
	if !ok1 || !ok2 || a == b {
		return false
	}
	vn := t.In(clock.VN)
	m := vn.Hour()*60 + vn.Minute()
	if a < b {
		return m >= a && m < b
	}
	return m >= a || m < b
}

// SurchargeLine is one surcharge applied to a quote: Rate is the configured
// amount (VND when Calc is fixed, basis points when percent) and Amount the
// VND charged.
type SurchargeLine struct {
	Kind   string `json:"kind"`
	Calc   string `json:"calc"`
	Rate   int64  `json:"rate"`
	Amount int64  `json:"amount"`
}

// CharterPricing is the price list of one vehicle class: its surcharges (only
// the active ones above 0 apply) and the night window of the service (nil: no
// night surcharge).
type CharterPricing struct {
	BaseFare, PricePerKm int64
	Surcharges           []Surcharge
	NightStart, NightEnd *string
}

// PricingInput is what a price depends on besides the price list.
type PricingInput struct {
	OneWayTenths int64
	RoundTrip    bool
	PickupAt     time.Time
	Holiday      bool
}

// CharterQuote is the price of a trip for one vehicle class. It is also the
// booking_charter_details.quote snapshot. In JSON, DistanceTenths and
// BilledTenths are written as distance_km and billed_km (one decimal).
type CharterQuote struct {
	VehicleClassID  uuid.UUID
	ClassName       string
	Seats           int
	Provider        string
	DistanceMeters  int64
	DurationSeconds int64
	DistanceTenths  int64
	BilledTenths    int64
	DurationMinutes int
	IsRoundTrip     bool
	BaseFare        int64
	PricePerKm      int64
	DistanceTotal   int64
	Fare            int64
	Surcharges      []SurchargeLine
	Total           int64
}

type charterQuoteJSON struct {
	VehicleClassID  uuid.UUID       `json:"vehicle_class_id"`
	ClassName       string          `json:"class_name"`
	Seats           int             `json:"seats"`
	Provider        string          `json:"provider"`
	DistanceMeters  int64           `json:"distance_meters"`
	DurationSeconds int64           `json:"duration_seconds"`
	DistanceKm      float64         `json:"distance_km"`
	BilledKm        float64         `json:"billed_km"`
	DurationMinutes int             `json:"duration_minutes"`
	IsRoundTrip     bool            `json:"is_round_trip"`
	BaseFare        int64           `json:"base_fare"`
	PricePerKm      int64           `json:"price_per_km"`
	DistanceTotal   int64           `json:"distance_total"`
	Fare            int64           `json:"fare"`
	Surcharges      []SurchargeLine `json:"surcharges"`
	Total           int64           `json:"total"`
}

func tenthsToKm(t int64) float64 { return float64(t) / 10 }

func kmToTenths(km float64) int64 { return int64(math.Round(km * 10)) }

// MarshalJSON writes the distances in km with one decimal.
func (q CharterQuote) MarshalJSON() ([]byte, error) {
	lines := q.Surcharges
	if lines == nil {
		lines = []SurchargeLine{}
	}
	return json.Marshal(charterQuoteJSON{
		VehicleClassID: q.VehicleClassID, ClassName: q.ClassName, Seats: q.Seats, Provider: q.Provider,
		DistanceMeters: q.DistanceMeters, DurationSeconds: q.DurationSeconds,
		DistanceKm: tenthsToKm(q.DistanceTenths), BilledKm: tenthsToKm(q.BilledTenths),
		DurationMinutes: q.DurationMinutes, IsRoundTrip: q.IsRoundTrip,
		BaseFare: q.BaseFare, PricePerKm: q.PricePerKm, DistanceTotal: q.DistanceTotal, Fare: q.Fare,
		Surcharges: lines, Total: q.Total,
	})
}

// UnmarshalJSON reads what MarshalJSON writes (a stored quote snapshot).
func (q *CharterQuote) UnmarshalJSON(b []byte) error {
	var v charterQuoteJSON
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&v); err != nil {
		return err
	}
	*q = CharterQuote{
		VehicleClassID: v.VehicleClassID, ClassName: v.ClassName, Seats: v.Seats, Provider: v.Provider,
		DistanceMeters: v.DistanceMeters, DurationSeconds: v.DurationSeconds,
		DistanceTenths: kmToTenths(v.DistanceKm), BilledTenths: kmToTenths(v.BilledKm),
		DurationMinutes: v.DurationMinutes, IsRoundTrip: v.IsRoundTrip,
		BaseFare: v.BaseFare, PricePerKm: v.PricePerKm, DistanceTotal: v.DistanceTotal, Fare: v.Fare,
		Surcharges: v.Surcharges, Total: v.Total,
	}
	return nil
}

// surchargeAmount is the VND a surcharge adds to fare: a fixed amount, or a
// percent (basis points) of the fare rounded half up.
func surchargeAmount(sc Surcharge, fare int64) int64 {
	if sc.Calc == CalcPercent {
		return money.MulBps(fare, int(sc.Amount))
	}
	return sc.Amount
}

// PriceCharter prices a trip (pure): billed distance = one-way distance (× 2
// for a round trip), distance total = price per km × billed km rounded half
// up, fare = base fare + distance total, then the surcharges in the order
// night, weekend or holiday, round_trip_waiting. A surcharge applies only when
// active and above 0. Night applies when the pickup (Vietnam time) is in the
// night window; weekend when the pickup's Vietnam date is a Saturday or
// Sunday; holiday when in.Holiday. On a weekend holiday only the larger of the
// two applies (holiday when equal). Percent surcharges are taken on the fare
// only, never on other surcharges.
func PriceCharter(p CharterPricing, in PricingInput) CharterQuote {
	billed := in.OneWayTenths
	if in.RoundTrip {
		billed *= 2
	}
	q := CharterQuote{
		DistanceTenths: in.OneWayTenths, BilledTenths: billed, IsRoundTrip: in.RoundTrip,
		BaseFare: p.BaseFare, PricePerKm: p.PricePerKm,
		DistanceTotal: money.MulTenths(p.PricePerKm, billed),
		Surcharges:    []SurchargeLine{},
	}
	q.Fare = p.BaseFare + q.DistanceTotal

	applied := map[string]SurchargeLine{}
	for _, sc := range p.Surcharges {
		if !sc.Active || sc.Amount <= 0 {
			continue
		}
		applied[sc.Kind] = SurchargeLine{Kind: sc.Kind, Calc: sc.Calc, Rate: sc.Amount, Amount: surchargeAmount(sc, q.Fare)}
	}
	add := func(kind string, when bool) {
		if l, ok := applied[kind]; ok && when {
			q.Surcharges = append(q.Surcharges, l)
		}
	}

	add(SurchargeNight, p.NightStart != nil && p.NightEnd != nil && InNightWindow(*p.NightStart, *p.NightEnd, in.PickupAt))
	wd := in.PickupAt.In(clock.VN).Weekday()
	weekend := wd == time.Saturday || wd == time.Sunday
	we, okWe := applied[SurchargeWeekend]
	ho, okHo := applied[SurchargeHoliday]
	okWe, okHo = okWe && weekend, okHo && in.Holiday
	switch {
	case okWe && okHo:
		if we.Amount > ho.Amount {
			add(SurchargeWeekend, true)
		} else {
			add(SurchargeHoliday, true)
		}
	case okWe:
		add(SurchargeWeekend, true)
	case okHo:
		add(SurchargeHoliday, true)
	}
	add(SurchargeRoundTripWaiting, in.RoundTrip)

	q.Total = q.Fare
	for _, l := range q.Surcharges {
		q.Total += l.Amount
	}
	return q
}

// CharterTrip is a normalized charter request.
type CharterTrip struct {
	Pickup, Dropoff CharterPlace
	PickupAt        time.Time
	RoundTrip       bool
	ReturnAt        *time.Time
}

// Normalize checks and normalizes a trip before any I/O: the pickup (its
// province is required) and the dropoff as CharterPlace.normalize does, and a
// pickup time must be given (ErrInvalidCharterTime).
func (t CharterTrip) Normalize() (CharterTrip, error) {
	pickup, err := t.Pickup.normalize(true)
	if err != nil {
		return t, err
	}
	dropoff, err := t.Dropoff.normalize(false)
	if err != nil {
		return t, err
	}
	if t.PickupAt.IsZero() {
		return t, ErrInvalidCharterTime
	}
	t.Pickup, t.Dropoff = pickup, dropoff
	return t, nil
}

// CharterPrecheck runs in tx, before the paid map call, every check of a
// normalized trip that does not need the route: the times (CheckCharterTime
// with a zero duration: whole minutes, horizon, return presence and span,
// lead time), the charter service (ErrListingNotFound), the pickup province in
// its service area (ErrOutsideServiceArea) and, with classID, that class
// active and of the listing (ErrCharterClassNotFound). It returns how many
// classes would be quoted (0: no active class, so the map need not be
// called). CharterQuoteTx repeats the checks with the route, authoritatively.
func (s *Service) CharterPrecheck(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, classID *uuid.UUID, trip CharterTrip, now time.Time, paymentHours int) (int, error) {
	if _, err := CheckCharterTime(trip.PickupAt, trip.RoundTrip, trip.ReturnAt, 0, now, paymentHours); err != nil {
		return 0, err
	}
	d, err := s.charterDetail(ctx, tx, listingID, false)
	if err != nil {
		return 0, err
	}
	if d.Service == nil {
		return 0, ErrListingNotFound
	}
	if !slices.Contains(d.Service.ProvinceCodes, trip.Pickup.ProvinceCode) {
		return 0, ErrOutsideServiceArea
	}
	if classID != nil {
		if !slices.ContainsFunc(d.Classes, func(c CharterClass) bool { return c.ID == *classID }) {
			return 0, ErrCharterClassNotFound
		}
		return 1, nil
	}
	return len(d.Classes), nil
}

// CharterQuoteTx prices trip on route inside the caller's tx: the listing's
// charter service must exist (ErrListingNotFound), the pickup province be in
// its service area (ErrOutsideServiceArea) and the one-way distance within
// its maximum (ErrDistanceExceedsMax). With classID nil every active class is
// quoted, by seats then id; otherwise only that class, which must be active
// and belong to the listing (ErrCharterClassNotFound). The caller holds the
// listing FOR SHARE (booking) or reads in a read-only tx (public quote).
func (s *Service) CharterQuoteTx(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, classID *uuid.UUID, trip CharterTrip, route CharterRoute) ([]CharterQuote, error) {
	d, err := s.charterDetail(ctx, tx, listingID, false)
	if err != nil {
		return nil, err
	}
	if d.Service == nil {
		return nil, ErrListingNotFound
	}
	if !slices.Contains(d.Service.ProvinceCodes, trip.Pickup.ProvinceCode) {
		return nil, ErrOutsideServiceArea
	}
	if limit := d.Service.MaxDistanceTenths; limit != nil && route.DistanceTenths > *limit {
		return nil, withMessage(ErrDistanceExceedsMax, fmt.Sprintf(
			"Quãng đường một chiều %s km vượt quá giới hạn %s km của nhà xe", kmText(route.DistanceTenths), kmText(*limit)))
	}
	classes := d.Classes
	if classID != nil {
		i := slices.IndexFunc(classes, func(c CharterClass) bool { return c.ID == *classID })
		if i < 0 {
			return nil, ErrCharterClassNotFound
		}
		classes = classes[i : i+1]
	} else {
		slices.SortFunc(classes, func(a, b CharterClass) int {
			if a.Seats != b.Seats {
				return a.Seats - b.Seats
			}
			return bytes.Compare(a.ID[:], b.ID[:])
		})
	}
	if s.holidays == nil {
		return nil, errors.New("charter quote: no holiday checker configured")
	}
	holiday, err := s.holidays.IsHoliday(ctx, tx, trip.PickupAt)
	if err != nil {
		return nil, err
	}
	in := PricingInput{OneWayTenths: route.DistanceTenths, RoundTrip: trip.RoundTrip, PickupAt: trip.PickupAt, Holiday: holiday}
	out := make([]CharterQuote, 0, len(classes))
	for _, c := range classes {
		q := PriceCharter(CharterPricing{
			BaseFare: c.BaseFare, PricePerKm: c.PricePerKm, Surcharges: c.Surcharges,
			NightStart: d.Service.NightStart, NightEnd: d.Service.NightEnd,
		}, in)
		q.VehicleClassID, q.ClassName, q.Seats = c.ID, c.Name, c.Seats
		q.Provider, q.DistanceMeters, q.DurationSeconds = route.Provider, route.DistanceMeters, route.DurationSeconds
		q.DurationMinutes = route.DurationMinutes
		out = append(out, q)
	}
	return out, nil
}

// kmText writes tenths of a km the Vietnamese way: 300,1.
func kmText(tenths int64) string {
	return strconv.FormatInt(tenths/10, 10) + "," + strconv.FormatInt(tenths%10, 10)
}

// CharterQuoteInput is a public quote request.
type CharterQuoteInput struct {
	ListingID   uuid.UUID    `json:"listing_id"`
	Pickup      CharterPlace `json:"pickup"`
	Dropoff     CharterPlace `json:"dropoff"`
	PickupAt    time.Time    `json:"pickup_at"`
	IsRoundTrip bool         `json:"is_round_trip"`
	ReturnAt    *time.Time   `json:"return_at"`
}

// CharterQuoteResult is the route and the price of every active class.
type CharterQuoteResult struct {
	DistanceKm      float64        `json:"distance_km"`
	DurationMinutes int            `json:"duration_minutes"`
	Quotes          []CharterQuote `json:"quotes"`
}

// visibleCharter fails with ErrListingNotFound unless the listing is publicly
// visible and a private charter.
func visibleCharter(ctx context.Context, dbtx store.DBTX, id uuid.UUID) error {
	st, err := store.New(dbtx).VisibleListingServiceType(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && st != charterServiceType) {
		return ErrListingNotFound
	}
	if err != nil {
		return fmt.Errorf("visible listing: %w", err)
	}
	return nil
}

// QuoteCharter prices a trip for every active class of a visible charter
// listing (ErrListingNotFound otherwise). The checks that do not need the
// route (CharterPrecheck) run first in a read-only transaction, and a listing
// without an active class answers no quotes without calling the map. Then
// one map call is made outside any transaction, and the times are checked
// against the route's duration and the prices read in a second read-only
// transaction. Nothing is stored.
func (s *Service) QuoteCharter(ctx context.Context, in CharterQuoteInput) (CharterQuoteResult, error) {
	trip, err := CharterTrip{
		Pickup: in.Pickup, Dropoff: in.Dropoff, PickupAt: in.PickupAt, RoundTrip: in.IsRoundTrip, ReturnAt: in.ReturnAt,
	}.Normalize()
	if err != nil {
		return CharterQuoteResult{}, err
	}
	now := s.clock.Now()
	readOnly := pgx.TxOptions{AccessMode: pgx.ReadOnly}
	paymentHours := func(tx pgx.Tx) (int, error) {
		h, err := store.New(tx).RequestPaymentHours(ctx)
		if err != nil {
			return 0, fmt.Errorf("read request_payment_hours: %w", err)
		}
		return int(h), nil
	}

	var active int
	err = pgx.BeginTxFunc(ctx, s.pool, readOnly, func(tx pgx.Tx) error {
		if err := visibleCharter(ctx, tx, in.ListingID); err != nil {
			return err
		}
		hours, err := paymentHours(tx)
		if err != nil {
			return err
		}
		active, err = s.CharterPrecheck(ctx, tx, in.ListingID, nil, trip, now, hours)
		return err
	})
	if err != nil {
		return CharterQuoteResult{}, err
	}
	if active == 0 {
		return CharterQuoteResult{Quotes: []CharterQuote{}}, nil
	}

	route, err := s.CharterRoute(ctx, trip.Pickup, trip.Dropoff)
	if err != nil {
		return CharterQuoteResult{}, err
	}
	out := CharterQuoteResult{DistanceKm: tenthsToKm(route.DistanceTenths), DurationMinutes: route.DurationMinutes}
	err = pgx.BeginTxFunc(ctx, s.pool, readOnly, func(tx pgx.Tx) error {
		if err := visibleCharter(ctx, tx, in.ListingID); err != nil {
			return err
		}
		hours, err := paymentHours(tx)
		if err != nil {
			return err
		}
		if _, err := CheckCharterTime(trip.PickupAt, trip.RoundTrip, trip.ReturnAt, route.DurationMinutes, now, hours); err != nil {
			return err
		}
		out.Quotes, err = s.CharterQuoteTx(ctx, tx, in.ListingID, nil, trip, route)
		return err
	})
	if err != nil {
		return CharterQuoteResult{}, err
	}
	return out, nil
}
