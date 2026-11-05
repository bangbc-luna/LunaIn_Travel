package listing_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

func visionInput() listing.RentalModelInput {
	return listing.RentalModelInput{
		VehicleKind: "motorbike", Name: "Honda Vision", ModelYear: 2024, Seats: 2,
		IncludedItems: ptr("2 mũ bảo hiểm, áo mưa"), Tags: []string{"Tiết kiệm xăng"},
		PricePerDay: 150_000, Quantity: 2,
	}
}

func (e *env) rentalModel(t *testing.T, p partner.Partner, l listing.Listing, in listing.RentalModelInput) listing.RentalModel {
	t.Helper()
	m, err := e.Svc.CreateRentalModel(context.Background(), p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
	return m
}

func (e *env) setQuantity(p partner.Partner, l listing.Listing, m listing.RentalModel, q int) error {
	_, err := e.Svc.UpdateRentalModel(context.Background(), p.ID, p.OwnerUserID, l.ID, m.ID, listing.RentalModelPatch{Quantity: &q})
	return err
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// randCode is a random booking/checkout code (prefix + 8 Crockford chars).
func randCode(prefix string) string {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	out := []byte(prefix)
	for _, c := range b {
		out = append(out, crockford[int(c)%len(crockford)])
	}
	return string(out)
}

// insertHold inserts, through x, a rental booking of model m in status over
// [pickup, ret) with its booking_rental_details row. requested and
// awaiting_payment hold until holdUntil; confirmed and in_service hold with
// no deadline; any other status does not hold.
func insertHold(ctx context.Context, x execer, l listing.Listing, m listing.RentalModel, status string, pickup, ret, now time.Time, holdUntil *time.Time) (uuid.UUID, error) {
	customer, checkout, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	due := now.Add(time.Hour)
	if holdUntil != nil {
		due = *holdUntil
	}
	holds := status == "requested" || status == "awaiting_payment" || status == "confirmed" || status == "in_service"
	var until *time.Time
	if status == "requested" || status == "awaiting_payment" {
		until = &due
	}
	steps := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email, role) VALUES ($1, $2, 'customer')`, []any{customer, customer.String() + "@gmail.com"}},
		{`INSERT INTO checkouts (id, code, customer_id, total_amount, status) VALUES ($1, $2, $3, 300000, 'open')`,
			[]any{checkout, randCode("CO"), customer}},
		{`INSERT INTO bookings (
			id, code, checkout_id, customer_id, partner_id, listing_id, service_type, booking_mode, status,
			service_start_at, service_end_at, original_service_start_at,
			gross_amount, commission_rate_bps, commission_amount, cancellation_policy_snapshot,
			contact_name, contact_phone, contact_email, qr_token, response_due_at,
			accepted_at, payment_due_at, hold_expires_at, hold_release_at, confirmed_at, in_service_at, completed_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'self_drive_rental', 'request', $7::text, $8, $9, $8,
			300000, 1000, 30000, '[]', 'A', '0900000000', 'a@customer.test', $10, $11,
			CASE WHEN $7::text = 'awaiting_payment' THEN $12::timestamptz END,
			CASE WHEN $7::text = 'awaiting_payment' THEN $11::timestamptz END,
			CASE WHEN $7::text = 'awaiting_payment' THEN $11::timestamptz END,
			CASE WHEN $7::text = 'awaiting_payment' THEN $11::timestamptz END,
			CASE WHEN $7::text IN ('confirmed','in_service','completed') THEN $12::timestamptz END,
			CASE WHEN $7::text IN ('in_service','completed') THEN $12::timestamptz END,
			CASE WHEN $7::text = 'completed' THEN $12::timestamptz END)`,
			[]any{id, randCode("LT"), checkout, customer, l.PartnerID, l.ID, status, pickup, ret, uuid.NewString(), due, now}},
		{`INSERT INTO booking_rental_details (
			booking_id, listing_id, vehicle_model_id, pickup_point_id, pickup_at, return_at, days,
			model_name, pickup_point_name, pickup_address, destination_province_code, holds_vehicle, hold_until)
			SELECT $1, $2, $3, id, $4, $5, 1, $6, name, address, province_code, $7, $8
			FROM rental_pickup_points WHERE listing_id = $2 ORDER BY created_at, id LIMIT 1`,
			[]any{id, l.ID, m.ID, pickup, ret, m.Name, holds, until}},
	}
	for _, s := range steps {
		if _, err := x.Exec(ctx, s.sql, s.args...); err != nil {
			return uuid.Nil, err
		}
	}
	return id, nil
}

func (e *env) hold(t *testing.T, l listing.Listing, m listing.RentalModel, status string, pickup, ret time.Time, holdUntil *time.Time) uuid.UUID {
	t.Helper()
	id, err := insertHold(context.Background(), e.Pool, l, m, status, pickup, ret, e.Clock.Now(), holdUntil)
	require.NoError(t, err)
	return id
}

func vn(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 0, 0, 0, clock.VN) }

// rentalModelShop is rentalShop plus one pickup point.
func (e *env) rentalModelShop(t *testing.T, p partner.Partner) listing.Listing {
	t.Helper()
	l := e.rentalShop(t, p)
	e.pickupPoint(t, p, l, "Cửa hàng chính", 0)
	return l
}

func TestCreateRentalModel_Validation422(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	noStore := e.rentalListing(t, p, "Chưa có cửa hàng")
	_, err := e.Svc.CreateRentalModel(ctx, p.ID, p.OwnerUserID, noStore.ID, visionInput())
	requireErr(t, err, http.StatusUnprocessableEntity, "rental_store_required")

	l := e.rentalShop(t, p)
	bad := map[string]func(*listing.RentalModelInput){
		"kind truck":  func(in *listing.RentalModelInput) { in.VehicleKind = "truck" },
		"year 1989":   func(in *listing.RentalModelInput) { in.ModelYear = 1989 },
		"year next+2": func(in *listing.RentalModelInput) { in.ModelYear = 2028 },
		"11 tags": func(in *listing.RentalModelInput) {
			in.Tags = []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}
		},
		"empty tag":     func(in *listing.RentalModelInput) { in.Tags = []string{"  "} },
		"long tag":      func(in *listing.RentalModelInput) { in.Tags = []string{string(make([]rune, 51))} },
		"price 9.999":   func(in *listing.RentalModelInput) { in.PricePerDay = 9_999 },
		"price 100M+1":  func(in *listing.RentalModelInput) { in.PricePerDay = 100_000_001 },
		"empty name":    func(in *listing.RentalModelInput) { in.Name = " " },
		"seats 0":       func(in *listing.RentalModelInput) { in.Seats = 0 },
		"seats 51":      func(in *listing.RentalModelInput) { in.Seats = 51 },
		"quantity -1":   func(in *listing.RentalModelInput) { in.Quantity = -1 },
		"quantity 1001": func(in *listing.RentalModelInput) { in.Quantity = 1001 },
	}
	for name, mutate := range bad {
		in := visionInput()
		mutate(&in)
		_, err := e.Svc.CreateRentalModel(ctx, p.ID, p.OwnerUserID, l.ID, in)
		require.Error(t, err, name)
		requireErr(t, err, http.StatusUnprocessableEntity, "invalid_rental")
	}
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM rental_vehicle_models`))

	// Next year (VN) is fine; tags are trimmed and deduplicated; 10 tags fit.
	in := visionInput()
	in.ModelYear = 2027
	in.Name = "  Honda Vision  "
	in.Tags = []string{" Tiết kiệm ", "Tiết kiệm", "Có cốp", "3", "4", "5", "6", "7", "8", "9", "10"}
	m, err := e.Svc.CreateRentalModel(ctx, p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
	require.Equal(t, "Honda Vision", m.Name)
	require.Equal(t, []string{"Tiết kiệm", "Có cốp", "3", "4", "5", "6", "7", "8", "9", "10"}, m.Tags)
	require.True(t, m.Active)
	require.Equal(t, l.ID, m.ListingID)
	require.Equal(t, 2027, m.ModelYear)

	got, err := e.Svc.GetRental(ctx, p.ID, l.ID)
	require.NoError(t, err)
	require.Len(t, got.Models, 1)
	require.Equal(t, m.ID, got.Models[0].ID)
	audits := e.audits(t, l.ID)
	require.Equal(t, "rental_model", audits[len(audits)-1].Action)
}

func TestCreateRentalModel_CoverOfOtherUser422(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	l := e.rentalShop(t, p)
	q := e.partner(t, "Beta", rental)

	in := visionInput()
	in.CoverFileID = ptr(e.upload(t, q.OwnerUserID, "listing_image"))
	_, err := e.Svc.CreateRentalModel(ctx, p.ID, p.OwnerUserID, l.ID, in)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_file")

	in.CoverFileID = ptr(e.upload(t, p.OwnerUserID, "business_license"))
	_, err = e.Svc.CreateRentalModel(ctx, p.ID, p.OwnerUserID, l.ID, in)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_file")

	own := e.upload(t, p.OwnerUserID, "listing_image")
	in.CoverFileID = &own
	m, err := e.Svc.CreateRentalModel(ctx, p.ID, p.OwnerUserID, l.ID, in)
	require.NoError(t, err)
	require.Equal(t, own, *m.CoverFileID)

	// Changing the cover checks the new file too.
	other := e.upload(t, q.OwnerUserID, "listing_image")
	_, err = e.Svc.UpdateRentalModel(ctx, p.ID, p.OwnerUserID, l.ID, m.ID, listing.RentalModelPatch{CoverFileID: &other})
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_file")
}

// Review Focus 2: now is Thursday 05/11 09:00. Vision quantity 2.
func TestUpdateRentalModel_QuantityVsPeakHolds(t *testing.T) {
	e, p := rentalEnv(t)
	l := e.rentalModelShop(t, p)
	now := e.Clock.Now()
	responseDue := now.Add(12 * time.Hour)
	holdRelease := now.Add(2*time.Hour + 5*time.Minute)

	t.Run("overlapping requested and awaiting_payment", func(t *testing.T) {
		m := e.rentalModel(t, p, l, visionInput())
		e.hold(t, l, m, "requested", vn(11, 7, 8), vn(11, 9, 8), &responseDue)
		e.hold(t, l, m, "awaiting_payment", vn(11, 8, 8), vn(11, 10, 8), &holdRelease)
		requireErr(t, e.setQuantity(p, l, m, 1), http.StatusConflict, "quantity_below_held")
		require.Equal(t, 2, e.count(t, `SELECT quantity FROM rental_vehicle_models WHERE id = $1`, m.ID))
		require.NoError(t, e.setQuantity(p, l, m, 2), "unchanged")
		require.NoError(t, e.setQuantity(p, l, m, 5), "raise")
		require.NoError(t, e.setQuantity(p, l, m, 2), "down to the peak")
	})

	t.Run("back-to-back", func(t *testing.T) {
		m := e.rentalModel(t, p, l, visionInput())
		e.hold(t, l, m, "requested", vn(11, 7, 8), vn(11, 9, 8), &responseDue)
		e.hold(t, l, m, "awaiting_payment", vn(11, 9, 8), vn(11, 11, 8), &holdRelease)
		require.NoError(t, e.setQuantity(p, l, m, 1))
		requireErr(t, e.setQuantity(p, l, m, 0), http.StatusConflict, "quantity_below_held")
	})

	t.Run("requested past response_due_at", func(t *testing.T) {
		m := e.rentalModel(t, p, l, visionInput())
		past := now.Add(-time.Minute)
		e.hold(t, l, m, "requested", vn(11, 7, 8), vn(11, 9, 8), &past)
		e.hold(t, l, m, "awaiting_payment", vn(11, 8, 8), vn(11, 10, 8), &holdRelease)
		require.NoError(t, e.setQuantity(p, l, m, 1))
	})

	t.Run("confirmed holds forever", func(t *testing.T) {
		m := e.rentalModel(t, p, l, visionInput())
		e.hold(t, l, m, "confirmed", vn(12, 20, 8), vn(12, 22, 8), nil)
		requireErr(t, e.setQuantity(p, l, m, 0), http.StatusConflict, "quantity_below_held")
		// Deactivating is always allowed; it only stops new requests.
		_, err := e.Svc.UpdateRentalModel(context.Background(), p.ID, p.OwnerUserID, l.ID, m.ID, listing.RentalModelPatch{Active: ptr(false)})
		require.NoError(t, err)
	})
}

func TestUpdateRentalModel_CompletedAndPastHoldsIgnored(t *testing.T) {
	e, p := rentalEnv(t)
	l := e.rentalModelShop(t, p)
	m := e.rentalModel(t, p, l, visionInput())
	e.hold(t, l, m, "completed", vn(11, 6, 8), vn(11, 8, 8), nil)
	// Still in_service on paper, but the scheduled return is already past.
	e.hold(t, l, m, "in_service", vn(11, 3, 8), vn(11, 5, 9), nil)
	require.NoError(t, e.setQuantity(p, l, m, 0))
	require.Equal(t, 0, e.count(t, `SELECT quantity FROM rental_vehicle_models WHERE id = $1`, m.ID))

	// A partner edit is audited, including the price (new requests only).
	price := int64(170_000)
	got, err := e.Svc.UpdateRentalModel(context.Background(), p.ID, p.OwnerUserID, l.ID, m.ID, listing.RentalModelPatch{PricePerDay: &price})
	require.NoError(t, err)
	require.Equal(t, price, got.PricePerDay)
	audits := e.audits(t, l.ID)
	last := audits[len(audits)-1]
	require.Equal(t, "rental_model", last.Action)
	require.Contains(t, last.Changes, "price_per_day")
	require.NotContains(t, last.Changes, "quantity")
}

func TestUpdateRentalModel_KindImmutable422(t *testing.T) {
	e, p := rentalEnv(t)
	l := e.rentalShop(t, p)
	m := e.rentalModel(t, p, l, visionInput())
	tok := e.token(t, p.OwnerUserID)
	path := "/v1/partner/listings/" + l.ID.String() + "/rental-models/" + m.ID.String()

	requireCode(t, e.do(t, "PATCH", path, tok, map[string]any{"vehicle_kind": "car"}), http.StatusUnprocessableEntity, "invalid_rental")
	requireCode(t, e.do(t, "PATCH", path, tok, map[string]any{"colour": "red"}), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "PATCH", path, tok, map[string]any{"seats": 0}), http.StatusUnprocessableEntity, "invalid_rental")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM rental_vehicle_models WHERE vehicle_kind = 'motorbike'`))

	rec := e.do(t, "PATCH", path, tok, map[string]any{"name": "Vision 2025", "tags": []string{"Mới"}, "included_items": ""})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"name":"Vision 2025"`)
	require.Contains(t, rec.Body.String(), `"included_items":null`)

	rec = e.do(t, "POST", "/v1/partner/listings/"+l.ID.String()+"/rental-models", tok, map[string]any{
		"vehicle_kind": "car", "name": "Toyota Vios", "model_year": 2023, "seats": 5, "price_per_day": 800000, "quantity": 1,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	requireCode(t, e.do(t, "POST", "/v1/partner/listings/"+l.ID.String()+"/rental-models", tok, map[string]any{
		"vehicle_kind": "truck", "name": "Hino", "model_year": 2023, "seats": 3, "price_per_day": 800000, "quantity": 1,
	}), http.StatusUnprocessableEntity, "invalid_rental")
}

func TestUpdateRentalModel_OtherPartner404(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	l := e.rentalShop(t, p)
	m := e.rentalModel(t, p, l, visionInput())
	q := e.partner(t, "Beta", rental)
	qty := 1

	_, err := e.Svc.UpdateRentalModel(ctx, q.ID, q.OwnerUserID, l.ID, m.ID, listing.RentalModelPatch{Quantity: &qty})
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.CreateRentalModel(ctx, q.ID, q.OwnerUserID, l.ID, visionInput())
	requireErr(t, err, http.StatusNotFound, "not_found")

	// A model of another listing of the same partner, or an unknown id.
	other := e.dalatLocation(t, p, e.rentalListing(t, p, "Thuê xe Nha Trang"))
	_, err = e.Svc.SetRentalStore(ctx, p.ID, p.OwnerUserID, other.ID, storeIn("07:00", "20:00"))
	require.NoError(t, err)
	_, err = e.Svc.UpdateRentalModel(ctx, p.ID, p.OwnerUserID, other.ID, m.ID, listing.RentalModelPatch{Quantity: &qty})
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.UpdateRentalModel(ctx, p.ID, p.OwnerUserID, l.ID, uuid.Must(uuid.NewV7()), listing.RentalModelPatch{Quantity: &qty})
	requireErr(t, err, http.StatusNotFound, "not_found")

	tok := e.token(t, q.OwnerUserID)
	base := "/v1/partner/listings/" + l.ID.String() + "/rental-models"
	requireCode(t, e.do(t, "PATCH", base+"/"+m.ID.String(), tok, map[string]any{"quantity": 1}), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "POST", base, tok, map[string]any{
		"vehicle_kind": "car", "name": "Vios", "model_year": 2023, "seats": 5, "price_per_day": 800000, "quantity": 1,
	}), http.StatusNotFound, "not_found")
	own := e.token(t, p.OwnerUserID)
	requireCode(t, e.do(t, "PATCH", base+"/not-a-uuid", own, map[string]any{"quantity": 1}), http.StatusNotFound, "not_found")
	require.Equal(t, 2, e.count(t, `SELECT quantity FROM rental_vehicle_models WHERE id = $1`, m.ID))
}

func TestSuspendedPartnerCannotEditModels(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	l := e.rentalShop(t, p)
	m := e.rentalModel(t, p, l, visionInput())
	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, p.ID, "Vi phạm"))

	_, err := e.Svc.CreateRentalModel(ctx, p.ID, p.OwnerUserID, l.ID, visionInput())
	requireErr(t, err, http.StatusForbidden, "partner_suspended")
	_, err = e.Svc.UpdateRentalModel(ctx, p.ID, p.OwnerUserID, l.ID, m.ID, listing.RentalModelPatch{Active: ptr(false)})
	requireErr(t, err, http.StatusForbidden, "partner_suspended")
}

func TestLockRentalModels_OrderedAndDeduped(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	l := e.rentalShop(t, p)
	a := e.rentalModel(t, p, l, visionInput())
	b := e.rentalModel(t, p, l, visionInput())
	c := e.rentalModel(t, p, l, visionInput())

	tx, err := e.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	got, err := e.Svc.LockRentalModels(ctx, tx, []uuid.UUID{c.ID, a.ID, c.ID, b.ID})
	require.NoError(t, err)
	require.Len(t, got, 3)
	for i := 1; i < len(got); i++ {
		require.Negative(t, compareUUID(got[i-1].ID, got[i].ID), "ordered by id")
	}
	// The rows are locked: another tx cannot take them.
	_, err = e.Pool.Exec(ctx, `SELECT 1 FROM rental_vehicle_models WHERE id = $1 FOR UPDATE NOWAIT`, b.ID)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "%v", err)
	require.Equal(t, "55P03", pgErr.Code)

	listingID, err := e.Svc.RentalModelListingID(ctx, tx, a.ID)
	require.NoError(t, err)
	require.Equal(t, l.ID, listingID)
	require.NoError(t, tx.Rollback(ctx))

	tx2, err := e.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx2.Rollback(ctx) }()
	_, err = e.Svc.LockRentalModels(ctx, tx2, []uuid.UUID{a.ID, uuid.Must(uuid.NewV7())})
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.RentalModelListingID(ctx, tx2, uuid.Must(uuid.NewV7()))
	requireErr(t, err, http.StatusNotFound, "not_found")
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// bookLast plays the booking side of the race: under the model lock it
// counts the peak over [pickup, ret) and holds a vehicle when one is left.
func (e *env) bookLast(ctx context.Context, l listing.Listing, m listing.RentalModel, pickup, ret time.Time, locked chan<- struct{}, release <-chan struct{}) (bool, error) {
	booked := false
	err := db.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
		ms, err := e.Svc.LockRentalModels(ctx, tx, []uuid.UUID{m.ID})
		if err != nil {
			return err
		}
		if locked != nil {
			close(locked)
		}
		if release != nil {
			<-release
		}
		peak, err := booking.Guard{}.RentalPeakHeld(ctx, tx, []uuid.UUID{m.ID}, pickup, &ret, e.Clock.Now())
		if err != nil {
			return err
		}
		if peak[m.ID]+1 > ms[0].Quantity {
			return nil
		}
		booked = true
		_, err = insertHold(ctx, tx, l, m, "confirmed", pickup, ret, e.Clock.Now(), nil)
		return err
	})
	return booked, err
}

// The partner lowers the quantity while a hold is being created: the model
// lock serializes the two, so the peak never ends above the quantity.
func TestUpdateRentalModel_QuantityDecreaseRace(t *testing.T) {
	e, p := rentalEnv(t)
	ctx := context.Background()
	l := e.rentalModelShop(t, p)

	finalPeak := func(m listing.RentalModel) (peak, qty int) {
		require.NoError(t, db.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
			got, err := booking.Guard{}.RentalPeakHeld(ctx, tx, []uuid.UUID{m.ID}, e.Clock.Now(), nil, e.Clock.Now())
			peak = got[m.ID]
			return err
		}))
		return peak, e.count(t, `SELECT quantity FROM rental_vehicle_models WHERE id = $1`, m.ID)
	}

	// Deterministic: the hold takes the lock first, the decrease waits and
	// then sees the new hold.
	m := e.rentalModel(t, p, l, visionInput())
	e.hold(t, l, m, "confirmed", vn(11, 7, 8), vn(11, 9, 8), nil)
	locked, release := make(chan struct{}), make(chan struct{})
	type res struct {
		booked bool
		err    error
	}
	bookDone := make(chan res, 1)
	go func() {
		b, err := e.bookLast(ctx, l, m, vn(11, 8, 8), vn(11, 10, 8), locked, release)
		bookDone <- res{b, err}
	}()
	<-locked
	updDone := make(chan error, 1)
	go func() { updDone <- e.setQuantity(p, l, m, 1) }()
	select {
	case err := <-updDone:
		t.Fatalf("quantity update did not wait for the model lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	r := <-bookDone
	require.NoError(t, r.err)
	require.True(t, r.booked)
	requireErr(t, <-updDone, http.StatusConflict, "quantity_below_held")
	peak, qty := finalPeak(m)
	require.Equal(t, 2, peak)
	require.Equal(t, 2, qty)

	// Free-running: exactly one side wins each round.
	for i := 0; i < 8; i++ {
		m := e.rentalModel(t, p, l, visionInput())
		e.hold(t, l, m, "confirmed", vn(11, 7, 8), vn(11, 9, 8), nil)
		var wg sync.WaitGroup
		var booked bool
		var bookErr, updErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			booked, bookErr = e.bookLast(ctx, l, m, vn(11, 8, 8), vn(11, 10, 8), nil, nil)
		}()
		go func() {
			defer wg.Done()
			updErr = e.setQuantity(p, l, m, 1)
		}()
		wg.Wait()
		require.NoError(t, bookErr)
		if booked {
			requireErr(t, updErr, http.StatusConflict, "quantity_below_held")
		} else {
			require.NoError(t, updErr)
		}
		peak, qty := finalPeak(m)
		require.LessOrEqual(t, peak, qty, "round %d", i)
	}
}
