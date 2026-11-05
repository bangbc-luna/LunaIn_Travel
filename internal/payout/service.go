// Package payout owns partner payouts: a partner asks to withdraw part of its
// available balance (held at once in the ledger, with a statement), a Super
// Admin approves, pays by hand or rejects it (spec §9).
package payout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/payout/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

// Payout statuses: requested -> approved -> paid; requested|approved ->
// rejected. requested and approved are "open" (at most one per partner).
const (
	StatusRequested = "requested"
	StatusApproved  = "approved"
	StatusPaid      = "paid"
	StatusRejected  = "rejected"

	defaultPageSize = 20
	maxPageSize     = 100

	oneOpenIndex = "payouts_one_open_idx"
)

// API errors of the payout use cases.
var (
	ErrInvalidAmount = httpx.NewError(http.StatusUnprocessableEntity, "invalid_amount",
		"Số tiền rút phải lớn hơn 0")
	ErrBelowMinimum = httpx.NewError(http.StatusUnprocessableEntity, "payout_below_minimum",
		"Số tiền rút thấp hơn mức tối thiểu")
	ErrInsufficientAvailable = httpx.NewError(http.StatusUnprocessableEntity, "insufficient_available",
		"Số dư khả dụng không đủ để rút số tiền này")
	ErrBankAccountRequired = httpx.NewError(http.StatusUnprocessableEntity, "bank_account_required",
		"Vui lòng khai báo tài khoản ngân hàng nhận tiền trước khi rút")
	ErrAlreadyOpen = httpx.NewError(http.StatusConflict, "payout_already_open",
		"Bạn đang có một yêu cầu rút tiền chưa xử lý xong")
	ErrPartnerSuspended = httpx.NewError(http.StatusConflict, "partner_suspended",
		"Đối tác đang bị đình chỉ, không thể rút tiền")
	ErrPayoutNotFound = httpx.NewError(http.StatusNotFound, "payout_not_found",
		"Không tìm thấy yêu cầu rút tiền")
)

// messageError carries a specific Vietnamese message while still matching
// its sentinel with errors.Is.
type messageError struct{ app, sentinel *httpx.AppError }

func (e *messageError) Error() string        { return e.app.Error() }
func (e *messageError) Unwrap() error        { return e.app }
func (e *messageError) Is(target error) bool { return target == e.sentinel }

func belowMinimum(min int64) error {
	return &messageError{
		app: httpx.NewError(ErrBelowMinimum.Status, ErrBelowMinimum.Code,
			"Số tiền rút tối thiểu là "+formatVND(min)),
		sentinel: ErrBelowMinimum,
	}
}

// formatVND writes 100000 as "100.000 đ".
func formatVND(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(ch)
	}
	out := b.String() + " đ"
	if neg {
		out = "-" + out
	}
	return out
}

// Deps are the collaborators of Service.
type Deps struct {
	Pool     *pgxpool.Pool
	Clock    clock.Clock
	Ledger   *ledger.Service
	Partners *partner.Service
	Settings *settings.Service
	Files    *files.Service
	// Jobs queues partner emails (outbox). May be nil in tests that send no
	// email.
	Jobs *river.Client[pgx.Tx]
	Log  *slog.Logger
}

// Service implements the payout use cases.
type Service struct {
	pool     *pgxpool.Pool
	clock    clock.Clock
	ledger   *ledger.Service
	partners *partner.Service
	settings *settings.Service
	files    *files.Service
	jobs     *river.Client[pgx.Tx]
	log      *slog.Logger

	// beforeCommit, when set (tests only), runs at the end of Request's
	// transaction: after payout_hold and the statement, before commit.
	beforeCommit func()
}

// NewService builds a Service.
func NewService(d Deps) *Service {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		pool: d.Pool, clock: d.Clock, ledger: d.Ledger, partners: d.Partners, settings: d.Settings,
		files: d.Files, jobs: d.Jobs, log: log,
	}
}

// BankSnapshot is the partner's bank account as it was when the payout was
// requested; the payout is paid to it whatever the partner sets afterwards.
type BankSnapshot struct {
	BankBIN       string `json:"bank_bin"`
	BankName      string `json:"bank_name"`
	AccountNumber string `json:"account_number"`
	AccountHolder string `json:"account_holder"`
}

// Payout is one withdrawal of a partner's available balance. Its statement
// covers ledger seq StatementSeqFrom..StatementSeqTo (the payout's own
// payout_hold): OpeningAvailable + Σ statement lines = ClosingAvailable.
type Payout struct {
	ID                uuid.UUID    `json:"id"`
	PartnerID         uuid.UUID    `json:"partner_id"`
	Amount            int64        `json:"amount"`
	Status            string       `json:"status"`
	Bank              BankSnapshot `json:"bank"`
	StatementSeqFrom  int64        `json:"statement_seq_from"`
	StatementSeqTo    int64        `json:"statement_seq_to"`
	OpeningAvailable  int64        `json:"opening_available"`
	ClosingAvailable  int64        `json:"closing_available"`
	RequestedAt       time.Time    `json:"requested_at"`
	ApprovedAt        *time.Time   `json:"approved_at"`
	PaidAt            *time.Time   `json:"paid_at"`
	RejectedAt        *time.Time   `json:"rejected_at"`
	TransferReference *string      `json:"transfer_reference"`
	RejectReason      *string      `json:"reject_reason"`
}

// PayoutView is a payout with its statement lines.
type PayoutView struct {
	Payout
	Statement []ledger.StatementLine `json:"statement"`
}

// ListFilter pages a payout list (newest first). Status is optional.
type ListFilter struct {
	Status *string
	Limit  int
	Cursor *uuid.UUID
}

// Preview is what a partner sees before requesting a payout: the provisional
// statement since the last payout's statement, up to the latest entry.
type Preview struct {
	Available        int64                  `json:"available"`
	MinPayoutAmount  int64                  `json:"min_payout_amount"`
	OpenPayoutID     *uuid.UUID             `json:"open_payout_id"`
	CanRequest       bool                   `json:"can_request"`
	StatementFromSeq int64                  `json:"statement_from_seq"`
	OpeningAvailable int64                  `json:"opening_available"`
	Statement        []ledger.StatementLine `json:"statement"`
}

func validStatus(s string) bool {
	switch s {
	case StatusRequested, StatusApproved, StatusPaid, StatusRejected:
		return true
	}
	return false
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func fromRow(r store.Payout) Payout {
	return Payout{
		ID: r.ID, PartnerID: r.PartnerID, Amount: r.Amount, Status: r.Status,
		Bank: BankSnapshot{
			BankBIN: r.BankBin, BankName: r.BankName, AccountNumber: r.AccountNumber, AccountHolder: r.AccountHolder,
		},
		StatementSeqFrom: deref(r.StatementSeqFrom), StatementSeqTo: deref(r.StatementSeqTo),
		OpeningAvailable: deref(r.OpeningAvailable), ClosingAvailable: deref(r.ClosingAvailable),
		RequestedAt: r.RequestedAt, ApprovedAt: r.ApprovedAt, PaidAt: r.PaidAt, RejectedAt: r.RejectedAt,
		TransferReference: r.TransferReference, RejectReason: r.RejectReason,
	}
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// Request creates a payout of amount for the partner and holds it in the
// ledger, in one transaction taking locks in DB §1.1 order: the partners row
// FOR SHARE (a suspended partner is ErrPartnerSuspended), the settings
// (amount below min_payout_amount is ErrBelowMinimum), the current bank
// account (none is ErrBankAccountRequired, else it is snapshotted), the new
// payouts row (a second open payout is ErrAlreadyOpen), then partner_balances
// through ledger.PostPayoutHold (available <= 0 or < amount is
// ErrInsufficientAvailable). The statement runs from the end of the previous
// payout's statement (whatever its status) + 1 to the hold's seq.
func (s *Service) Request(ctx context.Context, partnerID, actorUserID uuid.UUID, amount int64) (PayoutView, error) {
	if amount <= 0 {
		return PayoutView{}, ErrInvalidAmount
	}
	id, err := uuid.NewV7()
	if err != nil {
		return PayoutView{}, fmt.Errorf("payout id: %w", err)
	}
	now := s.clock.Now()
	var p Payout
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		st, err := s.partners.Status(ctx, tx, partnerID, true)
		if err != nil {
			return err
		}
		if st != partner.StatusActive {
			return ErrPartnerSuspended
		}
		set, err := s.settings.GetTx(ctx, tx)
		if err != nil {
			return err
		}
		if amount < set.MinPayoutAmount {
			return belowMinimum(set.MinPayoutAmount)
		}
		bank, err := s.partners.CurrentBankAccount(ctx, tx, partnerID)
		if err != nil {
			return err
		}
		if bank == nil {
			return ErrBankAccountRequired
		}

		q := store.New(tx)
		// The partial unique index makes a concurrent request of the same
		// partner wait here until this tx ends, then fail with 23505.
		if err := q.InsertPayout(ctx, store.InsertPayoutParams{
			ID: id, PartnerID: partnerID, Amount: amount,
			BankBin: bank.BankBIN, BankName: bank.BankName,
			AccountNumber: bank.AccountNumber, AccountHolder: bank.AccountHolder,
			BankAccountID: bank.ID, RequestedBy: actorUserID, Now: now,
		}); err != nil {
			if isUniqueViolation(err, oneOpenIndex) {
				return ErrAlreadyOpen
			}
			return fmt.Errorf("insert payout: %w", err)
		}
		// Every other payout of the partner is closed (the index above), so
		// their statement ends are committed and final.
		lastTo, err := q.LastStatementSeqTo(ctx, store.LastStatementSeqToParams{PartnerID: partnerID, ExcludeID: id})
		if err != nil {
			return fmt.Errorf("last statement seq: %w", err)
		}
		seqFrom := lastTo + 1

		hold, err := s.ledger.PostPayoutHold(ctx, tx, partnerID, id, amount)
		switch {
		case errors.Is(err, ledger.ErrNothingAvailable), errors.Is(err, ledger.ErrExceedsAvailable):
			return ErrInsufficientAvailable
		case errors.Is(err, ledger.ErrInvalidAmount):
			return ErrInvalidAmount
		case err != nil:
			return err
		}
		opening, err := s.ledger.AvailableAfter(ctx, tx, partnerID, seqFrom-1)
		if err != nil {
			return err
		}
		if err := q.SetPayoutStatement(ctx, store.SetPayoutStatementParams{
			SeqFrom: seqFrom, SeqTo: hold.Seq, Opening: opening, Closing: hold.AvailableAfter, Now: now, ID: id,
		}); err != nil {
			return fmt.Errorf("set payout statement: %w", err)
		}
		row, err := q.GetPayout(ctx, id)
		if err != nil {
			return fmt.Errorf("get payout: %w", err)
		}
		p = fromRow(row)
		if s.beforeCommit != nil {
			s.beforeCommit()
		}
		return nil
	})
	if err != nil {
		return PayoutView{}, err
	}
	return s.view(ctx, p)
}

// view reads p's statement lines (committed entries, so after the tx).
func (s *Service) view(ctx context.Context, p Payout) (PayoutView, error) {
	lines, err := s.ledger.Statement(ctx, p.PartnerID, p.StatementSeqFrom, p.StatementSeqTo)
	if err != nil {
		return PayoutView{}, err
	}
	return PayoutView{Payout: p, Statement: lines}, nil
}

// ListMine pages the partner's payouts, newest first. An unknown status is
// httpx.ErrInvalidRequest.
func (s *Service) ListMine(ctx context.Context, partnerID uuid.UUID, f ListFilter) ([]Payout, *uuid.UUID, error) {
	if f.Status != nil && !validStatus(*f.Status) {
		return nil, nil, httpx.ErrInvalidRequest
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	limit = min(limit, maxPageSize)
	rows, err := store.New(s.pool).ListPartnerPayouts(ctx, store.ListPartnerPayoutsParams{
		PartnerID: partnerID, Status: f.Status, Cursor: f.Cursor, Lim: int32(limit + 1),
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

// GetMine returns one of the partner's payouts with its statement; another
// partner's payout is ErrPayoutNotFound.
func (s *Service) GetMine(ctx context.Context, partnerID, id uuid.UUID) (PayoutView, error) {
	row, err := store.New(s.pool).GetPartnerPayout(ctx, store.GetPartnerPayoutParams{ID: id, PartnerID: partnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return PayoutView{}, ErrPayoutNotFound
	}
	if err != nil {
		return PayoutView{}, fmt.Errorf("get payout: %w", err)
	}
	return s.view(ctx, fromRow(row))
}

// Preview returns the partner's provisional statement, from the end of the
// last payout's statement + 1 up to the latest ledger entry, and whether a
// payout can be requested now: partner active, a bank account set, no open
// payout and available >= max(min_payout_amount, 1). Available is
// opening + Σ lines, so the preview always balances.
func (s *Service) Preview(ctx context.Context, partnerID uuid.UUID) (Preview, error) {
	var (
		pv      Preview
		active  bool
		hasBank bool
	)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return pv, fmt.Errorf("begin preview tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	st, err := s.partners.Status(ctx, tx, partnerID, false)
	if err != nil {
		return pv, err
	}
	active = st == partner.StatusActive
	set, err := s.settings.GetTx(ctx, tx)
	if err != nil {
		return pv, err
	}
	pv.MinPayoutAmount = set.MinPayoutAmount
	bank, err := s.partners.CurrentBankAccount(ctx, tx, partnerID)
	if err != nil {
		return pv, err
	}
	hasBank = bank != nil
	q := store.New(tx)
	open, err := q.OpenPayoutID(ctx, partnerID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return pv, fmt.Errorf("open payout: %w", err)
	default:
		pv.OpenPayoutID = &open
	}
	lastTo, err := q.LastStatementSeqTo(ctx, store.LastStatementSeqToParams{PartnerID: partnerID, ExcludeID: uuid.Nil})
	if err != nil {
		return pv, fmt.Errorf("last statement seq: %w", err)
	}
	pv.StatementFromSeq = lastTo + 1
	if pv.OpeningAvailable, err = s.ledger.AvailableAfter(ctx, tx, partnerID, lastTo); err != nil {
		return pv, err
	}
	_ = tx.Rollback(ctx)

	lastSeq, err := s.ledger.LastSeq(ctx, partnerID)
	if err != nil {
		return pv, err
	}
	if pv.Statement, err = s.ledger.Statement(ctx, partnerID, pv.StatementFromSeq, lastSeq); err != nil {
		return pv, err
	}
	pv.Available = pv.OpeningAvailable
	for _, l := range pv.Statement {
		pv.Available += l.Amount
	}
	pv.CanRequest = active && hasBank && pv.OpenPayoutID == nil && pv.Available >= max(pv.MinPayoutAmount, 1)
	return pv, nil
}
