package listing_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

// Ruling 7b: slot instants are built on the Vietnam calendar date.
func TestTimeSlot_StartAtEndAtVietnamDate(t *testing.T) {
	slot := listing.TimeSlot{StartTime: "08:00", EndTime: "10:30"}
	// 18:00Z on 8 Nov is already 01:00 on 9 Nov in Vietnam.
	evening := time.Date(2026, 11, 8, 18, 0, 0, 0, time.UTC)
	start, err := slot.StartAt(evening)
	require.NoError(t, err)
	end, err := slot.EndAt(evening)
	require.NoError(t, err)
	require.True(t, start.Equal(time.Date(2026, 11, 9, 8, 0, 0, 0, clock.VN)), start)
	require.True(t, end.Equal(time.Date(2026, 11, 9, 3, 30, 0, 0, time.UTC)), end)
	require.Equal(t, clock.VN, start.Location())

	// A VN-midnight date and a UTC-morning date of the same VN day agree.
	for _, d := range []time.Time{
		time.Date(2026, 11, 9, 0, 0, 0, 0, clock.VN),
		time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC),
	} {
		s, err := slot.StartAt(d)
		require.NoError(t, err)
		require.True(t, s.Equal(start), d)
	}

	_, err = listing.TimeSlot{StartTime: "25:00", EndTime: "10:00"}.StartAt(evening)
	require.Error(t, err)
}

// Ruling 7c.
func TestTicketTypesForBooking_EmptyIDs422(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	e.ticket(t, p, l, "A", 5000)
	for _, ids := range [][]uuid.UUID{nil, {}} {
		err := func() error {
			tx, err := e.Pool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(ctx) }()
			_, err = e.Svc.TicketTypesForBooking(ctx, tx, l.ID, ids)
			return err
		}()
		requireErr(t, err, http.StatusUnprocessableEntity, "invalid_ticket_type")
	}
}

// Ruling 7d: Approve re-checks completeness under the lock.
func TestApprove_RechecksComplete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.submitted(t, p, "Vé A")
	d := e.attractionDetail(t, l.ID)
	_, err := e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, d.TicketTypes[0].ID, listing.TicketTypePatch{Active: ptr(false)})
	require.NoError(t, err)

	_, err = e.Svc.Approve(ctx, e.Admin, l.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_incomplete")
	require.Contains(t, err.Error(), "loại vé")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM listings WHERE id = $1 AND review_status = 'pending_review'`, l.ID))
	require.Zero(t, e.count(t, `SELECT count(*) FROM admin_audit_log WHERE action = 'listing.approve'`))

	_, err = e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, d.TicketTypes[0].ID, listing.TicketTypePatch{Active: ptr(true)})
	require.NoError(t, err)
	ap, err := e.Svc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	require.Equal(t, listing.ReviewApproved, ap.ReviewStatus)
}

// Ruling 7e: the closed-date PUT body is optional.
func TestHTTP_ClosedDateEmptyBody(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	tok := e.token(t, p.OwnerUserID)
	base := "/v1/partner/listings/" + l.ID.String() + "/closed-dates/"

	rec := e.do(t, "PUT", base+"2027-01-01", tok, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM attraction_closed_dates WHERE date = '2027-01-01' AND reason IS NULL`))
	rec = e.do(t, "PUT", base+"2027-01-02", tok, `{"reason":"Tết"}`)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	requireCode(t, e.do(t, "PUT", base+"2027-01-03", tok, `{"reason":`), http.StatusBadRequest, "invalid_request")
}

// Ruling 7f: BookableListing holds the listings row FOR SHARE, so a
// concurrent admin Hide (FOR NO KEY UPDATE) waits for the booking tx.
func TestBookableListing_BlocksConcurrentHide(t *testing.T) {
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
	go func() {
		_, err := e.Svc.Hide(ctx, e.Admin, l.ID, "Vi phạm")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Hide finished while the booking tx held the listing: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	require.NoError(t, txA.Commit(ctx))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Hide did not finish after the booking tx committed")
	}
	_, err = e.bookable(t, l.ID)
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")
}

// Ruling 7g.
func TestUpdate_ClearCoordinates(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	_, err := e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{Latitude: ptr(10.5), Longitude: ptr(106.7)})
	require.NoError(t, err)

	for _, in := range []listing.UpdateInput{
		{ClearCoordinates: true, Latitude: ptr(1.0)},
		{ClearCoordinates: true, Longitude: ptr(1.0)},
		{ClearCoordinates: true, Latitude: ptr(1.0), Longitude: ptr(2.0)},
	} {
		_, err := e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, in)
		requireErr(t, err, http.StatusUnprocessableEntity, "invalid_listing")
	}
	base := len(e.audits(t, l.ID))

	rec := e.do(t, "PATCH", "/v1/partner/listings/"+l.ID.String(), e.token(t, p.OwnerUserID), map[string]any{"clear_coordinates": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decode[listing.Listing](t, rec.Body.Bytes())
	require.Nil(t, got.Latitude)
	require.Nil(t, got.Longitude)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM listings WHERE id = $1 AND latitude IS NULL AND longitude IS NULL`, l.ID))

	au := e.audits(t, l.ID)
	require.Len(t, au, base+1)
	last := au[len(au)-1]
	require.Equal(t, "update", last.Action)
	require.Equal(t, 10.5, last.Changes["latitude"].Old)
	require.Nil(t, last.Changes["latitude"].New)
	require.Equal(t, 106.7, last.Changes["longitude"].Old)
	require.Nil(t, last.Changes["longitude"].New)

	// Clearing again changes nothing and logs nothing.
	_, err = e.Svc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{ClearCoordinates: true})
	require.NoError(t, err)
	require.Len(t, e.audits(t, l.ID), base+1)
}

// Ruling 7i: images already in the gallery (uploaded by someone else) can be
// kept and re-ordered; only new file ids are ownership-checked; the handler
// returns the gallery built in the tx.
func TestSetImages_ReorderKeepsForeignExisting(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.partner(t, "Alpha", attraction)
	b := e.partner(t, "Beta", attraction)
	l := e.create(t, a, "Vé A")
	own := e.upload(t, a.OwnerUserID, "listing_image")
	_, _, err := e.Svc.SetImages(ctx, a.ID, a.OwnerUserID, l.ID, []uuid.UUID{own}, nil)
	require.NoError(t, err)
	// A gallery image uploaded by another user (e.g. a former staff member).
	foreign := e.upload(t, b.OwnerUserID, "listing_image")
	_, err = e.Pool.Exec(ctx, `INSERT INTO listing_images (id, listing_id, file_id, sort_order) VALUES ($1, $2, $3, 1)`,
		uuid.Must(uuid.NewV7()), l.ID, foreign)
	require.NoError(t, err)

	got, images, err := e.Svc.SetImages(ctx, a.ID, a.OwnerUserID, l.ID, []uuid.UUID{foreign, own}, &own)
	require.NoError(t, err)
	require.Equal(t, []listing.Image{{FileID: foreign, SortOrder: 0}, {FileID: own, SortOrder: 1}}, images)
	require.Equal(t, &own, got.CoverFileID)
	require.Equal(t, []uuid.UUID{foreign, own}, e.imageIDs(t, l.ID))

	// A new foreign file is still refused.
	another := e.upload(t, b.OwnerUserID, "listing_image")
	_, _, err = e.Svc.SetImages(ctx, a.ID, a.OwnerUserID, l.ID, []uuid.UUID{foreign, own, another}, nil)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_file")

	// HTTP: the response is the gallery written by the tx.
	rec := e.do(t, "PUT", "/v1/partner/listings/"+l.ID.String()+"/images", e.token(t, a.OwnerUserID),
		map[string]any{"file_ids": []uuid.UUID{own, foreign}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decode[struct {
		Images      []listing.Image `json:"images"`
		CoverFileID *uuid.UUID      `json:"cover_file_id"`
	}](t, rec.Body.Bytes())
	require.Equal(t, []listing.Image{{FileID: own, SortOrder: 0}, {FileID: foreign, SortOrder: 1}}, body.Images)
	require.Equal(t, &own, body.CoverFileID)
}

// Ruling 7j: a patch that changes nothing writes no audit row.
func TestAttraction_NoopPatchesNotAudited(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	tt := e.ticket(t, p, l, "NL", 5000)
	sl, err := e.slot(t, p, l, "08:00", "10:00", mon)
	require.NoError(t, err)
	base := len(e.audits(t, l.ID))

	got, err := e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, tt.ID, listing.TicketTypePatch{})
	require.NoError(t, err)
	require.Equal(t, tt, got)
	got, err = e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, tt.ID, listing.TicketTypePatch{Price: ptr(int64(5000)), Name: ptr(" NL ")})
	require.NoError(t, err)
	require.Equal(t, tt, got)
	gs, err := e.Svc.UpdateTimeSlot(ctx, p.ID, p.OwnerUserID, l.ID, sl.ID, listing.TimeSlotPatch{StartTime: ptr("08:00"), Active: ptr(true)})
	require.NoError(t, err)
	require.Equal(t, sl, gs)
	require.Len(t, e.audits(t, l.ID), base)

	// A real change is still logged.
	_, err = e.Svc.UpdateTicketType(ctx, p.ID, p.OwnerUserID, l.ID, tt.ID, listing.TicketTypePatch{Price: ptr(int64(6000))})
	require.NoError(t, err)
	_, err = e.Svc.UpdateTimeSlot(ctx, p.ID, p.OwnerUserID, l.ID, sl.ID, listing.TimeSlotPatch{EndTime: ptr("11:00")})
	require.NoError(t, err)
	require.Len(t, e.audits(t, l.ID), base+2)
}

// Ruling 7k: a ticket type or slot of listing A cannot be patched through
// listing B's URL, even by the same partner.
func TestHTTP_ChildOfOtherListingSamePartner404(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	la := e.create(t, p, "Vé A")
	lb := e.create(t, p, "Vé B")
	ttA := e.ticket(t, p, la, "NL", 5000)
	slA, err := e.slot(t, p, la, "08:00", "10:00", mon)
	require.NoError(t, err)
	tok := e.token(t, p.OwnerUserID)
	baseB := "/v1/partner/listings/" + lb.ID.String()
	before := len(e.audits(t, la.ID))

	requireCode(t, e.do(t, "PATCH", baseB+"/ticket-types/"+ttA.ID.String(), tok, map[string]any{"price": 9000, "active": false}),
		http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "PATCH", baseB+"/time-slots/"+slA.ID.String(), tok, map[string]any{"active": false}),
		http.StatusNotFound, "not_found")

	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM attraction_ticket_types WHERE id = $1 AND listing_id = $2 AND price = 5000 AND active`, ttA.ID, la.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM attraction_time_slots WHERE id = $1 AND listing_id = $2 AND active`, slA.ID, la.ID))
	require.Len(t, e.audits(t, la.ID), before)
	require.Len(t, e.audits(t, lb.ID), 1) // only B's creation
}

// Ruling 7l: closed dates at most 730 days ahead (Vietnam calendar).
func TestClosedDate_AtMost730DaysAhead(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	// testNow is 2026-11-04 09:00 ICT; +730 days is 2028-11-03.
	require.NoError(t, e.Svc.AddClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, time.Date(2028, 11, 3, 0, 0, 0, 0, clock.VN), nil))
	err := e.Svc.AddClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, time.Date(2028, 11, 4, 0, 0, 0, 0, clock.VN), nil)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_closed_date")
	require.Contains(t, err.Error(), "730")
	// 17:30Z on 3 Nov 2028 is already 4 Nov in Vietnam.
	err = e.Svc.AddClosedDate(ctx, p.ID, p.OwnerUserID, l.ID, time.Date(2028, 11, 3, 17, 30, 0, 0, time.UTC), nil)
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_closed_date")
	requireCode(t, e.do(t, "PUT", "/v1/partner/listings/"+l.ID.String()+"/closed-dates/2099-01-01", e.token(t, p.OwnerUserID), nil),
		http.StatusUnprocessableEntity, "invalid_closed_date")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM attraction_closed_dates`))
}
