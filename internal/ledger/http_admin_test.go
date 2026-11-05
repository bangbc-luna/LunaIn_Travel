package ledger_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

type adminEnv struct {
	*fixture
	mux   *http.ServeMux
	iss   *auth.Issuer
	admin uuid.UUID
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	f := newFixture(t)
	e := &adminEnv{fixture: f, mux: http.NewServeMux(), admin: newID(t)}
	f.mustExec(t, `INSERT INTO users (id, email, role) VALUES ($1, 'admin@lunain.test', 'super_admin')`, e.admin)
	e.iss = auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, f.clock)
	partnerSvc := partner.NewService(partner.Deps{Pool: f.pool, Clock: f.clock})
	f.svc.AdminRoutes(httpx.NewRecordingRouter(e.mux), e.iss, partnerSvc)
	return e
}

func (e *adminEnv) do(t *testing.T, user uuid.UUID, role auth.Role, method, path, body string) (int, []byte) {
	t.Helper()
	tok, _, err := e.iss.Issue(auth.Principal{UserID: user, Role: role})
	require.NoError(t, err)
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (e *adminEnv) asAdmin(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	return e.do(t, e.admin, auth.RoleSuperAdmin, method, path, body)
}

func errCode(t *testing.T, raw []byte) string {
	t.Helper()
	var eb struct {
		Error struct{ Code string } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &eb), string(raw))
	return eb.Error.Code
}

func TestHTTP_AdminAdjustment_AuditedAndPosted(t *testing.T) {
	e := newAdminEnv(t)
	b := e.withAvailable900k(t)
	path := "/v1/admin/partners/" + e.partner.String() + "/adjustments"

	status, raw := e.asAdmin(t, "POST", path, `{"amount":-70000,"reason":"  Trừ phí xử lý  ","booking_id":"`+b.BookingID.String()+`"}`)
	require.Equal(t, http.StatusCreated, status, string(raw))
	require.JSONEq(t, `{"seq":4,"available_after":830000}`, string(raw))
	e.requireBalance(t, e.partner, 0, 830_000)

	var (
		n           int
		actor       uuid.UUID
		targetKind  string
		targetID    uuid.UUID
		after       []byte
		entryReason string
		createdBy   uuid.UUID
	)
	ctx := context.Background()
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_log WHERE action = 'ledger.adjustment'`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT actor_user_id, target_kind, target_id, after
		FROM admin_audit_log WHERE action = 'ledger.adjustment'`).Scan(&actor, &targetKind, &targetID, &after))
	require.Equal(t, e.admin, actor)
	require.Equal(t, "partner", targetKind)
	require.Equal(t, e.partner, targetID)
	require.JSONEq(t, `{"amount":-70000,"booking_id":"`+b.BookingID.String()+`","seq":4}`, string(after))
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT reason, created_by FROM ledger_entries WHERE partner_id = $1 AND seq = 4`,
		e.partner).Scan(&entryReason, &createdBy))
	require.Equal(t, "Trừ phí xử lý", entryReason)
	require.Equal(t, e.admin, createdBy)

	// Without booking_id.
	status, raw = e.asAdmin(t, "POST", path, `{"amount":5000,"reason":"Bù"}`)
	require.Equal(t, http.StatusCreated, status, string(raw))
	require.JSONEq(t, `{"seq":5,"available_after":835000}`, string(raw))
}

func TestHTTP_AdminAdjustment_UnknownPartner404(t *testing.T) {
	e := newAdminEnv(t)
	status, raw := e.asAdmin(t, "POST", "/v1/admin/partners/"+uuid.NewString()+"/adjustments", `{"amount":1000,"reason":"x"}`)
	require.Equal(t, http.StatusNotFound, status, string(raw))
	require.Equal(t, "partner_not_found", errCode(t, raw))

	status, raw = e.asAdmin(t, "POST", "/v1/admin/partners/not-a-uuid/adjustments", `{"amount":1000,"reason":"x"}`)
	require.Equal(t, http.StatusNotFound, status, string(raw))

	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM admin_audit_log`).Scan(&n))
	require.Zero(t, n)
}

func TestHTTP_AdminAdjustment_BadBody422(t *testing.T) {
	e := newAdminEnv(t)
	_, p2 := e.newPartner(t, "beta")
	other := e.newBookingOf(t, p2, e.newListing(t, p2), 500_000, 50_000)
	path := "/v1/admin/partners/" + e.partner.String() + "/adjustments"

	for _, tc := range []struct{ body, code string }{
		{`{"amount":0,"reason":"x"}`, "invalid_adjustment_amount"},
		{`{"amount":1000000000001,"reason":"x"}`, "invalid_adjustment_amount"},
		{`{"amount":1000}`, "invalid_adjustment_reason"},
		{`{"amount":1000,"reason":"   "}`, "invalid_adjustment_reason"},
		{`{"amount":1000,"reason":"` + strings.Repeat("a", 501) + `"}`, "invalid_adjustment_reason"},
		{`{"amount":1000,"reason":"x","booking_id":"` + other.BookingID.String() + `"}`, "booking_not_of_partner"},
		{`{"amount":1000,"reason":"x","booking_id":"` + uuid.NewString() + `"}`, "booking_not_of_partner"},
	} {
		status, raw := e.asAdmin(t, "POST", path, tc.body)
		require.Equal(t, http.StatusUnprocessableEntity, status, tc.body+" -> "+string(raw))
		require.Equal(t, tc.code, errCode(t, raw), tc.body)
	}

	// Unknown field and malformed JSON are 400.
	for _, body := range []string{`{"amount":1000,"reason":"x","bogus":1}`, `{"amount":"1000","reason":"x"}`, `nope`} {
		status, raw := e.asAdmin(t, "POST", path, body)
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Equal(t, "invalid_request", errCode(t, raw))
	}

	e.requireBalance(t, e.partner, 0, 0)
	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM admin_audit_log`).Scan(&n))
	require.Zero(t, n)
}

type ledgerPage struct {
	Balance struct {
		Pending   int64 `json:"pending"`
		Available int64 `json:"available"`
	} `json:"balance"`
	Entries []struct {
		Seq            int64      `json:"seq"`
		Kind           string     `json:"kind"`
		Bucket         string     `json:"bucket"`
		Amount         int64      `json:"amount"`
		AvailableAfter int64      `json:"available_after"`
		BookingID      *uuid.UUID `json:"booking_id"`
		PayoutID       *uuid.UUID `json:"payout_id"`
		Reason         *string    `json:"reason"`
		CreatedAt      time.Time  `json:"created_at"`
	} `json:"entries"`
	NextAfterSeq *int64 `json:"next_after_seq"`
}

func TestHTTP_AdminLedgerPage(t *testing.T) {
	e := newAdminEnv(t)
	b := e.withAvailable900k(t)                                                                                      // seq 1..3
	_, err := e.adjust(ledger.Adjustment{PartnerID: e.partner, Amount: -20_000, Reason: "Phạt", CreatedBy: e.admin}) // seq 4
	require.NoError(t, err)
	base := "/v1/admin/partners/" + e.partner.String() + "/ledger"

	get := func(q string) ledgerPage {
		t.Helper()
		status, raw := e.asAdmin(t, "GET", base+q, "")
		require.Equal(t, http.StatusOK, status, string(raw))
		var p ledgerPage
		require.NoError(t, json.Unmarshal(raw, &p))
		return p
	}

	p := get("")
	require.Equal(t, int64(0), p.Balance.Pending)
	require.Equal(t, int64(880_000), p.Balance.Available)
	require.Len(t, p.Entries, 4)
	require.Nil(t, p.NextAfterSeq)
	require.Equal(t, "booking_earning", p.Entries[0].Kind)
	require.Equal(t, "pending", p.Entries[0].Bucket)
	require.Equal(t, &b.BookingID, p.Entries[0].BookingID)
	require.Equal(t, "available", p.Entries[3].Bucket)
	require.Equal(t, "adjustment", p.Entries[3].Kind)
	require.NotNil(t, p.Entries[3].Reason)

	// Paging by seq.
	p = get("?limit=2")
	require.Len(t, p.Entries, 2)
	require.Equal(t, []int64{1, 2}, []int64{p.Entries[0].Seq, p.Entries[1].Seq})
	require.NotNil(t, p.NextAfterSeq)
	require.Equal(t, int64(2), *p.NextAfterSeq)
	p = get("?limit=2&after_seq=2")
	require.Equal(t, []int64{3, 4}, []int64{p.Entries[0].Seq, p.Entries[1].Seq})
	require.Nil(t, p.NextAfterSeq)
	p = get("?after_seq=4")
	require.Empty(t, p.Entries)
	require.NotNil(t, p.Entries) // [] not null
	p = get("?limit=1000")
	require.Len(t, p.Entries, 4)

	// A partner without any entry.
	_, p2 := e.newPartner(t, "beta")
	status, raw := e.asAdmin(t, "GET", "/v1/admin/partners/"+p2.String()+"/ledger", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.JSONEq(t, `{"balance":{"pending":0,"available":0},"entries":[],"next_after_seq":null}`, string(raw))

	// Unknown / malformed partner: 404. Bad query: 400.
	status, raw = e.asAdmin(t, "GET", "/v1/admin/partners/"+uuid.NewString()+"/ledger", "")
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "partner_not_found", errCode(t, raw))
	status, _ = e.asAdmin(t, "GET", "/v1/admin/partners/xyz/ledger", "")
	require.Equal(t, http.StatusNotFound, status)
	for _, q := range []string{"?limit=0", "?limit=x", "?after_seq=-1", "?after_seq=x"} {
		status, raw = e.asAdmin(t, "GET", base+q, "")
		require.Equal(t, http.StatusBadRequest, status, q)
		require.Equal(t, "invalid_request", errCode(t, raw))
	}
}

func TestHTTP_AdminLedger_PartnerForbidden(t *testing.T) {
	e := newAdminEnv(t)
	base := "/v1/admin/partners/" + e.partner.String()
	status, _ := e.do(t, e.owner, auth.RolePartner, "GET", base+"/ledger", "")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = e.do(t, e.owner, auth.RolePartner, "POST", base+"/adjustments", `{"amount":1000,"reason":"x"}`)
	require.Equal(t, http.StatusForbidden, status)
	e.requireBalance(t, e.partner, 0, 0)
}
