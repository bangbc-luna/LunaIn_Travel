package payout

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var eb struct {
		Error struct{ Code string } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &eb), string(body))
	return eb.Error.Code
}

type listPageJSON struct {
	Items      []map[string]any `json:"items"`
	NextCursor *uuid.UUID       `json:"next_cursor"`
}

func TestHTTP_RequestPayout201(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	p := e.fundedPartner(t, "alpha")
	tok := e.token(t, p.Owner)

	status, body := do(t, srv, "POST", "/v1/partner/payouts", tok, `{"amount":300000}`)
	require.Equal(t, http.StatusCreated, status, string(body))
	var got struct {
		ID        uuid.UUID `json:"id"`
		PartnerID uuid.UUID `json:"partner_id"`
		Amount    int64     `json:"amount"`
		Status    string    `json:"status"`
		Bank      struct {
			BankBIN       string `json:"bank_bin"`
			AccountNumber string `json:"account_number"`
			AccountHolder string `json:"account_holder"`
		} `json:"bank"`
		StatementSeqFrom int64            `json:"statement_seq_from"`
		StatementSeqTo   int64            `json:"statement_seq_to"`
		OpeningAvailable int64            `json:"opening_available"`
		ClosingAvailable int64            `json:"closing_available"`
		Statement        []map[string]any `json:"statement"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, p.ID, got.PartnerID)
	require.Equal(t, int64(300_000), got.Amount)
	require.Equal(t, "requested", got.Status)
	require.Equal(t, "NGUYEN VAN AN", got.Bank.AccountHolder)
	require.Equal(t, "0011001234567", got.Bank.AccountNumber)
	require.Equal(t, int64(1), got.StatementSeqFrom)
	require.Equal(t, int64(0), got.OpeningAvailable)
	require.Equal(t, int64(600_000), got.ClosingAvailable)
	require.Len(t, got.Statement, 2)

	// Errors map to the documented codes.
	status, body = do(t, srv, "POST", "/v1/partner/payouts", tok, `{"amount":100000}`)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "payout_already_open", errCode(t, body))
	status, body = do(t, srv, "POST", "/v1/partner/payouts", tok, `{"amount":0}`)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_amount", errCode(t, body))

	// Preview over HTTP.
	status, body = do(t, srv, "GET", "/v1/partner/payouts/preview", tok, "")
	require.Equal(t, http.StatusOK, status, string(body))
	var pv struct {
		Available    int64      `json:"available"`
		OpenPayoutID *uuid.UUID `json:"open_payout_id"`
		CanRequest   bool       `json:"can_request"`
	}
	require.NoError(t, json.Unmarshal(body, &pv))
	require.Equal(t, int64(600_000), pv.Available)
	require.NotNil(t, pv.OpenPayoutID)
	require.Equal(t, got.ID, *pv.OpenPayoutID)
	require.False(t, pv.CanRequest)

	// Get it back.
	status, body = do(t, srv, "GET", "/v1/partner/payouts/"+got.ID.String(), tok, "")
	require.Equal(t, http.StatusOK, status, string(body))
}

func TestHTTP_GetOtherPartnersPayout404(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	a := e.fundedPartner(t, "alpha")
	b := e.fundedPartner(t, "beta")
	vb, err := e.svc.Request(context.Background(), b.ID, b.Owner, 300_000)
	require.NoError(t, err)

	status, body := do(t, srv, "GET", "/v1/partner/payouts/"+vb.ID.String(), e.token(t, a.Owner), "")
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "payout_not_found", errCode(t, body))

	_, err = e.svc.GetMine(context.Background(), a.ID, vb.ID)
	requireCode(t, err, http.StatusNotFound, "payout_not_found")

	status, _ = do(t, srv, "GET", "/v1/partner/payouts/"+vb.ID.String(), e.token(t, b.Owner), "")
	require.Equal(t, http.StatusOK, status)
}

func TestHTTP_MalformedID404(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	p := e.fundedPartner(t, "alpha")
	tok := e.token(t, p.Owner)
	for _, id := range []string{"abc", "123", uuid.Must(uuid.NewV7()).String()} {
		status, body := do(t, srv, "GET", "/v1/partner/payouts/"+id, tok, "")
		require.Equal(t, http.StatusNotFound, status, id)
		require.Equal(t, "payout_not_found", errCode(t, body), id)
	}
}

func TestHTTP_UnknownField400(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	p := e.fundedPartner(t, "alpha")
	tok := e.token(t, p.Owner)
	for _, body := range []string{
		`{"amount":300000,"partner_id":"` + p.ID.String() + `"}`,
		`{"amount":"300000"}`,
		`not json`,
	} {
		status, res := do(t, srv, "POST", "/v1/partner/payouts", tok, body)
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Equal(t, "invalid_request", errCode(t, res), body)
	}
	require.Equal(t, 0, countWhere(t, e, `SELECT count(*) FROM payouts`))
}

func TestHTTP_ListMine(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	ctx := context.Background()
	p := e.fundedPartner(t, "alpha")
	other := e.fundedPartner(t, "beta")
	_, err := e.svc.Request(ctx, other.ID, other.Owner, 100_000)
	require.NoError(t, err)
	v1, err := e.svc.Request(ctx, p.ID, p.Owner, 300_000)
	require.NoError(t, err)
	e.reject(t, v1)
	v2, err := e.svc.Request(ctx, p.ID, p.Owner, 200_000)
	require.NoError(t, err)
	tok := e.token(t, p.Owner)

	list := func(query string) listPageJSON {
		t.Helper()
		status, body := do(t, srv, "GET", "/v1/partner/payouts"+query, tok, "")
		require.Equal(t, http.StatusOK, status, string(body))
		var page listPageJSON
		require.NoError(t, json.Unmarshal(body, &page))
		return page
	}
	page := list("")
	require.Len(t, page.Items, 2)
	require.Equal(t, v2.ID.String(), page.Items[0]["id"])
	require.Equal(t, v1.ID.String(), page.Items[1]["id"])
	require.Nil(t, page.NextCursor)

	page = list("?limit=1")
	require.Len(t, page.Items, 1)
	require.Equal(t, v2.ID.String(), page.Items[0]["id"])
	require.NotNil(t, page.NextCursor)
	page = list("?limit=1&cursor=" + page.NextCursor.String())
	require.Len(t, page.Items, 1)
	require.Equal(t, v1.ID.String(), page.Items[0]["id"])
	require.Nil(t, page.NextCursor)

	page = list("?status=rejected")
	require.Len(t, page.Items, 1)
	require.Equal(t, v1.ID.String(), page.Items[0]["id"])

	for _, q := range []string{"?status=bogus", "?limit=0", "?cursor=xyz"} {
		status, body := do(t, srv, "GET", "/v1/partner/payouts"+q, tok, "")
		require.Equal(t, http.StatusBadRequest, status, q)
		require.Equal(t, "invalid_request", errCode(t, body), q)
	}
}
