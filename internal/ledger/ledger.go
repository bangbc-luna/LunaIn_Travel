// Package ledger owns the partner ledger (append-only, sequenced entries and
// the partner_balances cache), the per-booking settlements and the platform
// commission book (DB §8).
//
// Every posting method runs inside the caller's transaction. The caller
// MUST already hold the booking row FOR UPDATE; this package locks
// booking_settlements (a booking child row) next and partner_balances LAST,
// following the global lock order (DB §1.1). A posting that would repeat one
// already made for the booking fails with ErrAlreadyPosted; the caller's tx
// is then aborted and must be rolled back.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// ErrInvalidAmount: a negative amount, a commission above gross, or a
	// waiver that is not the booking's commission.
	ErrInvalidAmount = errors.New("ledger: invalid amount")
	// ErrAlreadyPosted: this posting was already made for the booking or
	// payout (unique violation on the settlement, the per-booking entry kind,
	// the platform commission kind or the per-payout entry kind). The tx is
	// aborted.
	ErrAlreadyPosted = errors.New("ledger: already posted")
	// ErrNothingAvailable: a payout hold while available <= 0.
	ErrNothingAvailable = errors.New("ledger: nothing available")
	// ErrExceedsAvailable: a payout hold above the available balance.
	ErrExceedsAvailable = errors.New("ledger: amount exceeds available")
	// ErrInvalidReason: an adjustment reason empty after trimming or longer
	// than 500 runes.
	ErrInvalidReason = errors.New("ledger: invalid adjustment reason")
	// ErrBookingNotOfPartner: an adjustment's booking is unknown or belongs
	// to another partner. The tx is aborted.
	ErrBookingNotOfPartner = errors.New("ledger: booking does not belong to the partner")
	// ErrEntryNotFound: no ledger entry with that seq for the partner.
	ErrEntryNotFound = errors.New("ledger: entry not found")
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

// entry is one ledger line to append. bookingID / payoutID / createdBy /
// reason are optional (nil) and must match what the entry's kind requires.
type entry struct {
	kind, bucket                   string
	amount                         int64
	bookingID, payoutID, createdBy *uuid.UUID
	reason                         *string
}

// Posted is where an appended entry landed: its seq and the partner's
// available balance right after it.
type Posted struct {
	Seq            int64
	AvailableAfter int64
}

// bookingEntry is an entry of a booking (earning, refund, waiver, release).
func bookingEntry(kind, bucket string, amount int64, bookingID uuid.UUID) entry {
	return entry{kind: kind, bucket: bucket, amount: amount, bookingID: &bookingID}
}

// PostBookingConfirmed records a confirmed booking: booking_earning
// +(Gross − Commission) in pending, the platform commission (skipped when 0)
// and the booking's settlement with net_pending = Gross − Commission.
//
// The caller MUST hold the bookings row FOR UPDATE. A second confirmation of
// the same booking is ErrAlreadyPosted.
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
		return fmt.Errorf("insert booking settlement: %w", mapDuplicate(err))
	}
	if err := s.postPlatform(ctx, q, b.BookingID, b.ServiceType, platformCommission, b.Commission, now); err != nil {
		return err
	}
	_, err := s.post(ctx, q, b.PartnerID, now, bookingEntry(kindBookingEarning, bucketPending, net, b.BookingID))
	return err
}

// PostRefund records a refund to the customer: refund_debit −refund in
// pending, taken from the booking's net_pending. A zero refund writes nothing.
//
// The caller MUST hold the bookings row FOR UPDATE. A booking is refunded at
// most once: a second non-zero refund is ErrAlreadyPosted.
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
	_, err := s.post(ctx, q, partnerID, now, bookingEntry(kindRefundDebit, bucketPending, -refund, bookingID))
	return err
}

// PostCommissionWaiver gives the commission back to the partner (force
// majeure): commission_waiver +commission in pending and a platform waiver of
// −commission. commission must equal the platform commission recorded for the
// booking at confirmation (0 when none was), else ErrInvalidAmount. A zero
// commission writes nothing.
//
// The caller MUST hold the bookings row FOR UPDATE. A second waiver is
// ErrAlreadyPosted.
func (s *Service) PostCommissionWaiver(ctx context.Context, tx pgx.Tx, bookingID, partnerID uuid.UUID, serviceType string, commission int64) error {
	if commission < 0 {
		return fmt.Errorf("%w: commission %d", ErrInvalidAmount, commission)
	}
	q := store.New(tx)
	if _, err := s.lockOpenSettlement(ctx, q, bookingID, partnerID); err != nil {
		return err
	}
	taken, err := q.GetPlatformCommission(ctx, bookingID)
	if errors.Is(err, pgx.ErrNoRows) {
		taken, err = 0, nil
	}
	if err != nil {
		return fmt.Errorf("get platform commission: %w", err)
	}
	if commission != taken {
		return fmt.Errorf("%w: waiver %d, commission taken %d", ErrInvalidAmount, commission, taken)
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
	_, err = s.post(ctx, q, partnerID, now, bookingEntry(kindCommissionWaiver, bucketPending, commission, bookingID))
	return err
}

// ScheduleRelease sets the booking's release_due_at unless it is already set
// or the settlement is already released (both no-ops). The caller MUST hold
// the bookings row FOR UPDATE.
func (s *Service) ScheduleRelease(ctx context.Context, tx pgx.Tx, bookingID uuid.UUID, dueAt time.Time) error {
	q := store.New(tx)
	st, err := s.lockSettlement(ctx, q, bookingID)
	if err != nil {
		return err
	}
	if st.ReleasedAt != nil {
		return nil
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
// already released booking does nothing. The caller MUST hold the bookings
// row FOR UPDATE.
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
	_, err = s.post(ctx, q, st.PartnerID, now,
		bookingEntry(kindRelease, bucketPending, -st.NetPending, bookingID),
		bookingEntry(kindRelease, bucketAvailable, st.NetPending, bookingID))
	return err
}

// ReleaseIfDue is Release, but only when the settlement is still due at now
// (release_due_at set and <= now, not released) once locked; otherwise it
// does nothing and reports false. The release job uses it after listing due
// settlements unlocked. The caller MUST hold the bookings row FOR UPDATE.
func (s *Service) ReleaseIfDue(ctx context.Context, tx pgx.Tx, bookingID uuid.UUID, now time.Time) (bool, error) {
	st, err := s.lockSettlement(ctx, store.New(tx), bookingID)
	if err != nil {
		return false, err
	}
	if st.ReleasedAt != nil || st.ReleaseDueAt == nil || st.ReleaseDueAt.After(now) {
		return false, nil
	}
	return true, s.Release(ctx, tx, bookingID)
}

// DueReleases lists (unlocked) up to limit bookings whose settlement is due
// for release at now and not released yet, oldest due first. The caller then
// locks each booking FOR UPDATE and calls Release.
func (s *Service) DueReleases(ctx context.Context, now time.Time, limit int32) ([]uuid.UUID, error) {
	ids, err := store.New(s.pool).ListDueReleases(ctx, store.ListDueReleasesParams{Now: now, Lim: limit})
	if err != nil {
		return nil, fmt.Errorf("list due releases: %w", err)
	}
	return ids, nil
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
// each bucket, seq contiguous from 1 up to last_seq, every entry's
// available_after (the running sum of the available bucket in seq order) and
// the last one against the cache. Any mismatch is an error. All reads share
// one snapshot.
func (s *Service) Verify(ctx context.Context, partnerID uuid.UUID) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin verify tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	problem, err := verifyPartner(ctx, store.New(tx), partnerID)
	if err != nil {
		return err
	}
	if problem != "" {
		return errors.New(problem)
	}
	return nil
}

// verifyPartner runs Verify's checks with q. A mismatch is reported as a
// non-empty problem; err is only for failed queries.
func verifyPartner(ctx context.Context, q *store.Queries, partnerID uuid.UUID) (problem string, err error) {
	cache, err := q.GetPartnerBalance(ctx, partnerID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("get partner balance: %w", err)
	}
	tot, err := q.LedgerTotals(ctx, partnerID)
	if err != nil {
		return "", fmt.Errorf("ledger totals: %w", err)
	}
	switch {
	case tot.Pending != cache.Pending:
		return fmt.Sprintf("ledger: partner %s pending cache %d, entries %d", partnerID, cache.Pending, tot.Pending), nil
	case tot.Available != cache.Available:
		return fmt.Sprintf("ledger: partner %s available cache %d, entries %d", partnerID, cache.Available, tot.Available), nil
	case tot.Entries != cache.LastSeq || tot.MaxSeq != cache.LastSeq:
		// seq > 0 and UNIQUE(partner_id, seq): count = max = last_seq means 1..last_seq without gaps.
		return fmt.Sprintf("ledger: partner %s last_seq %d, %d entries, max seq %d",
			partnerID, cache.LastSeq, tot.Entries, tot.MaxSeq), nil
	}
	if tot.Entries > 0 {
		bad, err := q.AvailableAfterMismatches(ctx, partnerID)
		if err != nil {
			return "", fmt.Errorf("available_after mismatches: %w", err)
		}
		if bad != 0 {
			return fmt.Sprintf("ledger: partner %s has %d entries with a wrong available_after", partnerID, bad), nil
		}
		last, err := q.LastAvailableAfter(ctx, partnerID)
		if err != nil {
			return "", fmt.Errorf("last available_after: %w", err)
		}
		if last != cache.Available {
			return fmt.Sprintf("ledger: partner %s available cache %d, last available_after %d",
				partnerID, cache.Available, last), nil
		}
	}
	return "", nil
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
		return fmt.Errorf("insert platform %s: %w", kind, mapDuplicate(err))
	}
	return nil
}

// lockBalance creates the partner's balance row lazily and locks it FOR
// UPDATE: the last lock of the transaction (DB §1.1).
func lockBalance(ctx context.Context, q *store.Queries, partnerID uuid.UUID, now time.Time) (store.PartnerBalance, error) {
	if err := q.EnsurePartnerBalance(ctx, store.EnsurePartnerBalanceParams{PartnerID: partnerID, Now: now}); err != nil {
		return store.PartnerBalance{}, fmt.Errorf("ensure partner balance: %w", err)
	}
	bal, err := q.LockPartnerBalance(ctx, partnerID)
	if err != nil {
		return store.PartnerBalance{}, fmt.Errorf("lock partner balance: %w", err)
	}
	return bal, nil
}

// post appends the non-zero entries for partnerID and returns where each
// landed (zero-amount entries are skipped and get no Posted). It creates the
// balance row lazily, locks it FOR UPDATE (the last lock of the transaction),
// hands out consecutive seq values, stamps available_after and updates the
// cache.
func (s *Service) post(ctx context.Context, q *store.Queries, partnerID uuid.UUID, now time.Time, entries ...entry) ([]Posted, error) {
	nonZero := entries[:0:0]
	for _, e := range entries {
		if e.amount != 0 {
			nonZero = append(nonZero, e)
		}
	}
	if len(nonZero) == 0 {
		return nil, nil
	}

	bal, err := lockBalance(ctx, q, partnerID, now)
	if err != nil {
		return nil, err
	}

	pending, available, seq := bal.Pending, bal.Available, bal.LastSeq
	posted := make([]Posted, 0, len(nonZero))
	for _, e := range nonZero {
		seq++
		switch e.bucket {
		case bucketPending:
			pending += e.amount
		case bucketAvailable:
			available += e.amount
		default:
			return nil, fmt.Errorf("ledger: unknown bucket %q", e.bucket)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return nil, err
		}
		if err := q.InsertLedgerEntry(ctx, store.InsertLedgerEntryParams{
			ID: id, PartnerID: partnerID, Seq: seq, AvailableAfter: available,
			BookingID: e.bookingID, PayoutID: e.payoutID, Kind: e.kind, Bucket: e.bucket, Amount: e.amount,
			Reason: e.reason, CreatedBy: e.createdBy, Now: now,
		}); err != nil {
			return nil, fmt.Errorf("insert ledger entry %s/%s: %w", e.kind, e.bucket, mapPostError(err))
		}
		posted = append(posted, Posted{Seq: seq, AvailableAfter: available})
	}
	if err := q.UpdatePartnerBalance(ctx, store.UpdatePartnerBalanceParams{
		Pending: pending, Available: available, LastSeq: seq, Now: now, PartnerID: partnerID,
	}); err != nil {
		return nil, fmt.Errorf("update partner balance: %w", err)
	}
	return posted, nil
}

// duplicatePostConstraints are the unique constraints that reject a posting
// repeated for the same booking or payout.
var duplicatePostConstraints = map[string]bool{
	"booking_settlements_pkey":                        true,
	"ledger_entries_booking_kind_bucket_idx":          true,
	"platform_commission_entries_booking_id_kind_key": true,
	"ledger_entries_payout_kind_idx":                  true,
}

// bookingPartnerFKs are the foreign keys an entry's booking_id violates when
// the booking does not exist or belongs to another partner.
var bookingPartnerFKs = map[string]bool{
	"ledger_entries_booking_id_partner_id_fkey": true,
	"ledger_entries_booking_id_fkey":            true,
}

// mapPostError is mapDuplicate plus ErrBookingNotOfPartner for an entry whose
// booking is unknown or another partner's (23503 on the booking FKs).
func mapPostError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" && bookingPartnerFKs[pgErr.ConstraintName] {
		return fmt.Errorf("%w: %w", ErrBookingNotOfPartner, err)
	}
	return mapDuplicate(err)
}

// mapDuplicate turns a unique violation of a per-booking posting constraint
// into ErrAlreadyPosted, keeping the driver error in the chain.
func mapDuplicate(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && duplicatePostConstraints[pgErr.ConstraintName] {
		return fmt.Errorf("%w: %w", ErrAlreadyPosted, err)
	}
	return err
}
