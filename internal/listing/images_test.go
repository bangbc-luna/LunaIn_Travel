package listing_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

func (e *env) policy(t *testing.T, listingID uuid.UUID) []listing.CancellationTier {
	t.Helper()
	var out []listing.CancellationTier
	require.NoError(t, db.InTx(context.Background(), e.Pool, func(tx pgx.Tx) (err error) {
		out, err = e.Svc.CancellationPolicy(context.Background(), tx, listingID)
		return err
	}))
	return out
}

func (e *env) imageIDs(t *testing.T, listingID uuid.UUID) []uuid.UUID {
	t.Helper()
	var out []uuid.UUID
	rows, err := e.Pool.Query(context.Background(), `SELECT file_id FROM listing_images WHERE listing_id = $1 ORDER BY sort_order`, listingID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestSetImages_OrderAndCover(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	f1, f2, f3 := e.upload(t, p.OwnerUserID, "listing_image"), e.upload(t, p.OwnerUserID, "listing_image"), e.upload(t, p.OwnerUserID, "listing_image")

	got, _, err := e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{f2, f1, f3}, nil)
	require.NoError(t, err)
	require.Equal(t, &f2, got.CoverFileID)
	require.Equal(t, []uuid.UUID{f2, f1, f3}, e.imageIDs(t, l.ID))

	var imgs []listing.Image
	require.NoError(t, db.InTx(ctx, e.Pool, func(tx pgx.Tx) (err error) {
		imgs, err = e.Svc.Images(ctx, tx, l.ID)
		return err
	}))
	require.Len(t, imgs, 3)
	require.Equal(t, 0, imgs[0].SortOrder)
	require.Equal(t, f3, imgs[2].FileID)
	require.Equal(t, 2, imgs[2].SortOrder)

	got, _, err = e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{f2, f1, f3}, &f3)
	require.NoError(t, err)
	require.Equal(t, &f3, got.CoverFileID)

	audits := e.audits(t, l.ID)
	require.Len(t, audits, 3)
	require.Equal(t, "images", audits[1].Action)
	require.Equal(t, []any{f2.String(), f1.String(), f3.String()}, audits[1].Changes["file_ids"].New)
	require.Equal(t, []any{f2.String(), f1.String(), f3.String()}, audits[2].Changes["file_ids"].Old)
	require.Equal(t, f3.String(), audits[2].Changes["cover_file_id"].New)
	require.Equal(t, f2.String(), audits[2].Changes["cover_file_id"].Old)
}

func TestSetImages_CoverMustBeInList(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	f1, f2 := e.upload(t, p.OwnerUserID, "listing_image"), e.upload(t, p.OwnerUserID, "listing_image")
	_, _, err := e.Svc.SetImages(context.Background(), p.ID, p.OwnerUserID, l.ID, []uuid.UUID{f1}, &f2)
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_listing", code)
	require.Empty(t, e.imageIDs(t, l.ID))

	_, _, err = e.Svc.SetImages(context.Background(), p.ID, p.OwnerUserID, l.ID, nil, &f2)
	_, code = appCode(t, err)
	require.Equal(t, "invalid_listing", code)
}

func TestSetImages_FileOfOtherUser422(t *testing.T) {
	e := newEnv(t)
	a := e.partner(t, "Alpha", attraction)
	b := e.partner(t, "Beta", attraction)
	l := e.create(t, a, "Vé A")
	other := e.upload(t, b.OwnerUserID, "listing_image")
	_, _, err := e.Svc.SetImages(context.Background(), a.ID, a.OwnerUserID, l.ID, []uuid.UUID{other}, nil)
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_file", code)
	_, _, err = e.Svc.SetImages(context.Background(), a.ID, a.OwnerUserID, l.ID, []uuid.UUID{uuid.Must(uuid.NewV7())}, nil)
	_, code = appCode(t, err)
	require.Equal(t, "invalid_file", code)
}

func TestSetImages_WrongPurpose422(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	lic := e.upload(t, p.OwnerUserID, "business_license")
	_, _, err := e.Svc.SetImages(context.Background(), p.ID, p.OwnerUserID, l.ID, []uuid.UUID{lic}, nil)
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_file", code)
}

func TestSetImages_Max20AndDuplicates(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	ids := make([]uuid.UUID, 21)
	for i := range ids {
		ids[i] = e.upload(t, p.OwnerUserID, "listing_image")
	}
	_, _, err := e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, ids, nil)
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_listing", code)
	_, _, err = e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, ids[:20], nil)
	require.NoError(t, err)
	require.Len(t, e.imageIDs(t, l.ID), 20)

	_, _, err = e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{ids[0], ids[0]}, nil)
	_, code = appCode(t, err)
	require.Equal(t, "invalid_listing", code)
	require.Len(t, e.imageIDs(t, l.ID), 20, "failed replace changes nothing")
}

func TestSetImages_ReplaceShrinks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.partner(t, "Alpha", attraction)
	l := e.create(t, p, "Vé A")
	f1, f2 := e.upload(t, p.OwnerUserID, "listing_image"), e.upload(t, p.OwnerUserID, "listing_image")
	_, _, err := e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{f1, f2}, nil)
	require.NoError(t, err)

	got, _, err := e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{f2}, nil)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{f2}, e.imageIDs(t, l.ID))
	require.Equal(t, &f2, got.CoverFileID)

	got, _, err = e.Svc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, nil, nil)
	require.NoError(t, err)
	require.Empty(t, e.imageIDs(t, l.ID))
	require.Nil(t, got.CoverFileID)
}

func TestSetImages_SuspendedAndForeign(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.partner(t, "Alpha", attraction)
	b := e.partner(t, "Beta", attraction)
	l := e.create(t, a, "Vé A")
	f := e.upload(t, b.OwnerUserID, "listing_image")
	_, _, err := e.Svc.SetImages(ctx, b.ID, b.OwnerUserID, l.ID, []uuid.UUID{f}, nil)
	require.ErrorIs(t, err, listing.ErrListingNotFound)

	fa := e.upload(t, a.OwnerUserID, "listing_image")
	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, a.ID, "test"))
	_, _, err = e.Svc.SetImages(ctx, a.ID, a.OwnerUserID, l.ID, []uuid.UUID{fa}, nil)
	require.ErrorIs(t, err, listing.ErrPartnerSuspended)
}
