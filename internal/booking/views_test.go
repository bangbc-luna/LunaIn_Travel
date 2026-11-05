package booking_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

type page[T any] struct {
	Items      []T        `json:"items"`
	NextCursor *uuid.UUID `json:"next_cursor"`
}

// Review Focus 4: customer B never sees or acts on customer A's booking.
func TestGetMine_OtherCustomer404(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := e.create(t, "a-key")
	b := e.customer(t, "b@gmail.com")

	got, err := e.Svc.GetMine(ctx, e.Customer, v.ID)
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
	_, err = e.Svc.GetMine(ctx, b, v.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")
	_, err = e.Svc.Pay(ctx, b, v.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")

	tokA, tokB := e.token(t, e.Customer, auth.RoleCustomer), e.token(t, b, auth.RoleCustomer)
	requireCode(t, e.do(t, "GET", "/v1/bookings/"+v.ID.String(), tokB, nil), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/pay", tokB, nil), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "GET", "/v1/bookings/not-a-uuid", tokA, nil), http.StatusNotFound, "not_found")
	requireCode(t, e.do(t, "GET", "/v1/bookings/"+uuid.Nil.String(), tokA, nil), http.StatusNotFound, "not_found")

	rec := e.do(t, "GET", "/v1/bookings", tokB, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, decode[page[booking.BookingSummary]](t, rec).Items)
	rec = e.do(t, "GET", "/v1/bookings", tokA, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	mine := decode[page[booking.BookingSummary]](t, rec)
	require.Len(t, mine.Items, 1)
	require.Equal(t, v.ID, mine.Items[0].ID)
	require.Equal(t, v.Code, mine.Items[0].Code)
	require.Equal(t, int64(380_000), mine.Items[0].GrossAmount)

	rec = e.do(t, "GET", "/v1/bookings/"+v.ID.String(), tokA, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, v.ID, decode[booking.BookingView](t, rec).ID)

	// Partner and admin tokens are not customers.
	requireCode(t, e.do(t, "GET", "/v1/bookings/"+v.ID.String(), e.token(t, e.P.OwnerUserID, auth.RolePartner), nil),
		http.StatusForbidden, "forbidden")
}

func TestPartnerView_NoQRToken(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := e.create(t, "qr")

	// Pending: no qr_token anywhere, the customer sees the payment.
	rec := e.do(t, "GET", "/v1/bookings/"+v.ID.String(), e.token(t, e.Customer, auth.RoleCustomer), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), `"qr_token"`)
	require.Contains(t, rec.Body.String(), `"checkout_url"`)

	e.confirm(t, v.ID)
	mine, err := e.Svc.GetMine(ctx, e.Customer, v.ID)
	require.NoError(t, err)
	require.NotNil(t, mine.QRToken, "the owning customer sees the QR once confirmed")
	require.Nil(t, mine.Payment, "payment is shown only while pending_payment")
	rec = e.do(t, "GET", "/v1/bookings/"+v.ID.String(), e.token(t, e.Customer, auth.RoleCustomer), nil)
	require.Contains(t, rec.Body.String(), `"qr_token":"`+*mine.QRToken+`"`)

	pv, err := e.Svc.GetForPartner(ctx, e.P.ID, v.ID)
	require.NoError(t, err)
	require.Nil(t, pv.QRToken)
	require.NotNil(t, pv.CommissionAmount)
	require.Equal(t, int64(38_000), *pv.CommissionAmount)

	ptok := e.token(t, e.P.OwnerUserID, auth.RolePartner)
	rec = e.do(t, "GET", "/v1/partner/bookings/"+v.ID.String(), ptok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "qr_token")
	require.NotContains(t, rec.Body.String(), *mine.QRToken)
	rec = e.do(t, "GET", "/v1/partner/bookings?status=confirmed", ptok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	list := decode[page[booking.BookingSummary]](t, rec)
	require.Len(t, list.Items, 1)
	require.Equal(t, v.ID, list.Items[0].ID)
	rec = e.do(t, "GET", "/v1/partner/bookings?status=pending_payment", ptok, nil)
	require.Empty(t, decode[page[booking.BookingSummary]](t, rec).Items)

	// Another partner: 404 and an empty list.
	other, err := e.Partner.Create(ctx, e.Admin, partner.CreateInput{
		LegalName: "Công ty Beta", DisplayName: "Beta",
		OwnerEmail: "owner@beta.vn", OwnerPassword: "matkhau123",
		ServiceTypes: []string{attraction},
	})
	require.NoError(t, err)
	_, err = e.Svc.GetForPartner(ctx, other.ID, v.ID)
	requireErr(t, err, http.StatusNotFound, "not_found")
	otok := e.token(t, other.OwnerUserID, auth.RolePartner)
	requireCode(t, e.do(t, "GET", "/v1/partner/bookings/"+v.ID.String(), otok, nil), http.StatusNotFound, "not_found")
	rec = e.do(t, "GET", "/v1/partner/bookings", otok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, decode[page[booking.BookingSummary]](t, rec).Items)
}

func TestAdminGet_History(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := e.create(t, "admin")
	e.confirm(t, v.ID)

	d, err := e.Svc.AdminGet(ctx, v.ID)
	require.NoError(t, err)
	require.Equal(t, v.ID, d.ID)
	require.Nil(t, d.QRToken)
	require.Equal(t, e.Customer, d.CustomerID)
	require.Equal(t, e.P.ID, d.PartnerID)
	require.Len(t, d.History, 1)
	h := d.History[0]
	require.Nil(t, h.FromStatus)
	require.Equal(t, "pending_payment", h.ToStatus)
	require.Equal(t, "customer", h.ActorKind)
	require.Equal(t, e.Customer, *h.ActorUserID)

	_, err = e.Svc.AdminGet(ctx, uuid.Must(uuid.NewV7()))
	requireErr(t, err, http.StatusNotFound, "not_found")

	atok := e.token(t, e.Admin, auth.RoleSuperAdmin)
	rec := e.do(t, "GET", "/v1/admin/bookings/"+v.ID.String(), atok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"status_history"`)
	require.NotContains(t, rec.Body.String(), "qr_token")
	rec = e.do(t, "GET", "/v1/admin/bookings?code="+v.Code, atok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	list := decode[page[booking.BookingSummary]](t, rec)
	require.Len(t, list.Items, 1)
	require.Equal(t, v.ID, list.Items[0].ID)
	rec = e.do(t, "GET", "/v1/admin/bookings?partner_id="+uuid.Nil.String(), atok, nil)
	require.Empty(t, decode[page[booking.BookingSummary]](t, rec).Items)
	requireCode(t, e.do(t, "GET", "/v1/admin/bookings?status=nope", atok, nil), http.StatusBadRequest, "invalid_request")
	requireCode(t, e.do(t, "GET", "/v1/admin/bookings/"+v.ID.String(), e.token(t, e.Customer, auth.RoleCustomer), nil),
		http.StatusForbidden, "forbidden")
}
