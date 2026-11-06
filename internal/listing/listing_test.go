package listing_test

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
)

var slugRe = regexp.MustCompile(`^[a-z0-9-]+-[a-z0-9]{6}$`)

func TestCreate_DraftWithSlug(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	prov, ward, _ := e.location(t)
	l, err := e.Svc.Create(context.Background(), p.ID, p.OwnerUserID, listing.CreateInput{
		ServiceType: attraction, Title: "  Vé VinWonders Nha Trang  ",
		Summary: ptr("Công viên giải trí"), ProvinceCode: &prov, WardCode: &ward,
		Latitude: ptr(12.2196), Longitude: ptr(109.2412),
	})
	require.NoError(t, err)
	require.Equal(t, p.ID, l.PartnerID)
	require.Equal(t, "Vé VinWonders Nha Trang", l.Title)
	require.Equal(t, "draft", l.ReviewStatus)
	require.Equal(t, "published", l.Visibility)
	require.Regexp(t, slugRe, l.Slug)
	require.True(t, strings.HasPrefix(l.Slug, "ve-vinwonders-nha-trang-"), l.Slug)
	require.Equal(t, prov, *l.ProvinceCode)
	require.Equal(t, ward, *l.WardCode)
	require.InDelta(t, 12.2196, *l.Latitude, 1e-9)
	require.Nil(t, l.Description)
	require.True(t, l.CreatedAt.Equal(testNow))

	got, err := e.Svc.Get(context.Background(), p.ID, l.ID)
	require.NoError(t, err)
	require.Equal(t, l.Slug, got.Slug)

	a := e.audits(t, l.ID)
	require.Len(t, a, 1)
	require.Equal(t, "create", a[0].Action)
	require.Equal(t, p.OwnerUserID, a[0].Actor)
	require.Equal(t, "Vé VinWonders Nha Trang", a[0].Changes["title"].New)

	// A title with no slug-able characters falls back to "listing".
	l2 := e.create(t, p, "!!!")
	require.Regexp(t, `^listing-[a-z0-9]{6}$`, l2.Slug)
}

func TestCreate_RejectsSuspendedPartner(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	require.NoError(t, e.Partner.Suspend(context.Background(), e.Admin, p.ID, "Vi phạm"))
	_, err := e.Svc.Create(context.Background(), p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: attraction, Title: "Vé A"})
	st, code := appCode(t, err)
	require.Equal(t, http.StatusForbidden, st)
	require.Equal(t, "partner_suspended", code)
	require.Zero(t, e.count(t, `SELECT count(*) FROM listings`))
	require.Zero(t, e.count(t, `SELECT count(*) FROM listing_audit_log`))
}

func TestCreate_RejectsDisabledServiceType(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Never enabled.
	p := e.partner(t, "Alpha", "scheduled_ride")
	_, err := e.Svc.Create(ctx, p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: attraction, Title: "Vé A"})
	st, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, st)
	require.Equal(t, "service_type_not_enabled", code)

	// Enabled, then disabled.
	q := e.partner(t, "Beta", attraction)
	require.NoError(t, e.Partner.DisableServiceType(ctx, e.Admin, q.ID, attraction))
	_, err = e.Svc.Create(ctx, q.ID, q.OwnerUserID, listing.CreateInput{ServiceType: attraction, Title: "Vé B"})
	_, code = appCode(t, err)
	require.Equal(t, "service_type_not_enabled", code)
	require.Zero(t, e.count(t, `SELECT count(*) FROM listings`))
}

func TestCreate_RejectsUnknownServiceType(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	_, err := e.Svc.Create(context.Background(), p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: "spaceship", Title: "Vé A"})
	st, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, st)
	require.Equal(t, "invalid_service_type", code)
}

// Ruling 5: only attraction_ticket can be listed for now.
func TestCreate_UnsupportedServiceType422(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction, "private_charter")
	_, err := e.Svc.Create(context.Background(), p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: "private_charter", Title: "Xe"})
	requireErr(t, err, http.StatusUnprocessableEntity, "service_type_not_supported")
	require.Contains(t, err.Error(), "Loại dịch vụ này chưa hỗ trợ đăng bán")
	require.Zero(t, e.count(t, `SELECT count(*) FROM listings`))
	require.Zero(t, e.count(t, `SELECT count(*) FROM listing_audit_log`))
}

// Ruling 5: a listing of an unsupported type (only reachable by SQL) is never
// complete, has no admin/public detail and is never bookable.
func TestUnsupportedServiceType_FailsClosed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction, "private_charter")
	id := e.rawListing(t, p, "private_charter", "Xe")
	_, _, err := e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, id, []uuid.UUID{e.upload(t, p.OwnerUserID, "listing_image")}, nil)
	require.NoError(t, err)

	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, id)
	requireErr(t, err, http.StatusUnprocessableEntity, "service_type_not_supported")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM listings WHERE id = $1 AND review_status = 'draft'`, id))

	// Even if it were approved and published by hand it stays closed.
	_, err = e.Pool.Exec(ctx, `UPDATE listings SET review_status = 'approved' WHERE id = $1`, id)
	require.NoError(t, err)
	_, err = e.bookable(t, id)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")
	_, err = e.Svc.AdminGet(ctx, id)
	requireErr(t, err, http.StatusUnprocessableEntity, "service_type_not_supported")
	_, err = e.Svc.PublicDetail(ctx, "raw-"+id.String())
	requireErr(t, err, http.StatusUnprocessableEntity, "service_type_not_supported")
}

func TestCreate_LocationMustMatch(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	_, ward, other := e.location(t)
	for name, in := range map[string]listing.CreateInput{
		"ward of another province": {ProvinceCode: &other, WardCode: &ward},
		"province without ward":    {ProvinceCode: &other},
		"ward without province":    {WardCode: &ward},
		"unknown ward":             {ProvinceCode: &other, WardCode: ptr("99999")},
	} {
		in.ServiceType, in.Title = attraction, "Vé A"
		_, err := e.Svc.Create(context.Background(), p.ID, p.OwnerUserID, in)
		st, code := appCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, st, name)
		require.Equal(t, "invalid_location", code, name)
	}
	require.Zero(t, e.count(t, `SELECT count(*) FROM listings`))
}

func TestCreate_Validation(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	cases := map[string]listing.CreateInput{
		"empty title":       {Title: "   "},
		"201-rune title":    {Title: strings.Repeat("ắ", 201)},
		"501-rune summary":  {Title: "Vé", Summary: ptr(strings.Repeat("a", 501))},
		"long description":  {Title: "Vé", Description: ptr(strings.Repeat("a", 20001))},
		"301-rune address":  {Title: "Vé", Address: ptr(strings.Repeat("a", 301))},
		"latitude only":     {Title: "Vé", Latitude: ptr(10.0)},
		"latitude too big":  {Title: "Vé", Latitude: ptr(90.5), Longitude: ptr(100.0)},
		"longitude too big": {Title: "Vé", Latitude: ptr(10.0), Longitude: ptr(-180.5)},
	}
	for name, in := range cases {
		in.ServiceType = attraction
		_, err := e.Svc.Create(context.Background(), p.ID, p.OwnerUserID, in)
		st, code := appCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, st, name)
		require.Equal(t, "invalid_listing", code, name)
		require.ErrorIs(t, err, listing.ErrInvalidListing, name)
	}
	_, err := e.Svc.Create(context.Background(), p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: attraction, Title: ""})
	require.ErrorContains(t, err, "title")

	// 200 runes is the limit.
	l := e.create(t, p, strings.Repeat("ắ", 200))
	require.Equal(t, 200, len([]rune(l.Title)))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM listings`))
}

func TestUpdate_LogsOnlyChangedFields(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l, err := e.Svc.Create(ctx, p.ID, p.OwnerUserID, listing.CreateInput{
		ServiceType: attraction, Title: "Vé cũ", Summary: ptr("Tóm tắt"),
	})
	require.NoError(t, err)

	e.Clock.T = testNow.Add(1)
	u, err := e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{
		Title: ptr("Vé mới"), Summary: ptr("  Tóm tắt "),
	})
	require.NoError(t, err)
	require.Equal(t, "Vé mới", u.Title)
	require.Equal(t, "Tóm tắt", *u.Summary)

	a := e.audits(t, l.ID)
	require.Len(t, a, 2)
	require.Equal(t, "update", a[1].Action)
	require.Equal(t, p.OwnerUserID, a[1].Actor)
	require.Len(t, a[1].Changes, 1, "%v", a[1].Changes)
	require.Equal(t, "Vé cũ", a[1].Changes["title"].Old)
	require.Equal(t, "Vé mới", a[1].Changes["title"].New)

	// Clearing an optional field logs old -> null.
	_, err = e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{Summary: ptr("")})
	require.NoError(t, err)
	a = e.audits(t, l.ID)
	require.Len(t, a, 3)
	require.Equal(t, "Tóm tắt", a[2].Changes["summary"].Old)
	require.Nil(t, a[2].Changes["summary"].New)

	// An update that changes nothing writes no audit row.
	same, err := e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{Title: ptr("Vé mới"), Summary: ptr("")})
	require.NoError(t, err)
	require.Equal(t, "Vé mới", same.Title)
	_, err = e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{})
	require.NoError(t, err)
	require.Len(t, e.audits(t, l.ID), 3)

	// Validation still applies and the title cannot be cleared.
	_, err = e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{Title: ptr("")})
	require.ErrorIs(t, err, listing.ErrInvalidListing)

	// Location and coordinates: set once (logged), then resent unchanged
	// (numeric(9,6) round trip compares equal, so nothing is logged).
	prov, ward, other := e.location(t)
	_, err = e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{ProvinceCode: &other, WardCode: &ward})
	_, code := appCode(t, err)
	require.Equal(t, "invalid_location", code)
	_, err = e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{ProvinceCode: &prov})
	_, code = appCode(t, err)
	require.Equal(t, "invalid_location", code)
	loc := listing.UpdateInput{ProvinceCode: &prov, WardCode: &ward, Latitude: ptr(10.7769412), Longitude: ptr(106.7009)}
	u, err = e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, loc)
	require.NoError(t, err)
	require.Equal(t, 10.776941, *u.Latitude)
	a = e.audits(t, l.ID)
	require.Len(t, a, 4)
	require.ElementsMatch(t, []string{"province_code", "ward_code", "latitude", "longitude"}, keys(a[3].Changes))
	_, err = e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, loc)
	require.NoError(t, err)
	require.Len(t, e.audits(t, l.ID), 4)

	// "" on both clears the location.
	u, err = e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{ProvinceCode: ptr(""), WardCode: ptr("")})
	require.NoError(t, err)
	require.Nil(t, u.ProvinceCode)
	require.Nil(t, u.WardCode)
	require.Len(t, e.audits(t, l.ID), 5)
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestUpdate_SlugStable(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé cũ")
	u, err := e.Svc.Update(context.Background(), p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{Title: ptr("Tên hoàn toàn khác")})
	require.NoError(t, err)
	require.Equal(t, l.Slug, u.Slug)
	require.True(t, strings.HasPrefix(u.Slug, "ve-cu-"))
}

func TestUpdate_OtherPartner404(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.partner(t, "Alpha", attraction)
	b := e.partner(t, "Beta", attraction)
	lb := e.create(t, b, "Vé của B")

	_, err := e.Svc.Update(ctx, a.ID, a.OwnerUserID, lb.ID, listing.UpdateInput{Title: ptr("Bị chiếm")})
	require.ErrorIs(t, err, listing.ErrListingNotFound)
	st, code := appCode(t, err)
	require.Equal(t, http.StatusNotFound, st)
	require.Equal(t, "not_found", code)

	_, err = e.Svc.Get(ctx, a.ID, lb.ID)
	require.ErrorIs(t, err, listing.ErrListingNotFound)
	_, err = e.Svc.Get(ctx, a.ID, uuid.New())
	require.ErrorIs(t, err, listing.ErrListingNotFound)

	got, err := e.Svc.Get(ctx, b.ID, lb.ID)
	require.NoError(t, err)
	require.Equal(t, "Vé của B", got.Title)
	require.Len(t, e.audits(t, lb.ID), 1)
}

func TestUpdate_SuspendedPartner403(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, p.ID, "Vi phạm"))

	_, err := e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{Title: ptr("Vé B")})
	st, code := appCode(t, err)
	require.Equal(t, http.StatusForbidden, st)
	require.Equal(t, "partner_suspended", code)

	// Still readable while suspended.
	got, err := e.Svc.Get(ctx, p.ID, l.ID)
	require.NoError(t, err)
	require.Equal(t, "Vé A", got.Title)
	require.Len(t, e.audits(t, l.ID), 1)
}

func TestListOwn_OnlyOwnAndPaginates(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.partner(t, "Alpha", attraction)
	b := e.partner(t, "Beta", attraction)
	var mine []uuid.UUID
	for _, title := range []string{"Vé 1", "Vé 2", "Vé 3"} {
		mine = append(mine, e.create(t, a, title).ID)
	}
	e.create(t, b, "Vé của B")

	page1, next, err := e.Svc.ListOwn(ctx, a.ID, listing.OwnFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.NotNil(t, next)
	page2, next2, err := e.Svc.ListOwn(ctx, a.ID, listing.OwnFilter{Limit: 2, Cursor: next})
	require.NoError(t, err)
	require.Len(t, page2, 1)
	require.Nil(t, next2)

	all := append(page1, page2...)
	var got []uuid.UUID
	for i, l := range all {
		require.Equal(t, a.ID, l.PartnerID)
		got = append(got, l.ID)
		if i > 0 {
			require.Equal(t, 1, bytes.Compare(all[i-1].ID[:], l.ID[:]), "ids must be descending")
		}
	}
	require.ElementsMatch(t, mine, got)

	drafts, _, err := e.Svc.ListOwn(ctx, a.ID, listing.OwnFilter{ReviewStatus: ptr("draft")})
	require.NoError(t, err)
	require.Len(t, drafts, 3)
	approved, _, err := e.Svc.ListOwn(ctx, a.ID, listing.OwnFilter{ReviewStatus: ptr("approved")})
	require.NoError(t, err)
	require.Empty(t, approved)
	_, _, err = e.Svc.ListOwn(ctx, a.ID, listing.OwnFilter{ReviewStatus: ptr("bogus")})
	st, _ := appCode(t, err)
	require.Equal(t, http.StatusBadRequest, st)
}
