package listing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

// complete makes a listing ready for review: all required fields, one image,
// one active ticket type and one active time slot.
func (e *env) complete(t *testing.T, p partner.Partner, title string) listing.Listing {
	t.Helper()
	ctx := context.Background()
	province, ward, _ := e.location(t)
	l := e.create(t, p, title)
	_, err := e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{
		Description: ptr("Mô tả chi tiết"), Address: ptr("1 Đường A"), ProvinceCode: &province, WardCode: &ward,
	})
	require.NoError(t, err)
	_, err = e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{e.upload(t, p.OwnerUserID, "listing_image")}, nil)
	require.NoError(t, err)
	e.ticket(t, p, l, "Người lớn", 100000)
	_, err = e.slot(t, p, l, "08:00", "12:00", monToFri)
	require.NoError(t, err)
	got, err := e.Svc.Get(ctx, p.ID, l.ID)
	require.NoError(t, err)
	return got
}

func (e *env) submitted(t *testing.T, p partner.Partner, title string) listing.Listing {
	t.Helper()
	l := e.complete(t, p, title)
	out, err := e.Svc.Submit(context.Background(), p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	return out
}

func requireErr(t *testing.T, err error, status int, code string) {
	t.Helper()
	require.Error(t, err)
	s, c := appCode(t, err)
	require.Equal(t, status, s)
	require.Equal(t, code, c)
}

func (e *env) attractionDetail(t *testing.T, id uuid.UUID) listing.AttractionDetail {
	t.Helper()
	d, err := e.Svc.AdminGet(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, d.Attraction)
	return *d.Attraction
}

func TestSubmit_IncompleteListsMissingFields(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	_, err := e.Svc.Submit(context.Background(), p.ID, p.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_incomplete")
	for _, want := range []string{"mô tả", "địa chỉ", "tỉnh", "ảnh", "loại vé", "khung giờ"} {
		require.Contains(t, err.Error(), want)
	}
	got, err := e.Svc.Get(context.Background(), p.ID, l.ID)
	require.NoError(t, err)
	require.Equal(t, listing.ReviewDraft, got.ReviewStatus)
}

func TestSubmit_InactiveTicketAndSlotDoNotCount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.complete(t, p, "Vé A")
	d := e.attractionDetail(t, l.ID)
	_, err := e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, d.TicketTypes[0].ID, listing.TicketTypePatch{Active: ptr(false)})
	require.NoError(t, err)
	_, err = e.Svc.UpdateTimeSlot(ctx, p.ID, p.OwnerUserID, l.ID, d.TimeSlots[0].ID, listing.TimeSlotPatch{Active: ptr(false)})
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_incomplete")
	require.Contains(t, err.Error(), "loại vé")
	require.Contains(t, err.Error(), "khung giờ")
}

func TestFullFlow_SubmitApprove(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.complete(t, p, "Vé A")

	sub, err := e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	require.Equal(t, listing.ReviewPendingReview, sub.ReviewStatus)
	require.NotNil(t, sub.SubmittedAt)
	require.True(t, sub.SubmittedAt.Equal(testNow))

	e.Clock.T = testNow.Add(time.Hour)
	ap, err := e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	require.Equal(t, listing.ReviewApproved, ap.ReviewStatus)
	require.Equal(t, "published", ap.Visibility)
	require.NotNil(t, ap.ReviewedAt)
	require.True(t, ap.ReviewedAt.Equal(testNow.Add(time.Hour)))
	var by uuid.UUID
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT reviewed_by FROM listings WHERE id = $1`, l.ID).Scan(&by))
	require.Equal(t, e.Admin, by)

	var acts []string
	for _, a := range e.audits(t, l.ID) {
		acts = append(acts, a.Action)
	}
	require.Equal(t, "submit", acts[len(acts)-1])
}

func TestReject_ThenEditAndResubmit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.submitted(t, p, "Vé A")

	_, err := e.Svc.Reject(ctx, e.Admin, l.ID, "")
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_listing")
	long := make([]rune, 501)
	for i := range long {
		long[i] = 'a'
	}
	_, err = e.Svc.Reject(ctx, e.Admin, l.ID, string(long))
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_listing")

	rj, err := e.Svc.Reject(ctx, e.Admin, l.ID, "  Ảnh mờ  ")
	require.NoError(t, err)
	require.Equal(t, listing.ReviewRejected, rj.ReviewStatus)
	require.Equal(t, ptr("Ảnh mờ"), rj.RejectionReason)

	_, err = e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{Title: ptr("Vé A (mới)")})
	require.NoError(t, err)
	re, err := e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	require.Equal(t, listing.ReviewPendingReview, re.ReviewStatus)
	require.Nil(t, re.RejectionReason)
}

func TestApprovedEditStaysLiveAndIsLogged(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.submitted(t, p, "Vé A")
	_, err := e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)

	got, err := e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{Title: ptr("Vé A đã sửa")})
	require.NoError(t, err)
	require.Equal(t, listing.ReviewApproved, got.ReviewStatus)
	require.Equal(t, "published", got.Visibility)

	audits := e.audits(t, l.ID)
	last := audits[len(audits)-1]
	require.Equal(t, "update", last.Action)
	require.Equal(t, "Vé A", last.Changes["title"].Old)
	require.Equal(t, "Vé A đã sửa", last.Changes["title"].New)
}

func TestHide_PartnerCannotPublish(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.submitted(t, p, "Vé A")
	_, err := e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)

	h, err := e.Svc.Hide(ctx, e.Admin, l.ID, "Vi phạm")
	require.NoError(t, err)
	require.Equal(t, "hidden_by_admin", h.Visibility)
	require.Equal(t, ptr("Vi phạm"), h.HiddenReason)

	_, err = e.Svc.Publish(ctx, p.ID, p.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")
	_, err = e.Svc.Unpublish(ctx, p.ID, p.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")
	_, err = e.Svc.Hide(ctx, e.Admin, l.ID, "lại")
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")
	_, err = e.Svc.Hide(ctx, e.Admin, uuid.Nil, "x")
	requireErr(t, err, http.StatusNotFound, "not_found")
}

func TestUnhide(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.submitted(t, p, "Vé A")
	_, err := e.Svc.Unhide(ctx, e.Admin, l.ID)
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")
	_, err = e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	_, err = e.Svc.Hide(ctx, e.Admin, l.ID, "Vi phạm")
	require.NoError(t, err)
	u, err := e.Svc.Unhide(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	require.Equal(t, "published", u.Visibility)
	require.Nil(t, u.HiddenReason)
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM listings WHERE id = $1 AND (hidden_at IS NOT NULL OR hidden_by IS NOT NULL)`, l.ID))
}

// Ruling 4: Unhide restores the partner's prior visibility.
func TestUnhide_RestoresPartnerUnpublished(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.approved(t, p, "Vé A")
	_, err := e.Svc.Unpublish(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	h, err := e.Svc.Hide(ctx, e.Admin, l.ID, "Vi phạm")
	require.NoError(t, err)
	require.Equal(t, "hidden_by_admin", h.Visibility)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM listings WHERE id = $1 AND hidden_from_visibility = 'unpublished_by_partner'`, l.ID))

	u, err := e.Svc.Unhide(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	require.Equal(t, "unpublished_by_partner", u.Visibility)
	require.Nil(t, u.HiddenReason)
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM listings WHERE id = $1 AND hidden_from_visibility IS NOT NULL`, l.ID))
	require.Empty(t, e.search(t, listing.SearchFilter{}))

	var before, after []byte
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT before, after FROM admin_audit_log
		WHERE action = 'listing.unhide' AND target_id = $1`, l.ID).Scan(&before, &after))
	require.JSONEq(t, `{"review_status":"approved","visibility":"hidden_by_admin"}`, string(before))
	require.JSONEq(t, `{"review_status":"approved","visibility":"unpublished_by_partner"}`, string(after))

	// The partner can publish again afterwards.
	pub, err := e.Svc.Publish(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	require.Equal(t, "published", pub.Visibility)

	// The schema refuses a hidden row that forgot its prior visibility.
	_, err = e.Pool.Exec(ctx, `UPDATE listings SET visibility = 'hidden_by_admin', hidden_reason = 'x' WHERE id = $1`, l.ID)
	require.Error(t, err)
}

func TestUnpublishPublish(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.submitted(t, p, "Vé A")
	_, err := e.Svc.Unpublish(ctx, p.ID, p.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")
	_, err = e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	_, err = e.Svc.Publish(ctx, p.ID, p.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")

	u, err := e.Svc.Unpublish(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	require.Equal(t, "unpublished_by_partner", u.Visibility)
	pub, err := e.Svc.Publish(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	require.Equal(t, "published", pub.Visibility)

	var acts []string
	for _, a := range e.audits(t, l.ID) {
		acts = append(acts, a.Action)
	}
	require.Equal(t, []string{"submit", "unpublish", "publish"}, acts[len(acts)-3:])
}

func TestInvalidTransitions409(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	draft := e.complete(t, p, "Vé A")
	_, err := e.Svc.Approve(ctx, e.Admin, draft.ID)
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")
	_, err = e.Svc.Reject(ctx, e.Admin, draft.ID, "x")
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")
	_, err = e.Svc.Hide(ctx, e.Admin, draft.ID, "x")
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")

	l := e.submitted(t, p, "Vé B")
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")
	_, err = e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	_, err = e.Svc.Reject(ctx, e.Admin, l.ID, "x")
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")
	_, err = e.Svc.Approve(ctx, e.Admin, l.ID)
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusConflict, "invalid_listing_state")
}

func TestPartnerTransitions_ForeignListing404(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.partner(t, "Alpha", attraction)
	b := e.partner(t, "Beta", attraction)
	l := e.complete(t, a, "Vé A")
	_, err := e.Svc.Submit(ctx, b.ID, b.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.Publish(ctx, b.ID, b.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.Unpublish(ctx, b.ID, b.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")
}

func TestAdminActionsAudited(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.submitted(t, p, "Vé A")
	_, err := e.Svc.Reject(ctx, e.Admin, l.ID, "Ảnh mờ")
	require.NoError(t, err)
	_, err = e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	_, err = e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	_, err = e.Svc.Hide(ctx, e.Admin, l.ID, "Vi phạm")
	require.NoError(t, err)
	_, err = e.Svc.Unhide(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	// A rejected transition writes no audit row.
	_, err = e.Svc.Approve(ctx, e.Admin, l.ID)
	require.Error(t, err)

	rows, err := e.Pool.Query(ctx, `SELECT action, actor_user_id, target_kind, target_id, before, after, reason
		FROM admin_audit_log WHERE target_kind = 'listing' ORDER BY created_at, id`)
	require.NoError(t, err)
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action, kind string
		var actor, target uuid.UUID
		var before, after []byte
		var reason *string
		require.NoError(t, rows.Scan(&action, &actor, &kind, &target, &before, &after, &reason))
		require.Equal(t, e.Admin, actor)
		require.Equal(t, l.ID, target)
		require.NotEmpty(t, before)
		require.NotEmpty(t, after)
		var a map[string]any
		require.NoError(t, json.Unmarshal(after, &a))
		require.Contains(t, a, "review_status")
		require.Contains(t, a, "visibility")
		if action == "listing.reject" || action == "listing.hide" {
			require.NotNil(t, reason)
		}
		actions = append(actions, action)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"listing.reject", "listing.approve", "listing.hide", "listing.unhide"}, actions)
}

func ids(ls []listing.Listing) []uuid.UUID {
	out := make([]uuid.UUID, len(ls))
	for i, l := range ls {
		out[i] = l.ID
	}
	return out
}

func TestAdminList_PendingOldestFirst(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	q := e.partner(t, "Beta", attraction)
	a := e.complete(t, p, "Vé A")
	b := e.complete(t, p, "Vé B")
	c := e.complete(t, q, "Vé C")
	// Submit out of creation order: B, C, A.
	for i, x := range []struct {
		l listing.Listing
		p partner.Partner
	}{{b, p}, {c, q}, {a, p}} {
		e.Clock.T = testNow.Add(time.Duration(i) * time.Minute)
		_, err := e.Svc.Submit(ctx, x.p.ID, x.p.OwnerUserID, x.l.ID)
		require.NoError(t, err)
	}
	_ = e.complete(t, p, "Vé D") // draft, not listed as pending

	pending := "pending_review"
	items, next, err := e.Svc.AdminList(ctx, listing.AdminFilter{ReviewStatus: &pending, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{b.ID, c.ID}, ids(items))
	require.NotNil(t, next)
	items, next, err = e.Svc.AdminList(ctx, listing.AdminFilter{ReviewStatus: &pending, Limit: 2, Cursor: next})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{a.ID}, ids(items))
	require.Nil(t, next)

	items, _, err = e.Svc.AdminList(ctx, listing.AdminFilter{PartnerID: &q.ID})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{c.ID}, ids(items))

	all, _, err := e.Svc.AdminList(ctx, listing.AdminFilter{})
	require.NoError(t, err)
	require.Len(t, all, 4)
	bad := "nope"
	_, _, err = e.Svc.AdminList(ctx, listing.AdminFilter{ReviewStatus: &bad})
	require.Error(t, err)
}

func TestAdminGet_Detail(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.submitted(t, p, "Vé A")
	_, err := e.Svc.SetCancellationPolicy(ctx, p.ID, p.OwnerUserID, l.ID, []listing.CancellationTier{{HoursBefore: 48, RefundBps: 10000}})
	require.NoError(t, err)
	d, err := e.Svc.AdminGet(ctx, l.ID)
	require.NoError(t, err)
	require.Equal(t, l.ID, d.Listing.ID)
	require.Len(t, d.Images, 1)
	require.Len(t, d.CancellationPolicy, 1)
	require.Len(t, d.Attraction.TicketTypes, 1)
	require.NotEmpty(t, d.AuditLog)
	require.LessOrEqual(t, len(d.AuditLog), 20)
	require.Equal(t, "cancellation_policy", d.AuditLog[0].Action) // newest first
	_, err = e.Svc.AdminGet(ctx, uuid.Nil)
	requireErr(t, err, http.StatusNotFound, "not_found")
}

func TestSubmit_SuspendedPartner403(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.complete(t, p, "Vé A")
	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, p.ID, "Vi phạm"))
	_, err := e.Svc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	requireErr(t, err, http.StatusForbidden, "partner_suspended")
}

func (e *env) adminToken(t *testing.T) string {
	t.Helper()
	s, _, err := e.Issuer.Issue(auth.Principal{UserID: e.Admin, Role: auth.RoleSuperAdmin})
	require.NoError(t, err)
	return s
}

func TestHTTP_AdminReviewFlow(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	l := e.complete(t, p, "Vé A")
	pt, at := e.token(t, p.OwnerUserID), e.adminToken(t)
	base := "/v1/partner/listings/" + l.ID.String()
	adm := "/v1/admin/listings/" + l.ID.String()

	requireCode(t, e.do(t, "GET", "/v1/admin/listings", pt, nil), http.StatusForbidden, "forbidden")
	requireCode(t, e.do(t, "POST", adm+"/approve", "", nil), http.StatusUnauthorized, "unauthorized")

	// Approve before submit: 409.
	requireCode(t, e.do(t, "POST", adm+"/approve", at, nil), http.StatusConflict, "invalid_listing_state")
	rec := e.do(t, "POST", base+"/submit", pt, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = e.do(t, "GET", "/v1/admin/listings?review_status=pending_review", at, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page struct {
		Items []listing.Listing `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	requireCode(t, e.do(t, "GET", "/v1/admin/listings?review_status=zzz", at, nil), http.StatusBadRequest, "invalid_request")

	rec = e.do(t, "GET", adm, at, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"audit_log"`)

	requireCode(t, e.do(t, "POST", adm+"/reject", at, map[string]string{"reason": ""}), http.StatusUnprocessableEntity, "invalid_listing")
	requireCode(t, e.do(t, "POST", adm+"/reject", at, `{"reason":"x","zz":"1"}`), http.StatusBadRequest, "invalid_request")
	rec = e.do(t, "POST", adm+"/reject", at, map[string]string{"reason": "Ảnh mờ"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"rejected"`)
	require.Equal(t, http.StatusOK, e.do(t, "POST", base+"/submit", pt, nil).Code)
	require.Equal(t, http.StatusOK, e.do(t, "POST", adm+"/approve", at, nil).Code)

	require.Equal(t, http.StatusOK, e.do(t, "POST", adm+"/hide", at, map[string]string{"reason": "Vi phạm"}).Code)
	requireCode(t, e.do(t, "POST", base+"/publish", pt, nil), http.StatusConflict, "invalid_listing_state")
	require.Equal(t, http.StatusOK, e.do(t, "POST", adm+"/unhide", at, nil).Code)
	require.Equal(t, http.StatusOK, e.do(t, "POST", base+"/unpublish", pt, nil).Code)
	require.Equal(t, http.StatusOK, e.do(t, "POST", base+"/publish", pt, nil).Code)

	// 404 for unknown / malformed ids and foreign partner.
	requireCode(t, e.do(t, "POST", "/v1/admin/listings/"+uuid.Nil.String()+"/approve", at, nil), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "GET", "/v1/admin/listings/zzz", at, nil), http.StatusNotFound, "not_found")
	b := e.partner(t, "Beta", attraction)
	requireCode(t, e.do(t, "POST", base+"/unpublish", e.token(t, b.OwnerUserID), nil), http.StatusNotFound, "not_found")
}
