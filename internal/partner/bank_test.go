package partner_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

func bankInput(number string) partner.BankAccountInput {
	return partner.BankAccountInput{
		BankBIN: "970436", BankName: "Vietcombank", AccountNumber: number, AccountHolder: "Nguyen Van An",
	}
}

func (e *env) current(t *testing.T, partnerID uuid.UUID) *partner.BankAccount {
	t.Helper()
	var cur *partner.BankAccount
	require.NoError(t, db.InTx(context.Background(), e.Pool, func(tx pgx.Tx) error {
		var err error
		cur, err = e.Svc.CurrentBankAccount(context.Background(), tx, partnerID)
		return err
	}))
	return cur
}

func TestSetBankAccount_RotatesCurrent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(t, input("alpha"))
	b := e.create(t, input("beta"))
	require.Nil(t, e.current(t, a.ID))

	first, err := e.Svc.SetBankAccount(ctx, a.ID, a.OwnerUserID, bankInput("0011001234567"))
	require.NoError(t, err)
	require.True(t, first.IsCurrent)
	second, err := e.Svc.SetBankAccount(ctx, a.ID, a.OwnerUserID, bankInput("0011009999999"))
	require.NoError(t, err)
	require.True(t, second.IsCurrent)
	_, err = e.Svc.SetBankAccount(ctx, b.ID, b.OwnerUserID, bankInput("1234567890"))
	require.NoError(t, err)

	list, err := e.Svc.BankAccounts(ctx, a.ID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, second.ID, list[0].ID)
	require.True(t, list[0].IsCurrent)
	require.Equal(t, first.ID, list[1].ID)
	require.False(t, list[1].IsCurrent)
	require.True(t, list[0].CreatedAt.Equal(testNow))

	var n int
	require.NoError(t, e.Pool.QueryRow(ctx,
		`SELECT count(*) FROM partner_bank_accounts WHERE partner_id = $1 AND is_current`, a.ID).Scan(&n))
	require.Equal(t, 1, n)

	cur := e.current(t, a.ID)
	require.NotNil(t, cur)
	require.Equal(t, second, *cur)

	// B's account is untouched by A's rotation.
	listB, err := e.Svc.BankAccounts(ctx, b.ID)
	require.NoError(t, err)
	require.Len(t, listB, 1)
	require.True(t, listB[0].IsCurrent)
}

func TestSetBankAccount_Validation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(t, input("alpha"))
	cases := map[string]partner.BankAccountInput{
		"bin 5 digits":       {BankBIN: "97043", BankName: "VCB", AccountNumber: "0011001234567", AccountHolder: "A"},
		"bin letters":        {BankBIN: "97043a", BankName: "VCB", AccountNumber: "0011001234567", AccountHolder: "A"},
		"number has letters": {BankBIN: "970436", BankName: "VCB", AccountNumber: "00110A1234567", AccountHolder: "A"},
		"number too short":   {BankBIN: "970436", BankName: "VCB", AccountNumber: "12345", AccountHolder: "A"},
		"number too long":    {BankBIN: "970436", BankName: "VCB", AccountNumber: "12345678901234567890", AccountHolder: "A"},
		"empty holder":       {BankBIN: "970436", BankName: "VCB", AccountNumber: "0011001234567", AccountHolder: "  "},
		"empty bank name":    {BankBIN: "970436", BankName: " ", AccountNumber: "0011001234567", AccountHolder: "A"},
	}
	for name, in := range cases {
		_, err := e.Svc.SetBankAccount(ctx, a.ID, a.OwnerUserID, in)
		status, code := appCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, status, name)
		require.Equal(t, "invalid_bank_account", code, name)
	}
	require.Equal(t, 0, e.count(t, "partner_bank_accounts"))
}

func TestSetBankAccount_HolderUppercasedNoDiacritics(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(t, input("alpha"))
	for in, want := range map[string]string{
		"Nguyễn Văn An":          "NGUYEN VAN AN",
		"  đặng   thị   ĐÀO  ":   "DANG THI DAO",
		"Công ty TNHH Ánh Dương": "CONG TY TNHH ANH DUONG",
	} {
		ba, err := e.Svc.SetBankAccount(ctx, a.ID, a.OwnerUserID, partner.BankAccountInput{
			BankBIN: "970436", BankName: "Vietcombank", AccountNumber: "0011001234567", AccountHolder: in,
		})
		require.NoError(t, err)
		require.Equal(t, want, ba.AccountHolder, in)
	}
}

func TestHTTP_BankAccounts(t *testing.T) {
	e := newEnv(t)
	srv := e.server(t)
	a := e.create(t, input("alpha"))
	b := e.create(t, input("beta"))
	_, err := e.Svc.SetBankAccount(context.Background(), b.ID, b.OwnerUserID, bankInput("1234567890"))
	require.NoError(t, err)
	tokA := e.token(t, a.OwnerUserID, auth.RolePartner)

	status, body := do(t, srv, "PUT", "/v1/partner/bank-account", tokA,
		`{"bank_bin":"970436","bank_name":"Vietcombank","account_number":"0011001234567","account_holder":"Nguyễn Văn An"}`)
	require.Equal(t, http.StatusOK, status, string(body))
	var ba partner.BankAccount
	require.NoError(t, json.Unmarshal(body, &ba))
	require.Equal(t, "NGUYEN VAN AN", ba.AccountHolder)

	status, body = do(t, srv, "PUT", "/v1/partner/bank-account", tokA,
		`{"bank_bin":"970436","bank_name":"V","account_number":"0011001234567","account_holder":"A","partner_id":"`+b.ID.String()+`"}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "invalid_request", errCode(t, body))

	status, body = do(t, srv, "GET", "/v1/partner/bank-accounts", tokA, "")
	require.Equal(t, http.StatusOK, status)
	var list []partner.BankAccount
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list, 1)
	require.Equal(t, ba.ID, list[0].ID)
}

func TestSetBankAccount_Audited(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(t, input("alpha"))
	first, err := e.Svc.SetBankAccount(ctx, a.ID, a.OwnerUserID, bankInput("0011001234567"))
	require.NoError(t, err)
	second, err := e.Svc.SetBankAccount(ctx, a.ID, a.OwnerUserID, bankInput("0011009999999"))
	require.NoError(t, err)

	type auditRow struct {
		Actor  uuid.UUID
		Kind   string
		Target uuid.UUID
		Before map[string]any
		After  map[string]any
	}
	rows, err := e.Pool.Query(ctx, `SELECT actor_user_id, target_kind, target_id, before, after
		FROM admin_audit_log WHERE action = 'partner.bank_account.set' ORDER BY id`)
	require.NoError(t, err)
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[auditRow])
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, r := range got {
		require.Equal(t, a.OwnerUserID, r.Actor)
		require.Equal(t, "partner", r.Kind)
		require.Equal(t, a.ID, r.Target)
	}
	require.Nil(t, got[0].Before)
	require.Equal(t, first.AccountNumber, got[0].After["account_number"])
	require.Equal(t, first.AccountNumber, got[1].Before["account_number"])
	require.Equal(t, first.ID.String(), got[1].Before["id"])
	require.Equal(t, second.AccountNumber, got[1].After["account_number"])
	require.Equal(t, second.ID.String(), got[1].After["id"])

	for _, id := range []uuid.UUID{first.ID, second.ID} {
		var createdBy *uuid.UUID
		require.NoError(t, e.Pool.QueryRow(ctx,
			`SELECT created_by FROM partner_bank_accounts WHERE id = $1`, id).Scan(&createdBy))
		require.NotNil(t, createdBy)
		require.Equal(t, a.OwnerUserID, *createdBy)
	}
}

func TestSetBankAccount_HolderWithDigits422(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(t, input("alpha"))
	_, err := e.Svc.SetBankAccount(ctx, a.ID, a.OwnerUserID, partner.BankAccountInput{
		BankBIN: "970436", BankName: "Vietcombank", AccountNumber: "0011001234567", AccountHolder: "Nguyen Van 2",
	})
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_bank_account", code)
	require.Equal(t, 0, e.count(t, "partner_bank_accounts"))
	require.Equal(t, 0, e.auditCount(t, "partner.bank_account.set"))
}

func TestOwnerEmail(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(t, input("alpha"))
	var got string
	require.NoError(t, db.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
		var err error
		got, err = e.Svc.OwnerEmail(ctx, tx, a.ID)
		return err
	}))
	require.Equal(t, "owner@alpha.vn", got)
}
