package partner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

func ptr[T any](v T) *T { return &v }

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	return buf.Bytes()
}

// upload stores a PNG for purpose as uploaded by the partner user owner.
func (e *env) upload(t *testing.T, owner uuid.UUID, purpose string) uuid.UUID {
	t.Helper()
	f, err := e.Files.Upload(context.Background(), auth.Principal{UserID: owner, Role: auth.RolePartner}, purpose, bytes.NewReader(pngBytes(t)))
	require.NoError(t, err)
	return f.ID
}

func TestUpdateProfile_Partial(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(t, input("alpha", "attraction_ticket"))
	lic := e.upload(t, a.OwnerUserID, "business_license")

	got, err := e.Svc.UpdateProfile(ctx, a.ID, a.OwnerUserID, partner.ProfileUpdate{
		DisplayName:           ptr("  Alpha Travel "),
		TaxCode:               ptr("0312345678"),
		Address:               ptr("12 Lê Lợi"),
		ProvinceCode:          ptr("79"),
		WardCode:              ptr("25747"),
		ContactPhone:          ptr("+84 912.345-678"),
		ContactEmail:          ptr(" Sales@Alpha.VN "),
		BusinessLicenseFileID: &lic,
	})
	require.NoError(t, err)
	require.Equal(t, "Alpha Travel", got.DisplayName)
	require.Equal(t, "Công ty alpha", got.LegalName)
	require.Equal(t, "0312345678", *got.TaxCode)
	require.Equal(t, "12 Lê Lợi", *got.Address)
	require.Equal(t, "79", *got.ProvinceCode)
	require.Equal(t, "25747", *got.WardCode)
	require.Equal(t, "Nguyễn Văn An", got.ContactName) // untouched
	require.Equal(t, "0912345678", got.ContactPhone)
	require.Equal(t, "sales@alpha.vn", got.ContactEmail)
	require.Equal(t, lic, *got.BusinessLicenseFileID)
	require.Equal(t, []string{"attraction_ticket"}, got.ServiceTypes)

	// Only contact_name given: everything else is kept; "" clears address.
	got2, err := e.Svc.UpdateProfile(ctx, a.ID, a.OwnerUserID, partner.ProfileUpdate{
		ContactName: ptr("Trần Thị Bình"), Address: ptr(""),
	})
	require.NoError(t, err)
	require.Equal(t, "Trần Thị Bình", got2.ContactName)
	require.Nil(t, got2.Address)
	require.Equal(t, "Alpha Travel", got2.DisplayName)
	require.Equal(t, "0912345678", got2.ContactPhone)
	require.Equal(t, lic, *got2.BusinessLicenseFileID)

	stored, err := e.Svc.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, got2, stored)

	for name, u := range map[string]partner.ProfileUpdate{
		"empty display_name": {DisplayName: ptr("  ")},
		"long address":       {Address: ptr(string(bytes.Repeat([]byte("a"), 201)))},
		"bad phone":          {ContactPhone: ptr("12345")},
		"bad email":          {ContactEmail: ptr("not-an-email")},
	} {
		_, err := e.Svc.UpdateProfile(ctx, a.ID, a.OwnerUserID, u)
		status, code := appCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, status, name)
		require.Equal(t, "invalid_partner", code, name)
	}
}

func TestUpdateProfile_WardMustMatchProvince(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(t, input("alpha"))
	for name, u := range map[string]partner.ProfileUpdate{
		"ward of Hà Nội in HCM": {ProvinceCode: ptr("79"), WardCode: ptr("00004")},
		"province only":         {ProvinceCode: ptr("79")},
		"ward only":             {WardCode: ptr("25747")},
		"ward cleared only":     {ProvinceCode: ptr("79"), WardCode: ptr("")},
		"unknown codes":         {ProvinceCode: ptr("99"), WardCode: ptr("99999")},
	} {
		_, err := e.Svc.UpdateProfile(ctx, a.ID, a.OwnerUserID, u)
		status, code := appCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, status, name)
		require.Equal(t, "invalid_location", code, name)
	}
	got, err := e.Svc.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Nil(t, got.ProvinceCode)
	require.Nil(t, got.WardCode)

	// Both set, then both cleared.
	_, err = e.Svc.UpdateProfile(ctx, a.ID, a.OwnerUserID, partner.ProfileUpdate{ProvinceCode: ptr("01"), WardCode: ptr("00004")})
	require.NoError(t, err)
	got, err = e.Svc.UpdateProfile(ctx, a.ID, a.OwnerUserID, partner.ProfileUpdate{ProvinceCode: ptr(""), WardCode: ptr("")})
	require.NoError(t, err)
	require.Nil(t, got.ProvinceCode)
	require.Nil(t, got.WardCode)
}

func TestUpdateProfile_LicenseFileMustBeOwnBusinessLicense(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(t, input("alpha"))
	b := e.create(t, input("beta"))
	ofB := e.upload(t, b.OwnerUserID, "business_license")
	image := e.upload(t, a.OwnerUserID, "listing_image")
	missing := uuid.Must(uuid.NewV7())

	for name, id := range map[string]uuid.UUID{"file of B": ofB, "listing_image": image, "missing": missing} {
		_, err := e.Svc.UpdateProfile(ctx, a.ID, a.OwnerUserID, partner.ProfileUpdate{BusinessLicenseFileID: &id})
		status, code := appCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, status, name)
		require.Equal(t, "invalid_file", code, name)
	}
	got, err := e.Svc.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Nil(t, got.BusinessLicenseFileID)
}

func TestUpdateProfile_TaxCodeTaken409(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(t, input("alpha"))
	b := e.create(t, input("beta"))
	_, err := e.Svc.UpdateProfile(ctx, a.ID, a.OwnerUserID, partner.ProfileUpdate{TaxCode: ptr("0312345678")})
	require.NoError(t, err)

	_, err = e.Svc.UpdateProfile(ctx, b.ID, b.OwnerUserID, partner.ProfileUpdate{TaxCode: ptr(" 0312345678 ")})
	status, code := appCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "tax_code_taken", code)

	// Re-saving its own tax code is not a conflict.
	_, err = e.Svc.UpdateProfile(ctx, a.ID, a.OwnerUserID, partner.ProfileUpdate{TaxCode: ptr("0312345678")})
	require.NoError(t, err)
}

func TestUpdateProfile_SuspendedStillAllowed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(t, input("alpha"))
	require.NoError(t, e.Svc.Suspend(ctx, e.Admin, a.ID, "Vi phạm chính sách"))

	got, err := e.Svc.UpdateProfile(ctx, a.ID, a.OwnerUserID, partner.ProfileUpdate{DisplayName: ptr("Alpha mới")})
	require.NoError(t, err)
	require.Equal(t, "Alpha mới", got.DisplayName)
	require.Equal(t, partner.StatusSuspended, got.Status)
}

func TestHTTP_PartnerCannotTargetAnotherPartner(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	a := e.create(t, input("alpha"))
	b := e.create(t, input("beta"))
	tokA := e.token(t, a.OwnerUserID, auth.RolePartner)

	status, body := do(t, srv, "PATCH", "/v1/partner/profile", tokA, `{"display_name":"X","partner_id":"`+b.ID.String()+`"}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "invalid_request", errCode(t, body))
	gotA, err := e.Svc.Get(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, "alpha", gotA.DisplayName)

	status, body = do(t, srv, "PATCH", "/v1/partner/profile", tokA, `{"display_name":"X"}`)
	require.Equal(t, http.StatusOK, status, string(body))
	var p partner.Partner
	require.NoError(t, json.Unmarshal(body, &p))
	require.Equal(t, a.ID, p.ID)
	require.Equal(t, "X", p.DisplayName)

	gotB, err := e.Svc.Get(context.Background(), b.ID)
	require.NoError(t, err)
	require.Equal(t, b, gotB)

	status, body = do(t, srv, "GET", "/v1/partner/profile", tokA, "")
	require.Equal(t, http.StatusOK, status)
	require.NoError(t, json.Unmarshal(body, &p))
	require.Equal(t, a.ID, p.ID)

	// Other roles are rejected before membership is looked up.
	status, _ = do(t, srv, "GET", "/v1/partner/profile", e.token(t, e.Admin, auth.RoleSuperAdmin), "")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = do(t, srv, "GET", "/v1/partner/profile", "", "")
	require.Equal(t, http.StatusUnauthorized, status)
}

func TestCreate_ContactPhoneUsesIdentityRule(t *testing.T) {
	e := newEnv(t)
	in := input("alpha")
	in.ContactPhone = "+84 90.123-4567"
	p := e.create(t, in)
	require.Equal(t, "0901234567", p.ContactPhone)

	bad := input("beta")
	bad.ContactPhone = "12345"
	_, err := e.Svc.Create(context.Background(), e.Admin, bad)
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_partner", code)
}
