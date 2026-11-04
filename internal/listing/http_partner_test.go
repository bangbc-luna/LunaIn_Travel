package listing_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/listing"
)

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(body, &v), string(body))
	return v
}

func TestHTTP_PartnerListingCRUD(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	tok := e.token(t, p.OwnerUserID)

	rec := e.do(t, "POST", "/v1/partner/listings", tok, map[string]any{
		"service_type": attraction, "title": "Vé Bà Nà Hills", "summary": "Cáp treo",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created := decode[listing.Listing](t, rec.Body.Bytes())
	require.Equal(t, "draft", created.ReviewStatus)
	require.Equal(t, p.ID, created.PartnerID)
	require.Regexp(t, slugRe, created.Slug)

	rec = e.do(t, "GET", "/v1/partner/listings/"+created.ID.String(), tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "Vé Bà Nà Hills", decode[listing.Listing](t, rec.Body.Bytes()).Title)

	rec = e.do(t, "PATCH", "/v1/partner/listings/"+created.ID.String(), tok, map[string]any{"title": "Vé Bà Nà"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	updated := decode[listing.Listing](t, rec.Body.Bytes())
	require.Equal(t, "Vé Bà Nà", updated.Title)
	require.Equal(t, created.Slug, updated.Slug)

	rec = e.do(t, "GET", "/v1/partner/listings?limit=10&review_status=draft", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page := decode[struct {
		Items      []listing.Listing `json:"items"`
		NextCursor *uuid.UUID        `json:"next_cursor"`
	}](t, rec.Body.Bytes())
	require.Len(t, page.Items, 1)
	require.Equal(t, created.ID, page.Items[0].ID)
	require.Nil(t, page.NextCursor)

	rec = e.do(t, "GET", "/v1/partner/listings?review_status=approved", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"items":[],"next_cursor":null}`, rec.Body.String())

	requireCode(t, e.do(t, "GET", "/v1/partner/listings?limit=abc", tok, nil), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "GET", "/v1/partner/listings?cursor=xyz", tok, nil), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "POST", "/v1/partner/listings", tok, map[string]any{"service_type": attraction, "title": ""}),
		http.StatusUnprocessableEntity, "invalid_listing")
	require.Len(t, e.audits(t, created.ID), 2)
}

func TestHTTP_CannotReadOtherPartnersListing404(t *testing.T) {
	e := newEnv(t)
	a := e.partner(t, "Alpha", attraction)
	b := e.partner(t, "Beta", attraction)
	lb := e.create(t, b, "Vé của B")
	tokA := e.token(t, a.OwnerUserID)

	requireCode(t, e.do(t, "GET", "/v1/partner/listings/"+lb.ID.String(), tokA, nil), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "PATCH", "/v1/partner/listings/"+lb.ID.String(), tokA, map[string]any{"title": "Bị chiếm"}),
		http.StatusNotFound, "not_found")

	rec := e.do(t, "GET", "/v1/partner/listings", tokA, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"items":[],"next_cursor":null}`, rec.Body.String())

	got, err := e.Svc.Get(t.Context(), b.ID, lb.ID)
	require.NoError(t, err)
	require.Equal(t, "Vé của B", got.Title)
}

func TestHTTP_MalformedID404(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	tok := e.token(t, p.OwnerUserID)
	requireCode(t, e.do(t, "GET", "/v1/partner/listings/not-a-uuid", tok, nil), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "PATCH", "/v1/partner/listings/not-a-uuid", tok, map[string]any{"title": "x"}), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "GET", "/v1/partner/listings/"+uuid.NewString(), tok, nil), http.StatusNotFound, "not_found")
}

func TestHTTP_UnknownFieldRejected(t *testing.T) {
	e := newEnv(t)
	p := e.partner(t, "Alpha", attraction)
	other := e.partner(t, "Beta", attraction)
	tok := e.token(t, p.OwnerUserID)

	requireCode(t, e.do(t, "POST", "/v1/partner/listings", tok, map[string]any{
		"service_type": attraction, "title": "Vé A", "partner_id": other.ID,
	}), http.StatusBadRequest, "invalid_request")
	require.Zero(t, e.count(t, `SELECT count(*) FROM listings`))

	l := e.create(t, p, "Vé A")
	requireCode(t, e.do(t, "PATCH", "/v1/partner/listings/"+l.ID.String(), tok, map[string]any{
		"partner_id": other.ID,
	}), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "PATCH", "/v1/partner/listings/"+l.ID.String(), tok, map[string]any{
		"service_type": "scheduled_ride",
	}), http.StatusBadRequest, "invalid_request")
	got, err := e.Svc.Get(t.Context(), p.ID, l.ID)
	require.NoError(t, err)
	require.Equal(t, p.ID, got.PartnerID)
	require.Equal(t, attraction, got.ServiceType)
}
