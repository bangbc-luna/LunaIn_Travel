package partner_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
)

func TestHTTP_AdminCreatePartnerFlow(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	adminTok := e.token(t, e.Admin, auth.RoleSuperAdmin)

	body := `{"legal_name":"Công ty Alpha","display_name":"Alpha","contact_name":"Nguyễn Văn An",` +
		`"contact_phone":"0901234567","contact_email":"lienhe@alpha.vn","owner_email":"owner@alpha.vn",` +
		`"owner_password":"` + ownerPassword + `","service_types":[]}`
	status, raw := do(t, srv, "POST", "/v1/admin/partners", adminTok, body)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var created partner.Partner
	require.NoError(t, json.Unmarshal(raw, &created))
	require.Equal(t, "Alpha", created.DisplayName)

	// The owner can sign in and sees their own partner.
	tokens, err := e.Identity.Login(context.Background(), "owner@alpha.vn", ownerPassword)
	require.NoError(t, err)
	status, raw = do(t, srv, "GET", "/v1/partner/profile", tokens.AccessToken, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	var prof partner.Partner
	require.NoError(t, json.Unmarshal(raw, &prof))
	require.Equal(t, created.ID, prof.ID)
	require.Equal(t, "Alpha", prof.DisplayName)

	// Unknown body field is rejected.
	status, raw = do(t, srv, "POST", "/v1/admin/partners", adminTok, `{"legal_name":"x","bogus":1}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "invalid_request", errCode(t, raw))

	// Duplicate owner email -> 409 email_taken.
	status, raw = do(t, srv, "POST", "/v1/admin/partners", adminTok, body)
	require.Equal(t, http.StatusConflict, status, string(raw))
	require.Equal(t, "email_taken", errCode(t, raw))
}

func TestHTTP_AdminPartnerLifecycle(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	tok := e.token(t, e.Admin, auth.RoleSuperAdmin)
	a := e.create(t, input("alpha"))
	e.create(t, input("beta"))
	base := "/v1/admin/partners/" + a.ID.String()

	// get / bad id / unknown id
	status, raw := do(t, srv, "GET", base, tok, "")
	require.Equal(t, http.StatusOK, status)
	var got partner.Partner
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, a.ID, got.ID)
	status, raw = do(t, srv, "GET", "/v1/admin/partners/not-a-uuid", tok, "")
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "not_found", errCode(t, raw))
	status, raw = do(t, srv, "GET", "/v1/admin/partners/"+uuid.NewString(), tok, "")
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "partner_not_found", errCode(t, raw))

	// list with pagination
	status, raw = do(t, srv, "GET", "/v1/admin/partners?limit=1", tok, "")
	require.Equal(t, http.StatusOK, status)
	var page struct {
		Items      []partner.Partner `json:"items"`
		NextCursor *uuid.UUID        `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(raw, &page))
	require.Len(t, page.Items, 1)
	require.NotNil(t, page.NextCursor)
	status, _ = do(t, srv, "GET", "/v1/admin/partners?limit=abc", tok, "")
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = do(t, srv, "GET", "/v1/admin/partners?cursor=zzz", tok, "")
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = do(t, srv, "GET", "/v1/admin/partners?status=bogus", tok, "")
	require.Equal(t, http.StatusBadRequest, status)

	// service types
	status, _ = do(t, srv, "PUT", base+"/service-types/attraction_ticket", tok, "")
	require.Equal(t, http.StatusNoContent, status)
	status, _ = do(t, srv, "DELETE", base+"/service-types/attraction_ticket", tok, "")
	require.Equal(t, http.StatusNoContent, status)
	status, raw = do(t, srv, "PUT", base+"/service-types/nope", tok, "")
	require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))

	// suspend / unsuspend
	status, raw = do(t, srv, "POST", base+"/suspend", tok, `{"reason":"Vi phạm","x":1}`)
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	status, _ = do(t, srv, "POST", base+"/suspend", tok, `{"reason":"Vi phạm"}`)
	require.Equal(t, http.StatusNoContent, status)
	status, raw = do(t, srv, "POST", base+"/suspend", tok, `{"reason":"Lại"}`)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "partner_already_suspended", errCode(t, raw))
	status, _ = do(t, srv, "POST", base+"/unsuspend", tok, "")
	require.Equal(t, http.StatusNoContent, status)
	status, raw = do(t, srv, "POST", base+"/unsuspend", tok, "")
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "partner_not_suspended", errCode(t, raw))
}
