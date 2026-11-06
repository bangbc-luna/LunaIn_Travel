package listing_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
)

func TestCreateCharterClass_AndAudit(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l := e.charterShop(t, p)
	c, err := e.Svc.CreateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, listing.CharterClassInput{Name: "  Xe 7 chỗ ", Seats: 7, BaseFare: 200_000, PricePerKm: 12_000})
	require.NoError(t, err)
	require.Equal(t, "Xe 7 chỗ", c.Name)
	require.True(t, c.Active)
	require.Equal(t, l.ID, c.ListingID)
	require.Empty(t, c.Surcharges)

	n := "Xe 7 chỗ VIP"
	up, err := e.Svc.UpdateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, listing.CharterClassPatch{Name: &n, PricePerKm: ptr(int64(13_000))})
	require.NoError(t, err)
	require.Equal(t, n, up.Name)
	require.Equal(t, int64(13_000), up.PricePerKm)
	require.Equal(t, int64(200_000), up.BaseFare)

	var audits []auditRow
	for _, a := range e.audits(t, l.ID) {
		if a.Action == "charter_class" {
			audits = append(audits, a)
		}
	}
	require.Len(t, audits, 2)
	require.Contains(t, audits[1].Changes, "price_per_km")
	require.NotContains(t, audits[1].Changes, "base_fare")

	// No change, no audit row.
	_, err = e.Svc.UpdateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, listing.CharterClassPatch{Seats: ptr(7)})
	require.NoError(t, err)
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM listing_audit_log WHERE listing_id = $1 AND action = 'charter_class'`, l.ID))
}

func TestCreateCharterClass_NeedsService422(t *testing.T) {
	e, p := charterEnv(t)
	l := e.charterListing(t, p, "Xe bao chuyến Đà Nẵng")
	_, err := e.Svc.CreateCharterClass(context.Background(), p.ID, p.OwnerUserID, l.ID, class7())
	requireErr(t, err, http.StatusUnprocessableEntity, "charter_service_required")
	require.Contains(t, err.Error(), "khu vực phục vụ")
	require.Zero(t, e.count(t, `SELECT count(*) FROM charter_vehicle_classes`))
}

func TestCreateCharterClass_Validation422(t *testing.T) {
	e, p := charterEnv(t)
	l := e.charterShop(t, p)
	long := make([]rune, 101)
	for i := range long {
		long[i] = 'a'
	}
	for name, in := range map[string]listing.CharterClassInput{
		"blank name":     {Name: "  ", Seats: 4, PricePerKm: 1000},
		"long name":      {Name: string(long), Seats: 4, PricePerKm: 1000},
		"0 seats":        {Name: "A", Seats: 0, PricePerKm: 1000},
		"51 seats":       {Name: "A", Seats: 51, PricePerKm: 1000},
		"km price high":  {Name: "A", Seats: 4, PricePerKm: 1_000_001},
		"base fare high": {Name: "A", Seats: 4, BaseFare: 50_000_001, PricePerKm: 1000},
		"base fare < 0":  {Name: "A", Seats: 4, BaseFare: -1, PricePerKm: 1000},
		"km price < 0":   {Name: "A", Seats: 4, PricePerKm: -5},
	} {
		_, err := e.Svc.CreateCharterClass(context.Background(), p.ID, p.OwnerUserID, l.ID, in)
		requireErr(t, err, http.StatusUnprocessableEntity, "invalid_charter")
		require.NotEmpty(t, name)
	}
	require.Zero(t, e.count(t, `SELECT count(*) FROM charter_vehicle_classes`))

	// The upper bounds themselves are fine.
	_, err := e.Svc.CreateCharterClass(context.Background(), p.ID, p.OwnerUserID, l.ID,
		listing.CharterClassInput{Name: "Max", Seats: 50, BaseFare: 50_000_000, PricePerKm: 1_000_000})
	require.NoError(t, err)
}

// Ruling R1: a class never prices a trip at 0 VND.
func TestCharterClass_PricePerKmMustBePositive422(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l := e.charterShop(t, p)

	_, err := e.Svc.CreateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, listing.CharterClassInput{Name: "Xe 4 chỗ", Seats: 4, BaseFare: 500_000, PricePerKm: 0})
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_charter")
	require.Contains(t, err.Error(), "Giá mỗi km phải lớn hơn 0")
	require.Zero(t, e.count(t, `SELECT count(*) FROM charter_vehicle_classes`))

	c := e.charterClass(t, p, l, class7())
	_, err = e.Svc.UpdateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, listing.CharterClassPatch{PricePerKm: ptr(int64(0))})
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_charter")
	require.Equal(t, 12_000, e.count(t, `SELECT price_per_km FROM charter_vehicle_classes WHERE id = $1`, c.ID))

	// Over HTTP: 0 is 422, base_fare 0 with a positive km price is 201.
	tok := e.token(t, p.OwnerUserID)
	base := "/v1/partner/listings/" + l.ID.String() + "/charter-classes"
	requireCode(t, e.do(t, "POST", base, tok, map[string]any{"name": "Xe 4 chỗ", "seats": 4, "base_fare": 500000, "price_per_km": 0}), http.StatusUnprocessableEntity, "invalid_charter")
	requireCode(t, e.do(t, "PATCH", base+"/"+c.ID.String(), tok, map[string]any{"price_per_km": 0}), http.StatusUnprocessableEntity, "invalid_charter")
	rec := e.do(t, "POST", base, tok, map[string]any{"name": "Xe 4 chỗ", "seats": 4, "base_fare": 0, "price_per_km": 12000})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM charter_vehicle_classes`))
}

func TestSetCharterSurcharges_Rules(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l := e.charterShop(t, p)
	c := e.charterClass(t, p, l, class7())

	bad := map[string][]listing.SurchargeInput{
		"night percent":   {{Kind: "night", Calc: "percent", Amount: 1000, Active: true}},
		"waiting percent": {{Kind: "round_trip_waiting", Calc: "percent", Amount: 1000, Active: true}},
		"weekend 10001":   {{Kind: "weekend", Calc: "percent", Amount: 10001, Active: true}},
		"fixed above 5m":  {{Kind: "holiday", Calc: "fixed", Amount: 5_000_001, Active: true}},
		"negative":        {{Kind: "holiday", Calc: "fixed", Amount: -1, Active: true}},
		"unknown kind":    {{Kind: "rain", Calc: "fixed", Amount: 1, Active: true}},
		"unknown calc":    {{Kind: "night", Calc: "bps", Amount: 1, Active: true}},
		"duplicate kind":  {{Kind: "night", Calc: "fixed", Amount: 1, Active: true}, {Kind: "night", Calc: "fixed", Amount: 2, Active: true}},
	}
	for name, in := range bad {
		_, err := e.Svc.SetCharterSurcharges(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, in)
		requireErr(t, err, http.StatusUnprocessableEntity, "invalid_charter")
		require.NotEmpty(t, name)
	}
	require.Zero(t, e.count(t, `SELECT count(*) FROM charter_surcharges`))

	all := []listing.SurchargeInput{
		{Kind: listing.SurchargeNight, Calc: listing.CalcFixed, Amount: 100_000, Active: true},
		{Kind: listing.SurchargeWeekend, Calc: listing.CalcPercent, Amount: 1000, Active: true},
		{Kind: listing.SurchargeHoliday, Calc: listing.CalcPercent, Amount: 3000, Active: true},
		{Kind: listing.SurchargeRoundTripWaiting, Calc: listing.CalcFixed, Amount: 150_000, Active: true},
	}
	got, err := e.Svc.SetCharterSurcharges(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, all)
	require.NoError(t, err)
	require.Len(t, got.Surcharges, 4)
	require.Equal(t, "night", got.Surcharges[0].Kind)
	require.Equal(t, int64(150_000), got.Surcharges[3].Amount)

	// Only weekend again (new amount): the other three become inactive, not deleted.
	got, err = e.Svc.SetCharterSurcharges(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, []listing.SurchargeInput{
		{Kind: listing.SurchargeWeekend, Calc: listing.CalcFixed, Amount: 50_000, Active: true},
	})
	require.NoError(t, err)
	require.Len(t, got.Surcharges, 4)
	for _, s := range got.Surcharges {
		if s.Kind == listing.SurchargeWeekend {
			require.True(t, s.Active)
			require.Equal(t, listing.CalcFixed, s.Calc)
			require.Equal(t, int64(50_000), s.Amount)
		} else {
			require.False(t, s.Active, s.Kind)
		}
	}
	require.Equal(t, 4, e.count(t, `SELECT count(*) FROM charter_surcharges WHERE vehicle_class_id = $1`, c.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM charter_surcharges WHERE vehicle_class_id = $1 AND active`, c.ID))
	// The amount of an inactive kind is kept for later.
	require.Equal(t, 100_000, e.count(t, `SELECT amount FROM charter_surcharges WHERE vehicle_class_id = $1 AND kind = 'night'`, c.ID))

	// Audit: one row per effective change; the same body again adds none.
	n := e.count(t, `SELECT count(*) FROM listing_audit_log WHERE listing_id = $1 AND action = 'charter_surcharges'`, l.ID)
	require.Equal(t, 2, n)
	_, err = e.Svc.SetCharterSurcharges(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, []listing.SurchargeInput{
		{Kind: listing.SurchargeWeekend, Calc: listing.CalcFixed, Amount: 50_000, Active: true},
	})
	require.NoError(t, err)
	require.Equal(t, n, e.count(t, `SELECT count(*) FROM listing_audit_log WHERE listing_id = $1 AND action = 'charter_surcharges'`, l.ID))

	// An empty list switches everything off.
	got, err = e.Svc.SetCharterSurcharges(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, []listing.SurchargeInput{})
	require.NoError(t, err)
	for _, s := range got.Surcharges {
		require.False(t, s.Active)
	}
}

func TestUpdateCharterClass_OtherPartner404(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l := e.charterShop(t, p)
	c := e.charterClass(t, p, l, class7())
	q := e.partner(t, "Beta", charter)

	_, err := e.Svc.UpdateCharterClass(ctx, q.ID, q.OwnerUserID, l.ID, c.ID, listing.CharterClassPatch{Seats: ptr(5)})
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.CreateCharterClass(ctx, q.ID, q.OwnerUserID, l.ID, class7())
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.SetCharterSurcharges(ctx, q.ID, q.OwnerUserID, l.ID, c.ID, nil)
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.SetCharterService(ctx, q.ID, q.OwnerUserID, l.ID, serviceIn())
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.GetCharter(ctx, q.ID, l.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")

	// An unknown class id of the owner.
	_, err = e.Svc.UpdateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, uuid.Must(uuid.NewV7()), listing.CharterClassPatch{Seats: ptr(5)})
	requireErr(t, err, http.StatusNotFound, "not_found")
	require.Equal(t, 7, e.count(t, `SELECT seats FROM charter_vehicle_classes WHERE id = $1`, c.ID))
}

func TestSuspendedPartnerCannotEditCharter(t *testing.T) {
	e, p := charterEnv(t)
	ctx := context.Background()
	l := e.charterShop(t, p)
	c := e.charterClass(t, p, l, class7())
	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, p.ID, "Vi phạm"))

	_, err := e.Svc.CreateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, class16())
	requireErr(t, err, http.StatusForbidden, "partner_suspended")
	_, err = e.Svc.UpdateCharterClass(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, listing.CharterClassPatch{Active: ptr(false)})
	requireErr(t, err, http.StatusForbidden, "partner_suspended")
	_, err = e.Svc.SetCharterSurcharges(ctx, p.ID, p.OwnerUserID, l.ID, c.ID, nil)
	requireErr(t, err, http.StatusForbidden, "partner_suspended")
	_, err = e.Svc.SetCharterService(ctx, p.ID, p.OwnerUserID, l.ID, serviceIn())
	requireErr(t, err, http.StatusForbidden, "partner_suspended")
}
