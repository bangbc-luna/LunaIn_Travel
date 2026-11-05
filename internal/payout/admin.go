package payout

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/payout/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
)

const (
	maxTransferReferenceRunes = 100
	maxRejectReasonRunes      = 500

	proofPurpose    = "payout_proof"
	auditTargetKind = "payout"
)

// API errors of the Super Admin payout use cases.
var (
	ErrInvalidState = httpx.NewError(http.StatusConflict, "invalid_payout_state",
		"Trạng thái yêu cầu rút tiền không cho phép thao tác này")
	ErrTransferReferenceInvalid = httpx.NewError(http.StatusUnprocessableEntity, "invalid_transfer_reference",
		"Vui lòng nhập mã giao dịch chuyển khoản (tối đa 100 ký tự)")
	ErrProofRequired = httpx.NewError(http.StatusUnprocessableEntity, "proof_file_required",
		"Vui lòng đính kèm chứng từ chuyển khoản")
	ErrRejectReasonInvalid = httpx.NewError(http.StatusUnprocessableEntity, "invalid_reject_reason",
		"Vui lòng nhập lý do từ chối (tối đa 500 ký tự)")
)

// AdminFilter pages the Super Admin payout list (newest first). Status and
// PartnerID are optional.
type AdminFilter struct {
	Status    *string
	PartnerID *uuid.UUID
	Limit     int
	Cursor    *uuid.UUID
}

// AdminPayoutView is a payout as the Super Admin sees it: with its
// statement, the partner's current balances, the partner's current bank
// account and whether it differs from the payout's snapshot (the payout is
// still paid to the snapshot), and the transfer proof once paid.
type AdminPayoutView struct {
	PayoutView
	Balance            ledger.Balance       `json:"balance"`
	CurrentBank        *partner.BankAccount `json:"current_bank"`
	CurrentBankDiffers bool                 `json:"current_bank_differs"`
	ProofFileID        *uuid.UUID           `json:"proof_file_id"`
}

// MarkPaidInput is what the Super Admin records after transferring a payout
// by hand: the bank transfer reference and an uploaded payout_proof file.
type MarkPaidInput struct {
	TransferReference string
	ProofFileID       uuid.UUID
}

// validText trims s and checks it has 1..max runes of valid UTF-8 without
// NUL (Postgres text cannot store it).
func validText(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	return s, n >= 1 && n <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// AdminList pages every partner's payouts, newest first. An unknown status
// is httpx.ErrInvalidRequest.
func (s *Service) AdminList(ctx context.Context, f AdminFilter) ([]Payout, *uuid.UUID, error) {
	if f.Status != nil && !validStatus(*f.Status) {
		return nil, nil, httpx.ErrInvalidRequest
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	limit = min(limit, maxPageSize)
	rows, err := store.New(s.pool).ListAdminPayouts(ctx, store.ListAdminPayoutsParams{
		Status: f.Status, PartnerID: f.PartnerID, Cursor: f.Cursor, Lim: int32(limit + 1),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list payouts: %w", err)
	}
	var next *uuid.UUID
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1].ID
		next = &last
	}
	out := make([]Payout, len(rows))
	for i, r := range rows {
		out[i] = fromRow(r)
	}
	return out, next, nil
}

// AdminGet returns any payout with its statement, the partner's balances
// and current bank account. CurrentBankDiffers is true when the current
// account's BIN or number is not the snapshot's (or no account is current).
func (s *Service) AdminGet(ctx context.Context, id uuid.UUID) (AdminPayoutView, error) {
	var (
		row  store.Payout
		bank *partner.BankAccount
	)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AdminPayoutView{}, fmt.Errorf("begin admin get tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err = store.New(tx).GetPayout(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminPayoutView{}, ErrPayoutNotFound
	}
	if err != nil {
		return AdminPayoutView{}, fmt.Errorf("get payout: %w", err)
	}
	if bank, err = s.partners.CurrentBankAccount(ctx, tx, row.PartnerID); err != nil {
		return AdminPayoutView{}, err
	}
	_ = tx.Rollback(ctx)

	p := fromRow(row)
	pv, err := s.view(ctx, p)
	if err != nil {
		return AdminPayoutView{}, err
	}
	bal, err := s.ledger.Balance(ctx, p.PartnerID)
	if err != nil {
		return AdminPayoutView{}, err
	}
	return AdminPayoutView{
		PayoutView:         pv,
		Balance:            bal,
		CurrentBank:        bank,
		CurrentBankDiffers: bank == nil || bank.BankBIN != p.Bank.BankBIN || bank.AccountNumber != p.Bank.AccountNumber,
		ProofFileID:        row.ProofFileID,
	}, nil
}

// lockPayout takes the payouts row FOR UPDATE: the first lock of every admin
// transition, before partner_balances (DB §1.1). Unknown is
// ErrPayoutNotFound.
func lockPayout(ctx context.Context, q *store.Queries, id uuid.UUID) (store.Payout, error) {
	row, err := q.LockPayout(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrPayoutNotFound
	}
	if err != nil {
		return row, fmt.Errorf("lock payout: %w", err)
	}
	return row, nil
}

// Approve moves a requested payout to approved (anything else is
// ErrInvalidState), audits payout.approve and emails the partner owner, in
// one transaction. A suspended partner's payout can still be approved.
func (s *Service) Approve(ctx context.Context, actor, id uuid.UUID) (Payout, error) {
	now := s.clock.Now()
	var out Payout
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := lockPayout(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Status != StatusRequested {
			return ErrInvalidState
		}
		row, err := q.ApprovePayout(ctx, store.ApprovePayoutParams{Actor: actor, Now: now, ID: id})
		if err != nil {
			return fmt.Errorf("approve payout: %w", err)
		}
		out = fromRow(row)
		if err := audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor, Action: "payout.approve", TargetKind: auditTargetKind, TargetID: &id,
			Before: map[string]any{"status": cur.Status},
			After:  map[string]any{"status": out.Status, "partner_id": out.PartnerID, "amount": out.Amount},
		}); err != nil {
			return err
		}
		return s.emailOwner(ctx, tx, out.PartnerID, "Yêu cầu rút tiền đã được duyệt",
			fmt.Sprintf("Yêu cầu rút %s (mã %s) đã được duyệt.\n"+
				"LunaIn sẽ chuyển khoản vào tài khoản %s.\n",
				formatVND(out.Amount), out.ID, bankLine(out.Bank)))
	})
	return out, err
}

// MarkPaid records that an approved payout was transferred by hand: the
// trimmed transfer reference (1..100 runes, else
// ErrTransferReferenceInvalid) and a payout_proof file uploaded by the actor
// (missing is ErrProofRequired, another's or another purpose is
// files.ErrInvalidFile). A payout not approved is ErrInvalidState. It posts
// no ledger entry: the money left available at hold time. Audited as
// payout.mark_paid; the owner's email names the snapshot account.
func (s *Service) MarkPaid(ctx context.Context, actor, id uuid.UUID, in MarkPaidInput) (Payout, error) {
	ref, ok := validText(in.TransferReference, maxTransferReferenceRunes)
	if !ok {
		return Payout{}, ErrTransferReferenceInvalid
	}
	if in.ProofFileID == uuid.Nil {
		return Payout{}, ErrProofRequired
	}
	now := s.clock.Now()
	var out Payout
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.files.Owned(ctx, tx, in.ProofFileID, actor, proofPurpose); err != nil {
			return err
		}
		q := store.New(tx)
		cur, err := lockPayout(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Status != StatusApproved {
			return ErrInvalidState
		}
		row, err := q.MarkPayoutPaid(ctx, store.MarkPayoutPaidParams{
			Actor: actor, Now: now, TransferReference: ref, ProofFileID: in.ProofFileID, ID: id,
		})
		if err != nil {
			return fmt.Errorf("mark payout paid: %w", err)
		}
		out = fromRow(row)
		if err := audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor, Action: "payout.mark_paid", TargetKind: auditTargetKind, TargetID: &id,
			Before: map[string]any{"status": cur.Status},
			After: map[string]any{
				"status": out.Status, "partner_id": out.PartnerID, "amount": out.Amount,
				"transfer_reference": ref, "proof_file_id": in.ProofFileID, "bank": out.Bank,
			},
		}); err != nil {
			return err
		}
		return s.emailOwner(ctx, tx, out.PartnerID, "LunaIn đã chuyển tiền rút cho bạn",
			fmt.Sprintf("LunaIn đã chuyển %s cho yêu cầu rút tiền mã %s.\n"+
				"Tài khoản nhận: %s.\n"+
				"Mã giao dịch: %s.\n",
				formatVND(out.Amount), out.ID, bankLine(out.Bank), ref))
	})
	return out, err
}

// Reject closes a requested or approved payout as rejected (anything else is
// ErrInvalidState) with the trimmed reason (1..500 runes, else
// ErrRejectReasonInvalid) and gives the amount back to available
// (payout_reversal), whatever available is now. The payouts row is locked
// before ledger.PostPayoutReversal takes partner_balances (DB §1.1). Audited
// as payout.reject; the owner is emailed the reason.
func (s *Service) Reject(ctx context.Context, actor, id uuid.UUID, reason string) (Payout, error) {
	reason, ok := validText(reason, maxRejectReasonRunes)
	if !ok {
		return Payout{}, ErrRejectReasonInvalid
	}
	now := s.clock.Now()
	var out Payout
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := lockPayout(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Status != StatusRequested && cur.Status != StatusApproved {
			return ErrInvalidState
		}
		row, err := q.RejectPayout(ctx, store.RejectPayoutParams{Actor: actor, Now: now, Reason: reason, ID: id})
		if err != nil {
			return fmt.Errorf("reject payout: %w", err)
		}
		out = fromRow(row)
		rev, err := s.ledger.PostPayoutReversal(ctx, tx, out.PartnerID, out.ID, out.Amount)
		if err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, now, audit.Entry{
			ActorUserID: actor, Action: "payout.reject", TargetKind: auditTargetKind, TargetID: &id,
			Before: map[string]any{"status": cur.Status},
			After: map[string]any{
				"status": out.Status, "partner_id": out.PartnerID, "amount": out.Amount,
				"reversal_seq": rev.Seq, "available_after": rev.AvailableAfter,
			},
			Reason: reason,
		}); err != nil {
			return err
		}
		return s.emailOwner(ctx, tx, out.PartnerID, "Yêu cầu rút tiền bị từ chối",
			fmt.Sprintf("Yêu cầu rút %s (mã %s) đã bị từ chối.\n"+
				"Lý do: %s\n"+
				"Số tiền này đã được hoàn lại vào số dư khả dụng của bạn.\n",
				formatVND(out.Amount), out.ID, reason))
	})
	return out, err
}

// bankLine renders a bank snapshot for an email.
func bankLine(b BankSnapshot) string {
	return fmt.Sprintf("%s - số tài khoản %s - chủ tài khoản %s", b.BankName, b.AccountNumber, b.AccountHolder)
}

// emailOwner queues an email to the partner's owner in tx (sent only if tx
// commits). Without a job client (some tests) it is only logged.
func (s *Service) emailOwner(ctx context.Context, tx pgx.Tx, partnerID uuid.UUID, subject, text string) error {
	to, err := s.partners.OwnerEmail(ctx, tx, partnerID)
	if err != nil {
		return err
	}
	m := mailer.Message{To: to, Subject: subject, Text: text}
	if s.jobs == nil {
		s.log.Info("payout: email not queued (no job client)", "to", m.To, "subject", m.Subject)
		return nil
	}
	if err := mailer.EnqueueTx(ctx, tx, s.jobs, m); err != nil {
		return fmt.Errorf("enqueue email: %w", err)
	}
	return nil
}
