package listing_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestHTTP_ImagesAndPolicyOtherPartner404(t *testing.T) {
	e := newEnv(t)
	a := e.partner(t, "Alpha", attraction)
	b := e.partner(t, "Beta", attraction)
	l := e.create(t, a, "Vé A")
	atok, btok := e.token(t, a.OwnerUserID), e.token(t, b.OwnerUserID)
	path := "/v1/partner/listings/" + l.ID.String()
	fb := e.upload(t, b.OwnerUserID, "listing_image")

	fa := e.upload(t, a.OwnerUserID, "listing_image")
	rec := e.do(t, "PUT", path+"/images", atok, map[string]any{"file_ids": []uuid.UUID{fa}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	out := decode[struct {
		Images []struct {
			FileID    uuid.UUID `json:"file_id"`
			SortOrder int       `json:"sort_order"`
		} `json:"images"`
		CoverFileID *uuid.UUID `json:"cover_file_id"`
	}](t, rec.Body.Bytes())
	require.Len(t, out.Images, 1)
	require.Equal(t, fa, out.Images[0].FileID)
	require.Equal(t, &fa, out.CoverFileID)

	rec = e.do(t, "PUT", path+"/cancellation-policy", atok, map[string]any{"tiers": []map[string]int{{"hours_before": 24, "refund_bps": 5000}}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = e.do(t, "GET", path+"/cancellation-policy", atok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"tiers":[{"hours_before":24,"refund_bps":5000}]}`, rec.Body.String())

	requireCode(t, e.do(t, "PUT", path+"/cancellation-policy", atok,
		map[string]any{"tiers": []map[string]int{{"hours_before": 24, "refund_bps": 10000}, {"hours_before": 48, "refund_bps": 5000}}}),
		http.StatusUnprocessableEntity, "invalid_cancellation_policy")
	requireCode(t, e.do(t, "PUT", path+"/images", atok, `{"file_ids":[],"zzz":1}`), http.StatusBadRequest, "invalid_request")

	// Partner B gets 404 on every route, and A's data is untouched.
	requireCode(t, e.do(t, "PUT", path+"/images", btok, map[string]any{"file_ids": []uuid.UUID{fb}}), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "PUT", path+"/cancellation-policy", btok, map[string]any{"tiers": []any{}}), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "GET", path+"/cancellation-policy", btok, nil), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "PUT", "/v1/partner/listings/not-a-uuid/images", atok, map[string]any{"file_ids": []any{}}), http.StatusNotFound, "not_found")
	require.Equal(t, []uuid.UUID{fa}, e.imageIDs(t, l.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM listing_cancellation_tiers WHERE listing_id = $1`, l.ID))
}
