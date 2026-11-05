package payout

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestHTTP_AdminPayoutList_FilterStatus(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	q := e.fundedPartner(t, "beta")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	w, err := e.svc.Request(ctx, q.ID, q.Owner, 200_000)
	require.NoError(t, err)
	tok := e.adminToken(t, e.admin)

	status, body := do(t, srv, "POST", "/v1/admin/payouts/"+w.ID.String()+"/approve", tok, "")
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = do(t, srv, "POST", "/v1/admin/payouts/"+v.ID.String()+"/approve", tok, "{}")
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = do(t, srv, "POST", "/v1/admin/payouts/"+v.ID.String()+"/reject", tok, `{"reason":"Sai tài khoản"}`)
	require.Equal(t, http.StatusOK, status, string(body))
	var rj struct {
		Status       string `json:"status"`
		RejectReason string `json:"reject_reason"`
	}
	require.NoError(t, json.Unmarshal(body, &rj))
	require.Equal(t, "rejected", rj.Status)
	require.Equal(t, "Sai tài khoản", rj.RejectReason)

	status, body = do(t, srv, "GET", "/v1/admin/payouts?status=approved", tok, "")
	require.Equal(t, http.StatusOK, status, string(body))
	var page listPageJSON
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, w.ID.String(), page.Items[0]["id"])

	status, body = do(t, srv, "GET", "/v1/admin/payouts?partner_id="+p.ID.String(), tok, "")
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, v.ID.String(), page.Items[0]["id"])

	status, body = do(t, srv, "GET", "/v1/admin/payouts?limit=1", tok, "")
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Items, 1)
	require.NotNil(t, page.NextCursor)

	for _, qs := range []string{"status=bogus", "partner_id=x", "limit=0", "cursor=x"} {
		status, body = do(t, srv, "GET", "/v1/admin/payouts?"+qs, tok, "")
		require.Equal(t, http.StatusBadRequest, status, qs)
		require.Equal(t, "invalid_request", errCode(t, body))
	}

	// Detail.
	status, body = do(t, srv, "GET", "/v1/admin/payouts/"+w.ID.String(), tok, "")
	require.Equal(t, http.StatusOK, status, string(body))
	var got struct {
		ID          uuid.UUID                 `json:"id"`
		Status      string                    `json:"status"`
		Statement   []map[string]any          `json:"statement"`
		Balance     struct{ Available int64 } `json:"balance"`
		CurrentBank *struct {
			AccountNumber string `json:"account_number"`
		} `json:"current_bank"`
		CurrentBankDiffers bool       `json:"current_bank_differs"`
		ProofFileID        *uuid.UUID `json:"proof_file_id"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, w.ID, got.ID)
	require.Equal(t, "approved", got.Status)
	require.NotEmpty(t, got.Statement)
	require.Equal(t, int64(700_000), got.Balance.Available)
	require.NotNil(t, got.CurrentBank)
	require.Equal(t, "0011001234567", got.CurrentBank.AccountNumber)
	require.False(t, got.CurrentBankDiffers)
	require.Nil(t, got.ProofFileID)

	// Mark paid over HTTP.
	proof := e.uploadProof(t, e.admin, "payout_proof")
	status, body = do(t, srv, "POST", "/v1/admin/payouts/"+w.ID.String()+"/mark-paid", tok,
		`{"transfer_reference":"FT77","proof_file_id":"`+proof.String()+`"}`)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, "paid", got.Status)

	// State errors map to 409.
	status, body = do(t, srv, "POST", "/v1/admin/payouts/"+w.ID.String()+"/approve", tok, "")
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "invalid_payout_state", errCode(t, body))
}

func TestHTTP_AdminPayout_MalformedID404(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	tok := e.adminToken(t, e.admin)
	for _, id := range []string{"not-a-uuid", uuid.Must(uuid.NewV7()).String()} {
		for _, c := range []struct{ method, suffix, body string }{
			{"GET", "", ""},
			{"POST", "/approve", ""},
			// An unknown proof file, a missing proof and an empty reference:
			// the payout is looked up first, so all of them are 404.
			{"POST", "/mark-paid", `{"transfer_reference":"FT1","proof_file_id":"` + uuid.Must(uuid.NewV7()).String() + `"}`},
			{"POST", "/mark-paid", `{"transfer_reference":"FT1"}`},
			{"POST", "/mark-paid", `{"transfer_reference":"","proof_file_id":"` + uuid.Nil.String() + `"}`},
			{"POST", "/reject", `{"reason":"x"}`},
			{"POST", "/reject", `{"reason":""}`},
		} {
			status, body := do(t, srv, c.method, "/v1/admin/payouts/"+id+c.suffix, tok, c.body)
			require.Equal(t, http.StatusNotFound, status, "%s %s: %s", c.method, c.suffix, body)
			require.Equal(t, "payout_not_found", errCode(t, body))
		}
	}
}

func TestHTTP_MarkPaid_BadBody422(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	_, err = e.svc.Approve(ctx, e.admin, v.ID)
	require.NoError(t, err)
	tok := e.adminToken(t, e.admin)
	proof := e.uploadProof(t, e.admin, "payout_proof")
	path := "/v1/admin/payouts/" + v.ID.String() + "/mark-paid"

	for body, code := range map[string]string{
		`{"transfer_reference":"  ","proof_file_id":"` + proof.String() + `"}`:                    "invalid_transfer_reference",
		`{"proof_file_id":"` + proof.String() + `"}`:                                              "invalid_transfer_reference",
		`{"transfer_reference":"FT1"}`:                                                            "proof_file_required",
		`{"transfer_reference":"FT1","proof_file_id":null}`:                                       "proof_file_required",
		`{"transfer_reference":"FT1","proof_file_id":"` + uuid.Nil.String() + `"}`:                "proof_file_required",
		`{"transfer_reference":"FT1","proof_file_id":"` + uuid.Must(uuid.NewV7()).String() + `"}`: "invalid_file",
	} {
		status, b := do(t, srv, "POST", path, tok, body)
		require.Equal(t, http.StatusUnprocessableEntity, status, "%s: %s", body, b)
		require.Equal(t, code, errCode(t, b), body)
	}
	// Malformed JSON or unknown fields are 400.
	for _, body := range []string{``, `{`, `{"transfer_reference":"FT1","proof_file_id":"` + proof.String() + `","x":1}`} {
		status, b := do(t, srv, "POST", path, tok, body)
		require.Equal(t, http.StatusBadRequest, status, "%s: %s", body, b)
	}
	status, b := do(t, srv, "POST", "/v1/admin/payouts/"+v.ID.String()+"/reject", tok, `{"reason":""}`)
	require.Equal(t, http.StatusUnprocessableEntity, status, string(b))
	require.Equal(t, "invalid_reject_reason", errCode(t, b))
	status, b = do(t, srv, "POST", "/v1/admin/payouts/"+v.ID.String()+"/approve", tok, `{"x":1}`)
	require.Equal(t, http.StatusBadRequest, status, string(b))
	require.Equal(t, StatusApproved, e.statusOf(t, v.ID))
}

func TestHTTP_AdminPayout_PartnerRole403(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	v, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	tok := e.token(t, p.Owner)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/admin/payouts", ""},
		{"GET", "/v1/admin/payouts/" + v.ID.String(), ""},
		{"POST", "/v1/admin/payouts/" + v.ID.String() + "/approve", ""},
		{"POST", "/v1/admin/payouts/" + v.ID.String() + "/mark-paid", `{"transfer_reference":"FT1","proof_file_id":"` + uuid.Nil.String() + `"}`},
		{"POST", "/v1/admin/payouts/" + v.ID.String() + "/reject", `{"reason":"x"}`},
	} {
		status, _ := do(t, srv, c.method, c.path, tok, c.body)
		require.Equal(t, http.StatusForbidden, status, c.path)
		status, _ = do(t, srv, c.method, c.path, "", c.body)
		require.Equal(t, http.StatusUnauthorized, status, c.path)
	}
	require.Equal(t, StatusRequested, e.statusOf(t, v.ID))
}
