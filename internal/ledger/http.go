package ledger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/ledger/store"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/audit"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	defaultLedgerPage = 50
	maxLedgerPage     = 200
)

// API errors of the admin ledger routes.
var (
	ErrAdjustmentAmountInvalid = httpx.NewError(http.StatusUnprocessableEntity, "invalid_adjustment_amount",
		"Số tiền điều chỉnh phải khác 0 và không vượt quá 1.000.000.000.000 đồng")
	ErrAdjustmentReasonInvalid = httpx.NewError(http.StatusUnprocessableEntity, "invalid_adjustment_reason",
		"Vui lòng nhập lý do điều chỉnh (tối đa 500 ký tự)")
	ErrAdjustmentBooking = httpx.NewError(http.StatusUnprocessableEntity, "booking_not_of_partner",
		"Đơn đặt chỗ không thuộc đối tác này")
)

// PartnerRoutes registers the partner balance API. The chain is
// Authenticate -> RequireRole(partner) -> requireMember (partner.Service's
// RequireMember); the partner_id comes from partner.IDFrom, never the client.
func (s *Service) PartnerRoutes(mux httpx.Router, iss *auth.Issuer, requireMember func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/partner/balance", httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		partnerID, ok := partner.IDFrom(r.Context())
		if !ok {
			httpx.WriteError(w, r, httpx.ErrForbidden)
			return
		}
		b, err := s.Balance(r.Context(), partnerID)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, b)
	}), auth.Authenticate(iss), auth.RequireRole(auth.RolePartner), requireMember))
}

type ledgerPage struct {
	Balance      Balance      `json:"balance"`
	Entries      []LedgerLine `json:"entries"`
	NextAfterSeq *int64       `json:"next_after_seq"`
}

type adjustmentResult struct {
	Seq            int64 `json:"seq"`
	AvailableAfter int64 `json:"available_after"`
}

// AdminRoutes registers the Super Admin ledger API
// (Authenticate -> RequireRole(super_admin)): a partner's ledger by seq and
// manual adjustments (audited in the same tx).
func (s *Service) AdminRoutes(mux httpx.Router, iss *auth.Issuer, partners *partner.Service) {
	admin := func(h http.HandlerFunc) http.Handler {
		return httpx.Chain(h, auth.Authenticate(iss), auth.RequireRole(auth.RoleSuperAdmin))
	}

	mux.Handle("GET /v1/admin/partners/{id}/ledger", admin(func(w http.ResponseWriter, r *http.Request) {
		id, ok := partnerPathID(w, r)
		if !ok {
			return
		}
		afterSeq, limit, err := parseLedgerQuery(r)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		page, err := s.adminLedger(r.Context(), partners, id, afterSeq, limit)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, page)
	}))

	mux.Handle("POST /v1/admin/partners/{id}/adjustments", admin(func(w http.ResponseWriter, r *http.Request) {
		id, ok := partnerPathID(w, r)
		if !ok {
			return
		}
		var in struct {
			Amount    int64      `json:"amount"`
			Reason    string     `json:"reason"`
			BookingID *uuid.UUID `json:"booking_id"`
		}
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		res, err := s.adminAdjust(r.Context(), partners, p.UserID, Adjustment{
			PartnerID: id, BookingID: in.BookingID, Amount: in.Amount, Reason: in.Reason, CreatedBy: p.UserID,
		})
		if err != nil {
			httpx.WriteError(w, r, adjustmentAPIError(err))
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, res)
	}))
}

// adminLedger reads the balance and one page of entries in one snapshot.
func (s *Service) adminLedger(ctx context.Context, partners *partner.Service, partnerID uuid.UUID, afterSeq int64, limit int) (ledgerPage, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ledgerPage{}, fmt.Errorf("begin admin ledger tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := partners.Status(ctx, tx, partnerID, false); err != nil {
		return ledgerPage{}, err
	}
	q := store.New(tx)
	var page ledgerPage
	bal, err := q.GetPartnerBalance(ctx, partnerID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ledgerPage{}, fmt.Errorf("get partner balance: %w", err)
	}
	page.Balance = Balance{Pending: bal.Pending, Available: bal.Available}
	// One extra row tells whether a next page exists.
	lines, err := entriesAfter(ctx, q, partnerID, afterSeq, int32(limit+1))
	if err != nil {
		return ledgerPage{}, err
	}
	if len(lines) > limit {
		lines = lines[:limit]
		next := lines[limit-1].Seq
		page.NextAfterSeq = &next
	}
	page.Entries = lines
	return page, nil
}

// adminAdjust posts a Super Admin adjustment and its audit row in one tx.
// Locks follow DB §1.1: partners (read, unlocked) → bookings (FOR KEY SHARE,
// inside PostAdjustment when booking_id is set) → partner_balances.
func (s *Service) adminAdjust(ctx context.Context, partners *partner.Service, actor uuid.UUID, a Adjustment) (adjustmentResult, error) {
	var res adjustmentResult
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := partners.Status(ctx, tx, a.PartnerID, false); err != nil {
			return err
		}
		posted, err := s.PostAdjustment(ctx, tx, a)
		if err != nil {
			return err
		}
		res = adjustmentResult{Seq: posted.Seq, AvailableAfter: posted.AvailableAfter}
		return audit.Record(ctx, tx, s.clock.Now(), audit.Entry{
			ActorUserID: actor,
			Action:      "ledger.adjustment",
			TargetKind:  "partner",
			TargetID:    &a.PartnerID,
			After: map[string]any{
				"amount":     a.Amount,
				"booking_id": a.BookingID,
				"seq":        posted.Seq,
			},
			Reason: strings.TrimSpace(a.Reason),
		})
	})
	return res, err
}

// adjustmentAPIError maps the ledger's adjustment errors to API errors.
func adjustmentAPIError(err error) error {
	switch {
	case errors.Is(err, ErrInvalidAmount):
		return ErrAdjustmentAmountInvalid
	case errors.Is(err, ErrInvalidReason):
		return ErrAdjustmentReasonInvalid
	case errors.Is(err, ErrBookingNotOfPartner):
		return ErrAdjustmentBooking
	}
	return err
}

// partnerPathID parses {id}; a malformed id is a 404 like an unknown one.
func partnerPathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return uuid.Nil, false
	}
	return id, true
}

// parseLedgerQuery reads after_seq (default 0, >= 0) and limit (default 50,
// >= 1, capped at 200); anything else is ErrInvalidRequest.
func parseLedgerQuery(r *http.Request) (afterSeq int64, limit int, err error) {
	q := r.URL.Query()
	limit = defaultLedgerPage
	if v := q.Get("after_seq"); v != "" {
		afterSeq, err = strconv.ParseInt(v, 10, 64)
		if err != nil || afterSeq < 0 {
			return 0, 0, httpx.ErrInvalidRequest
		}
	}
	if v := q.Get("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil || limit < 1 {
			return 0, 0, httpx.ErrInvalidRequest
		}
		limit = min(limit, maxLedgerPage)
	}
	return afterSeq, limit, nil
}
