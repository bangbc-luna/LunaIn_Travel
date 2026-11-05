package partner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/partner/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/validate"
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

// NormalizeAccountHolder renders a name the way it is printed on a bank card
// (see validate.NormalizeAccountHolder).
func NormalizeAccountHolder(s string) string { return validate.NormalizeAccountHolder(s) }

func (in BankAccountInput) normalize() (BankAccountInput, error) {
	acc, msg := validate.NormalizeBankAccount(in.BankBIN, in.AccountNumber, in.AccountHolder)
	out := BankAccountInput{BankBIN: acc.BIN, AccountNumber: acc.Number, AccountHolder: acc.Holder}
	if msg != "" {
		return out, invalidBankAccount(msg)
	}
	var err error
	if out.BankName, err = normalizeText(in.BankName, "bank_name", true, maxTextLen); err != nil {
		return out, invalidBankAccount("bank_name không được để trống (tối đa 200 ký tự)")
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
// row, DB §1.1), clears the old current account, inserts the new one (with
// created_by = actorUserID) and records the change in the audit log
// (partner.bank_account.set, before = the old current account or null).
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
		before, err := s.CurrentBankAccount(ctx, tx, partnerID)
		if err != nil {
			return err
		}
		if err := q.ClearCurrentBankAccount(ctx, store.ClearCurrentBankAccountParams{PartnerID: partnerID, Now: now}); err != nil {
			return fmt.Errorf("clear current bank account: %w", err)
		}
		row, err := q.InsertBankAccount(ctx, store.InsertBankAccountParams{
			ID: id, PartnerID: partnerID, BankBin: in.BankBIN, BankName: in.BankName,
			AccountNumber: in.AccountNumber, AccountHolder: in.AccountHolder,
			CreatedBy: actorUserID, Now: now,
		})
		if err != nil {
			return fmt.Errorf("insert bank account: %w", err)
		}
		out = bankFromRow(row)
		entry := audit.Entry{
			ActorUserID: actorUserID, Action: "partner.bank_account.set",
			TargetKind: auditTargetKind, TargetID: &partnerID, After: out,
		}
		if before != nil {
			// A nil *BankAccount inside an interface would marshal as JSON
			// null rather than leave the column NULL.
			entry.Before = *before
		}
		return audit.Record(ctx, tx, now, entry)
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
