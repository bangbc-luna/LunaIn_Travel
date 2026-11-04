package listing_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
)

// approved returns a complete listing that the admin approved (visible).
func (e *env) approved(t *testing.T, p partner.Partner, title string) listing.Listing {
	t.Helper()
	l := e.submitted(t, p, title)
	out, err := e.Svc.Approve(context.Background(), e.Admin, l.ID)
	require.NoError(t, err)
	return out
}

func (e *env) search(t *testing.T, f listing.SearchFilter) []listing.PublicCard {
	t.Helper()
	cards, err := e.Svc.Search(context.Background(), f)
	require.NoError(t, err)
	return cards
}

func slugs(cards []listing.PublicCard) []string {
	out := make([]string, len(cards))
	for i, c := range cards {
		out[i] = c.Slug
	}
	return out
}

// bookable runs BookableListing in its own committed tx.
func (e *env) bookable(t *testing.T, id uuid.UUID) (listing.Listing, error) {
	t.Helper()
	var out listing.Listing
	var callErr error
	inTx(t, e, func(tx pgx.Tx) error {
		out, callErr = e.Svc.BookableListing(context.Background(), tx, id)
		return nil
	})
	return out, callErr
}

func TestSearch_OnlyVisible(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	e.create(t, p, "Vé nháp")
	e.submitted(t, p, "Vé chờ duyệt")
	ok := e.approved(t, p, "Vé đã duyệt")
	hidden := e.approved(t, p, "Vé bị ẩn")
	_, err := e.Svc.Hide(ctx, e.Admin, hidden.ID, "Vi phạm")
	require.NoError(t, err)
	unpub := e.approved(t, p, "Vé tự gỡ")
	_, err = e.Svc.Unpublish(ctx, p.ID, p.OwnerUserID, unpub.ID)
	require.NoError(t, err)

	cards := e.search(t, listing.SearchFilter{})
	require.Equal(t, []string{ok.Slug}, slugs(cards))
	c := cards[0]
	require.Equal(t, "Vé đã duyệt", c.Title)
	require.Equal(t, attraction, c.ServiceType)
	require.NotNil(t, c.CoverURL)
	require.True(t, strings.HasPrefix(*c.CoverURL, "http://files.test/public/listing_image/"), *c.CoverURL)
	require.Nil(t, c.RatingAvg)
	require.Equal(t, 0, c.RatingCount)
}

// Review Focus 2: a suspended partner's listings and listings of a disabled
// service type disappear from public search; re-enabling brings them back.
func TestSearch_HidesSuspendedPartnerAndDisabledType(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.partner(t, "Alpha", attraction)
	b := e.partner(t, "Beta", attraction)
	la := e.approved(t, a, "Vé A")
	lb := e.approved(t, b, "Vé B")
	require.ElementsMatch(t, []string{la.Slug, lb.Slug}, slugs(e.search(t, listing.SearchFilter{})))

	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, a.ID, "Vi phạm"))
	require.Equal(t, []string{lb.Slug}, slugs(e.search(t, listing.SearchFilter{})))

	require.NoError(t, e.Partner.DisableServiceType(ctx, e.Admin, b.ID, attraction))
	require.Empty(t, e.search(t, listing.SearchFilter{}))

	require.NoError(t, e.Partner.Unsuspend(ctx, e.Admin, a.ID))
	require.NoError(t, e.Partner.EnableServiceType(ctx, e.Admin, b.ID, attraction))
	require.ElementsMatch(t, []string{la.Slug, lb.Slug}, slugs(e.search(t, listing.SearchFilter{})))
}

// Review Focus 4: unaccented, case-insensitive search.
func TestSearch_UnaccentedQuery(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	vt := e.approved(t, p, "Khu du lịch Vũng Tàu Paradise")
	e.approved(t, p, "Bà Nà Hills")
	for _, q := range []string{"vung tau", "VŨNG TÀU", "  Vung Tau  ", "paradis"} {
		require.Equal(t, []string{vt.Slug}, slugs(e.search(t, listing.SearchFilter{Q: ptr(q)})), q)
	}
	// Trigram similarity catches a typo that ILIKE does not.
	require.Equal(t, []string{vt.Slug}, slugs(e.search(t, listing.SearchFilter{Q: ptr("khu du lich vung tao paradise")})))
	require.Empty(t, e.search(t, listing.SearchFilter{Q: ptr("phú quốc")}))
	// A blank q is no filter.
	require.Len(t, e.search(t, listing.SearchFilter{Q: ptr("   ")}), 2)
}

// Review Focus 4: %, _ and \ in q are literal characters.
func TestSearch_WildcardsLiteral(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	pct := e.approved(t, p, "Giảm 50% vé")
	e.approved(t, p, "Vé A")
	e.approved(t, p, "Vé B")
	for _, q := range []string{"%", "_", `\`, "%%", "_%_"} {
		cards := e.search(t, listing.SearchFilter{Q: ptr(q)})
		if q == "%" {
			require.Equal(t, []string{pct.Slug}, slugs(cards), q)
			continue
		}
		require.Empty(t, cards, q)
	}
	_, err := e.Svc.Search(context.Background(), listing.SearchFilter{Q: ptr(strings.Repeat("a", 101))})
	requireErr(t, err, http.StatusBadRequest, "invalid_request")
	_, err = e.Svc.Search(context.Background(), listing.SearchFilter{Q: ptr(strings.Repeat("ạ", 100))})
	require.NoError(t, err, "100 runes is allowed")
}

func TestSearch_FilterByProvinceAndType(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l1 := e.approved(t, p, "Vé một")
	l2 := e.approved(t, p, "Vé hai")
	var otherProvince, otherWard string
	require.NoError(t, e.Pool.QueryRow(ctx,
		`SELECT province_code, code FROM wards WHERE province_code <> $1 ORDER BY code LIMIT 1`, *l1.ProvinceCode,
	).Scan(&otherProvince, &otherWard))
	_, err := e.Svc.Update(ctx, p.ID, p.OwnerUserID, l2.ID, listing.UpdateInput{ProvinceCode: &otherProvince, WardCode: &otherWard})
	require.NoError(t, err)

	require.Equal(t, []string{l1.Slug}, slugs(e.search(t, listing.SearchFilter{ProvinceCode: l1.ProvinceCode})))
	cards := e.search(t, listing.SearchFilter{ProvinceCode: &otherProvince, ServiceType: ptr(attraction)})
	require.Equal(t, []string{l2.Slug}, slugs(cards))
	require.Equal(t, otherProvince, *cards[0].ProvinceCode)
	require.Empty(t, e.search(t, listing.SearchFilter{ServiceType: ptr("self_drive_rental")}))
	// Without q: newest first; limit and offset page through.
	require.Equal(t, []string{l2.Slug, l1.Slug}, slugs(e.search(t, listing.SearchFilter{ServiceType: ptr(attraction)})))
	require.Equal(t, []string{l2.Slug}, slugs(e.search(t, listing.SearchFilter{Limit: 1})))
	require.Equal(t, []string{l1.Slug}, slugs(e.search(t, listing.SearchFilter{Limit: 1, Offset: 1})))
	require.Len(t, e.search(t, listing.SearchFilter{Limit: 500}), 2, "limit is capped, not rejected")
	for _, f := range []listing.SearchFilter{{Offset: 1001}, {Offset: -1}, {Limit: -1}} {
		_, err := e.Svc.Search(ctx, f)
		requireErr(t, err, http.StatusBadRequest, "invalid_request")
	}
	_, err = e.Svc.Search(ctx, listing.SearchFilter{Offset: 1000})
	require.NoError(t, err)
}

func TestSearch_PriceFromIsMinActive(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.approved(t, p, "Vé A") // one active ticket at 100000
	cheap := e.ticket(t, p, l, "Trẻ em", 50000)
	e.ticket(t, p, l, "Người cao tuổi", 80000)
	cards := e.search(t, listing.SearchFilter{})
	require.Len(t, cards, 1)
	require.Equal(t, ptr(int64(50000)), cards[0].PriceFrom)

	_, err := e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, cheap.ID, listing.TicketTypePatch{Active: ptr(false)})
	require.NoError(t, err)
	require.Equal(t, ptr(int64(80000)), e.search(t, listing.SearchFilter{})[0].PriceFrom)
}

func TestPublicDetail_404WhenNotVisible(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	draft := e.create(t, p, "Vé nháp")
	pending := e.submitted(t, p, "Vé chờ duyệt")
	hidden := e.approved(t, p, "Vé bị ẩn")
	_, err := e.Svc.Hide(ctx, e.Admin, hidden.ID, "Vi phạm")
	require.NoError(t, err)
	for _, slug := range []string{draft.Slug, pending.Slug, hidden.Slug, "khong-ton-tai"} {
		_, err := e.Svc.PublicDetail(ctx, slug)
		requireErr(t, err, http.StatusNotFound, "not_found")
	}

	q := e.partner(t, "Beta", attraction)
	ok := e.approved(t, q, "Vé B")
	_, err = e.Svc.PublicDetail(ctx, ok.Slug)
	require.NoError(t, err)
	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, q.ID, "Vi phạm"))
	_, err = e.Svc.PublicDetail(ctx, ok.Slug)
	requireErr(t, err, http.StatusNotFound, "not_found")
}

func TestPublicDetail_OnlyActiveTicketTypes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.approved(t, p, "Vé A")
	off := e.ticket(t, p, l, "Vé cũ", 20000)
	_, err := e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, off.ID, listing.TicketTypePatch{Active: ptr(false)})
	require.NoError(t, err)
	_, err = e.Svc.SetCancellationPolicy(ctx, p.ID, p.OwnerUserID, l.ID, []listing.CancellationTier{{HoursBefore: 48, RefundBps: 10000}})
	require.NoError(t, err)

	d, err := e.Svc.PublicDetail(ctx, l.Slug)
	require.NoError(t, err)
	require.Equal(t, l.Slug, d.Slug)
	require.Equal(t, ptr("Mô tả chi tiết"), d.Description)
	require.Equal(t, ptr("1 Đường A"), d.Address)
	require.Equal(t, l.WardCode, d.WardCode)
	require.Len(t, d.ImageURLs, 1)
	require.True(t, strings.HasPrefix(d.ImageURLs[0], "http://files.test/public/listing_image/"), d.ImageURLs[0])
	require.Equal(t, d.ImageURLs[0], *d.CoverURL)
	require.Equal(t, []listing.CancellationTier{{HoursBefore: 48, RefundBps: 10000}}, d.CancellationPolicy)
	require.NotNil(t, d.Attraction)
	require.Len(t, d.Attraction.TicketTypes, 1)
	require.Equal(t, "Người lớn", d.Attraction.TicketTypes[0].Name)
	require.Len(t, d.Attraction.TimeSlots, 1)
	require.Equal(t, ptr(int64(100000)), d.PriceFrom)
	require.Nil(t, d.RatingAvg)

	// rating_* stay 0 until plan 9; set them directly to check the average.
	_, err = e.Pool.Exec(ctx, `UPDATE listings SET rating_sum = 9, rating_count = 2 WHERE id = $1`, l.ID)
	require.NoError(t, err)
	d, err = e.Svc.PublicDetail(ctx, l.Slug)
	require.NoError(t, err)
	require.Equal(t, ptr(4.5), d.RatingAvg)
	require.Equal(t, 2, d.RatingCount)
}

func TestBookableListing_OK(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	l := e.approved(t, p, "Vé A")
	got, err := e.bookable(t, l.ID)
	require.NoError(t, err)
	require.Equal(t, l.ID, got.ID)
	require.Equal(t, p.ID, got.PartnerID)

	_, err = e.bookable(t, uuid.Must(uuid.NewV7()))
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")
	draft := e.create(t, p, "Vé nháp")
	_, err = e.bookable(t, draft.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")
}

// Review Focus 2.
func TestBookableListing_SuspendedNotBookable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.approved(t, p, "Vé A")
	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, p.ID, "Vi phạm"))
	_, err := e.bookable(t, l.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")
	require.Contains(t, err.Error(), "Dịch vụ này hiện không nhận đặt chỗ")

	require.NoError(t, e.Partner.Unsuspend(ctx, e.Admin, p.ID))
	require.NoError(t, e.Partner.DisableServiceType(ctx, e.Admin, p.ID, attraction))
	_, err = e.bookable(t, l.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")
}

func TestBookableListing_HiddenNotBookable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	hidden := e.approved(t, p, "Vé A")
	_, err := e.Svc.Hide(ctx, e.Admin, hidden.ID, "Vi phạm")
	require.NoError(t, err)
	_, err = e.bookable(t, hidden.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")

	unpub := e.approved(t, p, "Vé B")
	_, err = e.Svc.Unpublish(ctx, p.ID, p.OwnerUserID, unpub.ID)
	require.NoError(t, err)
	_, err = e.bookable(t, unpub.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")
}

// BookableListing holds the partners row FOR SHARE, so a concurrent Suspend
// (FOR NO KEY UPDATE) waits for the booking tx to end.
func TestBookableListing_BlocksConcurrentSuspend(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.approved(t, p, "Vé A")

	txA, err := e.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = txA.Rollback(ctx) }()
	_, err = e.Svc.BookableListing(ctx, txA, l.ID)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- e.Partner.Suspend(ctx, e.Admin, p.ID, "Vi phạm") }()
	select {
	case err := <-done:
		t.Fatalf("Suspend finished while the booking tx held the partner: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	require.NoError(t, txA.Commit(ctx))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Suspend did not finish after the booking tx committed")
	}
	_, err = e.bookable(t, l.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")
}

// Ruling 6: search, the public detail and BookableListing read the same
// visible_listings rule, so they agree on every state.
func TestVisibility_SearchDetailBookableAgree(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	susp := e.partner(t, "Beta", attraction)
	dis := e.partner(t, "Gamma", attraction)

	visible := e.approved(t, p, "Vé hiện")
	rejected := e.submitted(t, p, "Vé bị từ chối")
	_, err := e.Svc.Reject(ctx, e.Admin, rejected.ID, "Ảnh mờ")
	require.NoError(t, err)
	unpub := e.approved(t, p, "Vé tự gỡ")
	_, err = e.Svc.Unpublish(ctx, p.ID, p.OwnerUserID, unpub.ID)
	require.NoError(t, err)
	hidden := e.approved(t, p, "Vé bị ẩn")
	_, err = e.Svc.Hide(ctx, e.Admin, hidden.ID, "Vi phạm")
	require.NoError(t, err)
	ofSuspended := e.approved(t, susp, "Vé đối tác bị đình chỉ")
	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, susp.ID, "Vi phạm"))
	ofDisabled := e.approved(t, dis, "Vé loại bị tắt")
	require.NoError(t, e.Partner.DisableServiceType(ctx, e.Admin, dis.ID, attraction))

	notVisible := map[string]listing.Listing{
		"draft":             e.create(t, p, "Vé nháp"),
		"pending_review":    e.submitted(t, p, "Vé chờ duyệt"),
		"rejected":          rejected,
		"unpublished":       unpub,
		"hidden_by_admin":   hidden,
		"partner_suspended": ofSuspended,
		"type_disabled":     ofDisabled,
	}
	found := map[string]bool{}
	for _, c := range e.search(t, listing.SearchFilter{}) {
		found[c.Slug] = true
	}
	require.True(t, found[visible.Slug])
	_, err = e.Svc.PublicDetail(ctx, visible.Slug)
	require.NoError(t, err)
	_, err = e.bookable(t, visible.ID)
	require.NoError(t, err)

	for state, l := range notVisible {
		require.False(t, found[l.Slug], "search shows %s", state)
		_, err := e.Svc.PublicDetail(ctx, l.Slug)
		requireErr(t, err, http.StatusNotFound, "not_found")
		_, err = e.bookable(t, l.ID)
		requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")
	}
	require.Len(t, found, 1)
}

func TestHTTP_PublicSearchAndDetail(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	l := e.approved(t, p, "Khu du lịch Vũng Tàu Paradise")
	e.create(t, p, "Vũng Tàu nháp")

	rec := e.do(t, "GET", "/v1/listings?q="+url.QueryEscape("vung tau")+"&service_type="+attraction+"&province="+*l.ProvinceCode+"&limit=10&offset=0", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))
	page := decode[struct {
		Items []map[string]any `json:"items"`
	}](t, rec.Body.Bytes())
	require.Len(t, page.Items, 1)
	item := page.Items[0]
	require.Equal(t, l.Slug, item["slug"])
	require.Equal(t, float64(100000), item["price_from"])
	require.Nil(t, item["rating_avg"])
	require.NotContains(t, item, "id")
	require.NotContains(t, item, "partner_id")

	rec = e.do(t, "GET", "/v1/listings/"+l.Slug, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))
	d := decode[map[string]any](t, rec.Body.Bytes())
	require.Equal(t, l.Slug, d["slug"])
	require.Len(t, d["image_urls"], 1)
	require.NotNil(t, d["attraction"])
	require.NotNil(t, d["cancellation_policy"])

	rec = e.do(t, "GET", "/v1/listings?province=", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, "empty params are no filter")
	require.Len(t, decode[struct {
		Items []map[string]any `json:"items"`
	}](t, rec.Body.Bytes()).Items, 1)

	for _, path := range []string{"/v1/listings?limit=x", "/v1/listings?offset=-1", "/v1/listings?offset=1001"} {
		rec := e.do(t, "GET", path, "", nil)
		requireCode(t, rec, http.StatusBadRequest, "invalid_request")
		require.Empty(t, rec.Header().Get("Cache-Control"), path)
	}
	rec = e.do(t, "GET", "/v1/listings/khong-ton-tai", "", nil)
	requireCode(t, rec, http.StatusNotFound, "not_found")
	require.Empty(t, rec.Header().Get("Cache-Control"), "errors are not cached")
}
