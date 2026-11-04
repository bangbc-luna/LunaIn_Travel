package partner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"

	"github.com/bangbc-luna/lunain-travel/internal/partner/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

var (
	bankBINRe       = regexp.MustCompile(`^[0-9]{6}$`)
	accountNumberRe = regexp.MustCompile(`^[0-9]{6,19}$`)
)

// BankAccount is one payout account of a partner. Exactly one row per
// partner is current; older rows are kept as history.
type BankAccount struct {
	ID            uuid.UUID `json:"id"`
	BankBIN       string    `json:"bank_bin"`
	BankName      string    `json:"bank_name"`
	AccountNumber string    `json:"account_number"`
	AccountHolder string    `json:"account_holder"`
	IsCurrent     bool      `json:"is_current"`
	CreatedAt     time.Time `json:"created_at"`
}

// BankAccountInput is the PUT /v1/partner/bank-account body.
type BankAccountInput struct {
	BankBIN       string `json:"bank_bin"`
	BankName      string `json:"bank_name"`
	AccountNumber string `json:"account_number"`
	AccountHolder string `json:"account_holder"`
}

// NormalizeAccountHolder renders a name the way it is printed on a bank card:
// upper case without Vietnamese diacritics (đ becomes D) and with runs of
// whitespace collapsed to one space.
func NormalizeAccountHolder(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case r == 'đ' || r == 'Đ':
			r = 'D'
		default:
			r = unicode.ToUpper(r)
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func (in BankAccountInput) normalize() (BankAccountInput, error) {
	out := BankAccountInput{
		BankBIN:       strings.TrimSpace(in.BankBIN),
		AccountNumber: strings.TrimSpace(in.AccountNumber),
		AccountHolder: NormalizeAccountHolder(in.AccountHolder),
	}
	if !bankBINRe.MatchString(out.BankBIN) {
		return out, invalidBankAccount("bank_bin phải gồm đúng 6 chữ số")
	}
	if !accountNumberRe.MatchString(out.AccountNumber) {
		return out, invalidBankAccount("account_number phải gồm 6 đến 19 chữ số")
	}
	var err error
	if out.BankName, err = normalizeText(in.BankName, "bank_name", true, maxTextLen); err != nil {
		return out, invalidBankAccount("bank_name không được để trống (tối đa 200 ký tự)")
	}
	if out.AccountHolder == "" || len([]rune(out.AccountHolder)) > maxTextLen {
		return out, invalidBankAccount("account_holder không được để trống (tối đa 200 ký tự)")
	}
	return out, nil
}

func bankFromRow(r store.PartnerBankAccount) BankAccount {
	return BankAccount{
		ID: r.ID, BankBIN: r.BankBin, BankName: r.BankName, AccountNumber: r.AccountNumber,
		AccountHolder: r.AccountHolder, IsCurrent: r.IsCurrent, CreatedAt: r.CreatedAt,
	}
}

// SetBankAccount makes in the partner's current payout account. In one
// transaction it locks the partners row (before any partner_bank_accounts
// row, DB §1.1), clears the old current account and inserts the new one.
func (s *Service) SetBankAccount(ctx context.Context, partnerID, actorUserID uuid.UUID, in BankAccountInput) (BankAccount, error) {
	in, err := in.normalize()
	if err != nil {
		return BankAccount{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return BankAccount{}, fmt.Errorf("bank account id: %w", err)
	}
	now := s.clock.Now()
	var out BankAccount
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := lockPartner(ctx, q, partnerID); err != nil {
			return err
		}
		if err := q.ClearCurrentBankAccount(ctx, store.ClearCurrentBankAccountParams{PartnerID: partnerID, Now: now}); err != nil {
			return fmt.Errorf("clear current bank account: %w", err)
		}
		row, err := q.InsertBankAccount(ctx, store.InsertBankAccountParams{
			ID: id, PartnerID: partnerID, BankBin: in.BankBIN, BankName: in.BankName,
			AccountNumber: in.AccountNumber, AccountHolder: in.AccountHolder, Now: now,
		})
		if err != nil {
			return fmt.Errorf("insert bank account: %w", err)
		}
		out = bankFromRow(row)
		return nil
	})
	if err != nil {
		return BankAccount{}, err
	}
	return out, nil
}

// BankAccounts lists the partner's payout accounts, newest first.
func (s *Service) BankAccounts(ctx context.Context, partnerID uuid.UUID) ([]BankAccount, error) {
	rows, err := store.New(s.pool).ListBankAccounts(ctx, partnerID)
	if err != nil {
		return nil, fmt.Errorf("list bank accounts: %w", err)
	}
	out := make([]BankAccount, len(rows))
	for i, r := range rows {
		out[i] = bankFromRow(r)
	}
	return out, nil
}

// CurrentBankAccount returns, inside tx, the partner's current payout account
// or nil when none was set.
func (s *Service) CurrentBankAccount(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID) (*BankAccount, error) {
	row, err := store.New(tx).CurrentBankAccount(ctx, partnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("current bank account: %w", err)
	}
	ba := bankFromRow(row)
	return &ba, nil
}
