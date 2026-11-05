// Package ledger owns the partner ledger (append-only, sequenced entries and
// the partner_balances cache), the per-booking settlements and the platform
// commission book (DB §8).
//
// Every posting method runs inside the caller's transaction. The caller
// already holds its booking-level locks; this package locks
// booking_settlements (a booking child row) next and partner_balances LAST,
// following the global lock order (DB §1.1).
package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bangbc-luna/lunain-travel/internal/ledger/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

// Entry kinds and buckets (ledger_entries.kind / bucket).
const (
	kindBookingEarning   = "booking_earning"
	kindRefundDebit      = "refund_debit"
	kindCommissionWaiver = "commission_waiver"
	kindRelease          = "release"

	bucketPending   = "pending"
	bucketAvailable = "available"

	platformCommission = "commission"
	platformWaiver     = "waiver"
)

var (
	// ErrSettlementNotFound: the booking has no settlement (it was never
	// confirmed in the ledger) or it belongs to another partner.
	ErrSettlementNotFound = errors.New("ledger: booking settlement not found")
	// ErrAlreadyReleased: the booking's money already left pending.
	ErrAlreadyReleased = errors.New("ledger: booking settlement already released")
	// ErrInvalidAmount: a negative amount, or a commission above gross.
	ErrInvalidAmount = errors.New("ledger: invalid amount")
)

// Service posts ledger entries and reads partner balances.
type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewService builds a Service.
func NewService(pool *pgxpool.Pool, c clock.Clock) *Service {
	return &Service{pool: pool, clock: c}
}

// BookingMoney is what a confirmed booking brings into the ledger.
type BookingMoney struct {
	BookingID, PartnerID uuid.UUID
	ServiceType          string
	Gross, Commission    int64
}

// Balance is a partner's cached balances; either may be negative.
type Balance struct {
	Pending   int64 `json:"pending"`
	Available int64 `json:"available"`
}

type entry struct {
	kind, bucket string
	amount       int64
}

// PostBookingConfirmed records a confirmed booking: booking_earning
// +(Gross − Commission) in pending, the platform commission (skipped when 0)
// and the booking's settlement with net_pending = Gross − Commission.
func (s *Service) PostBookingConfirmed(ctx context.Context, tx pgx.Tx, b BookingMoney) error {
	if b.Gross < 0 || b.Commission < 0 || b.Commission > b.Gross {
		return fmt.Errorf("%w: gross %d, commission %d", ErrInvalidAmount, b.Gross, b.Commission)
	}
	q := store.New(tx)
	now := s.clock.Now()
	net := b.Gross - b.Commission

	// Settlement first: a booking child row, locked before partner_balances.
	// Its primary key also rejects a second confirmation of the same booking.
	if err := q.InsertSettlement(ctx, store.InsertSettlementParams{
		BookingID: b.BookingID, PartnerID: b.PartnerID, NetPending: net, Now: now,
	}); err != nil {
		return fmt.Errorf("insert booking settlement: %w", err)
	}
	if err := s.postPlatform(ctx, q, b.BookingID, b.ServiceType, platformCommission, b.Commission, now); err != nil {
		return err
	}
	return s.post(ctx, q, b.PartnerID, b.BookingID, now, entry{kindBookingEarning, bucketPending, net})
}

// PostRefund records a refund to the customer: refund_debit −refund in
// pending, taken from the booking's net_pending. A zero refund writes nothing.
func (s *Service) PostRefund(ctx context.Context, tx pgx.Tx, bookingID, partnerID uuid.UUID, refund int64) error {
	if refund < 0 {
		return fmt.Errorf("%w: refund %d", ErrInvalidAmount, refund)
	}
	q := store.New(tx)
	if _, err := s.lockOpenSettlement(ctx, q, bookingID, partnerID); err != nil {
		return err
	}
	if refund == 0 {
		return nil
	}
	now := s.clock.Now()
	if err := q.AddSettlementNetPending(ctx, store.AddSettlementNetPendingParams{
		Delta: -refund, Now: now, BookingID: bookingID,
	}); err != nil {
		return fmt.Errorf("update settlement net pending: %w", err)
	}
	return s.post(ctx, q, partnerID, bookingID, now, entry{kindRefundDebit, bucketPending, -refund})
}

// PostCommissionWaiver gives the commission back to the partner (force
// majeure): commission_waiver +commission in pending and a platform waiver of
// −commission. A zero commission writes nothing.
func (s *Service) PostCommissionWaiver(ctx context.Context, tx pgx.Tx, bookingID, partnerID uuid.UUID, serviceType string, commission int64) error {
	if commission < 0 {
		return fmt.Errorf("%w: commission %d", ErrInvalidAmount, commission)
	}
	q := store.New(tx)
	if _, err := s.lockOpenSettlement(ctx, q, bookingID, partnerID); err != nil {
		return err
	}
	if commission == 0 {
		return nil
	}
	now := s.clock.Now()
	if err := q.AddSettlementNetPending(ctx, store.AddSettlementNetPendingParams{
		Delta: commission, Now: now, BookingID: bookingID,
	}); err != nil {
		return fmt.Errorf("update settlement net pending: %w", err)
	}
	if err := s.postPlatform(ctx, q, bookingID, serviceType, platformWaiver, -commission, now); err != nil {
		return err
	}
	return s.post(ctx, q, partnerID, bookingID, now, entry{kindCommissionWaiver, bucketPending, commission})
}

// ScheduleRelease sets the booking's release_due_at unless it is already set.
func (s *Service) ScheduleRelease(ctx context.Context, tx pgx.Tx, bookingID uuid.UUID, dueAt time.Time) error {
	q := store.New(tx)
	if _, err := s.lockSettlement(ctx, q, bookingID); err != nil {
		return err
	}
	if err := q.SetSettlementReleaseDue(ctx, store.SetSettlementReleaseDueParams{
		ReleaseDueAt: dueAt, Now: s.clock.Now(), BookingID: bookingID,
	}); err != nil {
		return fmt.Errorf("set settlement release due: %w", err)
	}
	return nil
}

// Release moves the booking's net_pending from pending to available (two
// release entries; none when it is 0) and closes the settlement. Releasing an
// already released booking does nothing.
func (s *Service) Release(ctx context.Context, tx pgx.Tx, bookingID uuid.UUID) error {
	q := store.New(tx)
	st, err := s.lockSettlement(ctx, q, bookingID)
	if err != nil {
		return err
	}
	if st.ReleasedAt != nil {
		return nil
	}
	now := s.clock.Now()
	if err := q.MarkSettlementReleased(ctx, store.MarkSettlementReleasedParams{Now: now, BookingID: bookingID}); err != nil {
		return fmt.Errorf("mark settlement released: %w", err)
	}
	return s.post(ctx, q, st.PartnerID, bookingID, now,
		entry{kindRelease, bucketPending, -st.NetPending},
		entry{kindRelease, bucketAvailable, st.NetPending})
}

// Balance returns the partner's cached balances; 0 and 0 before any entry.
func (s *Service) Balance(ctx context.Context, partnerID uuid.UUID) (Balance, error) {
	b, err := store.New(s.pool).GetPartnerBalance(ctx, partnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Balance{}, nil
	}
	if err != nil {
		return Balance{}, fmt.Errorf("get partner balance: %w", err)
	}
	return Balance{Pending: b.Pending, Available: b.Available}, nil
}

// Verify checks the partner_balances cache against the entries: the sum of
// each bucket, seq contiguous from 1 up to last_seq, and the last entry's
// available_after. Any mismatch is an error. All reads share one snapshot.
func (s *Service) Verify(ctx context.Context, partnerID uuid.UUID) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin verify tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := store.New(tx)

	var cache store.PartnerBalance
	cache, err = q.GetPartnerBalance(ctx, partnerID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("get partner balance: %w", err)
	}
	tot, err := q.LedgerTotals(ctx, partnerID)
	if err != nil {
		return fmt.Errorf("ledger totals: %w", err)
	}
	switch {
	case tot.Pending != cache.Pending:
		return fmt.Errorf("ledger: partner %s pending cache %d, entries %d", partnerID, cache.Pending, tot.Pending)
	case tot.Available != cache.Available:
		return fmt.Errorf("ledger: partner %s available cache %d, entries %d", partnerID, cache.Available, tot.Available)
	case tot.Entries != cache.LastSeq || tot.MaxSeq != cache.LastSeq:
		// seq > 0 and UNIQUE(partner_id, seq): count = max = last_seq means 1..last_seq without gaps.
		return fmt.Errorf("ledger: partner %s last_seq %d, %d entries, max seq %d",
			partnerID, cache.LastSeq, tot.Entries, tot.MaxSeq)
	}
	if tot.Entries > 0 {
		last, err := q.LastAvailableAfter(ctx, partnerID)
		if err != nil {
			return fmt.Errorf("last available_after: %w", err)
		}
		if last != cache.Available {
			return fmt.Errorf("ledger: partner %s available cache %d, last available_after %d",
				partnerID, cache.Available, last)
		}
	}
	return nil
}

// lockSettlement locks the booking's settlement row FOR UPDATE.
func (s *Service) lockSettlement(ctx context.Context, q *store.Queries, bookingID uuid.UUID) (store.BookingSettlement, error) {
	st, err := q.LockSettlement(ctx, bookingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, ErrSettlementNotFound
	}
	if err != nil {
		return st, fmt.Errorf("lock booking settlement: %w", err)
	}
	return st, nil
}

// lockOpenSettlement locks the settlement and requires it to belong to
// partnerID and not to be released yet.
func (s *Service) lockOpenSettlement(ctx context.Context, q *store.Queries, bookingID, partnerID uuid.UUID) (store.BookingSettlement, error) {
	st, err := s.lockSettlement(ctx, q, bookingID)
	if err != nil {
		return st, err
	}
	if st.PartnerID != partnerID {
		return st, ErrSettlementNotFound
	}
	if st.ReleasedAt != nil {
		return st, ErrAlreadyReleased
	}
	return st, nil
}

// postPlatform writes one platform commission entry; a zero amount is skipped
// (a 0% rate has no commission and so nothing to waive).
func (s *Service) postPlatform(ctx context.Context, q *store.Queries, bookingID uuid.UUID, serviceType, kind string, amount int64, now time.Time) error {
	if amount == 0 {
		return nil
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if err := q.InsertPlatformCommission(ctx, store.InsertPlatformCommissionParams{
		ID: id, BookingID: bookingID, ServiceType: serviceType, Kind: kind, Amount: amount, Now: now,
	}); err != nil {
		return fmt.Errorf("insert platform %s: %w", kind, err)
	}
	return nil
}

// post appends the non-zero entries for partnerID. It creates the balance row
// lazily, locks it FOR UPDATE (the last lock of the transaction), hands out
// consecutive seq values, stamps available_after and updates the cache.
func (s *Service) post(ctx context.Context, q *store.Queries, partnerID, bookingID uuid.UUID, now time.Time, entries ...entry) error {
	nonZero := entries[:0:0]
	for _, e := range entries {
		if e.amount != 0 {
			nonZero = append(nonZero, e)
		}
	}
	if len(nonZero) == 0 {
		return nil
	}

	if err := q.EnsurePartnerBalance(ctx, store.EnsurePartnerBalanceParams{PartnerID: partnerID, Now: now}); err != nil {
		return fmt.Errorf("ensure partner balance: %w", err)
	}
	bal, err := q.LockPartnerBalance(ctx, partnerID)
	if err != nil {
		return fmt.Errorf("lock partner balance: %w", err)
	}

	pending, available, seq := bal.Pending, bal.Available, bal.LastSeq
	for _, e := range nonZero {
		seq++
		switch e.bucket {
		case bucketPending:
			pending += e.amount
		case bucketAvailable:
			available += e.amount
		default:
			return fmt.Errorf("ledger: unknown bucket %q", e.bucket)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if err := q.InsertLedgerEntry(ctx, store.InsertLedgerEntryParams{
			ID: id, PartnerID: partnerID, Seq: seq, AvailableAfter: available,
			BookingID: &bookingID, Kind: e.kind, Bucket: e.bucket, Amount: e.amount, Now: now,
		}); err != nil {
			return fmt.Errorf("insert ledger entry %s/%s: %w", e.kind, e.bucket, err)
		}
	}
	if err := q.UpdatePartnerBalance(ctx, store.UpdatePartnerBalanceParams{
		Pending: pending, Available: available, LastSeq: seq, Now: now, PartnerID: partnerID,
	}); err != nil {
		return fmt.Errorf("update partner balance: %w", err)
	}
	return nil
}
