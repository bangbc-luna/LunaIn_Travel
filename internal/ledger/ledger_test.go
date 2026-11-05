package ledger_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

func TestConfirmThenRelease(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)

	f.confirm(t, b)
	f.requireBalance(t, f.partner, 900_000, 0)
	require.Equal(t, []commissionRow{{"commission", 100_000, attraction}}, f.commissions(t, b.BookingID))
	require.Equal(t, int64(900_000), f.settlement(t, b.BookingID).NetPending)

	f.release(t, b)
	f.requireBalance(t, f.partner, 0, 900_000)

	require.Equal(t, []entryRow{
		{1, "booking_earning", "pending", 900_000, 0, b.BookingID},
		{2, "release", "pending", -900_000, 0, b.BookingID},
		{3, "release", "available", 900_000, 900_000, b.BookingID},
	}, f.entries(t, f.partner))
	s := f.settlement(t, b.BookingID)
	require.Equal(t, int64(0), s.NetPending)
	require.NotNil(t, s.ReleasedAt)
	require.True(t, s.ReleasedAt.Equal(testNow))
}

// Spec §6.3: ticket 1.000.000, commission 10%, 50% refund -> partner 400.000.
func TestCancelHalfRefund(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)

	f.confirm(t, b)
	f.refund(t, b, 500_000)
	f.requireBalance(t, f.partner, 400_000, 0)
	require.Equal(t, int64(400_000), f.settlement(t, b.BookingID).NetPending)

	f.release(t, b)
	f.requireBalance(t, f.partner, 0, 400_000)
	require.Equal(t, []entryRow{
		{1, "booking_earning", "pending", 900_000, 0, b.BookingID},
		{2, "refund_debit", "pending", -500_000, 0, b.BookingID},
		{3, "release", "pending", -400_000, 0, b.BookingID},
		{4, "release", "available", 400_000, 400_000, b.BookingID},
	}, f.entries(t, f.partner))
	// The commission is kept in full.
	require.Equal(t, []commissionRow{{"commission", 100_000, attraction}}, f.commissions(t, b.BookingID))
}

func TestFullRefundGoesNegative(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)

	f.confirm(t, b)
	f.refund(t, b, 1_000_000)
	f.requireBalance(t, f.partner, -100_000, 0)

	f.release(t, b)
	f.requireBalance(t, f.partner, 0, -100_000)
	got := f.entries(t, f.partner)
	require.Len(t, got, 4)
	require.Equal(t, entryRow{3, "release", "pending", 100_000, 0, b.BookingID}, got[2])
	require.Equal(t, entryRow{4, "release", "available", -100_000, -100_000, b.BookingID}, got[3])
}

func TestForceMajeureWaiver(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)

	f.confirm(t, b)
	f.refund(t, b, 1_000_000)
	f.waive(t, b)
	f.requireBalance(t, f.partner, 0, 0)
	require.Equal(t, int64(0), f.settlement(t, b.BookingID).NetPending)

	f.release(t, b)
	f.requireBalance(t, f.partner, 0, 0)
	// Net 0: no release rows at all, but the settlement is closed.
	require.Equal(t, []entryRow{
		{1, "booking_earning", "pending", 900_000, 0, b.BookingID},
		{2, "refund_debit", "pending", -1_000_000, 0, b.BookingID},
		{3, "commission_waiver", "pending", 100_000, 0, b.BookingID},
	}, f.entries(t, f.partner))
	require.NotNil(t, f.settlement(t, b.BookingID).ReleasedAt)
	require.Equal(t, []commissionRow{
		{"commission", 100_000, attraction},
		{"waiver", -100_000, attraction},
	}, f.commissions(t, b.BookingID))
}

func TestReleaseIdempotent(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)
	f.confirm(t, b)

	f.release(t, b)
	f.release(t, b)
	f.requireBalance(t, f.partner, 0, 900_000)
	require.Len(t, f.entries(t, f.partner), 3)

	// Concurrent releases of the same booking also post once.
	b2 := f.newBooking(t, 500_000, 50_000)
	f.confirm(t, b2)
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Go(func() {
			errs <- f.tryTx(func(ctx context.Context, tx pgx.Tx) error { return f.svc.Release(ctx, tx, b2.BookingID) })
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	f.requireBalance(t, f.partner, 0, 1_350_000)
	require.Len(t, f.entries(t, f.partner), 6)
}

// Controller Ruling 4: a 0% commission writes no platform row and no waiver.
func TestZeroCommission(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 0)

	f.confirm(t, b)
	f.requireBalance(t, f.partner, 1_000_000, 0)
	require.Empty(t, f.commissions(t, b.BookingID))

	f.refund(t, b, 1_000_000)
	f.waive(t, b) // commission 0: no-op
	f.requireBalance(t, f.partner, 0, 0)
	require.Empty(t, f.commissions(t, b.BookingID))
	require.Equal(t, []entryRow{
		{1, "booking_earning", "pending", 1_000_000, 0, b.BookingID},
		{2, "refund_debit", "pending", -1_000_000, 0, b.BookingID},
	}, f.entries(t, f.partner))
}

func TestZeroAmountsAreNotWritten(t *testing.T) {
	f := newFixture(t)
	// 100% commission: earning 0 is not written, the settlement still exists.
	b := f.newBooking(t, 100_000, 100_000)
	f.confirm(t, b)
	f.refund(t, b, 0)
	f.requireBalance(t, f.partner, 0, 0)
	require.Empty(t, f.entries(t, f.partner))
	require.Equal(t, []commissionRow{{"commission", 100_000, attraction}}, f.commissions(t, b.BookingID))

	f.release(t, b)
	require.Empty(t, f.entries(t, f.partner))
	require.NotNil(t, f.settlement(t, b.BookingID).ReleasedAt)
}

func TestScheduleReleaseKeepsFirstDueAt(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)
	f.confirm(t, b)
	require.Nil(t, f.settlement(t, b.BookingID).ReleaseDueAt)

	first := testNow.Add(72 * time.Hour)
	f.inTx(t, func(ctx context.Context, tx pgx.Tx) error { return f.svc.ScheduleRelease(ctx, tx, b.BookingID, first) })
	f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.ScheduleRelease(ctx, tx, b.BookingID, first.Add(time.Hour))
	})
	got := f.settlement(t, b.BookingID).ReleaseDueAt
	require.NotNil(t, got)
	require.True(t, got.Equal(first))

	err := f.tryTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.ScheduleRelease(ctx, tx, uuid.New(), first)
	})
	require.ErrorIs(t, err, ledger.ErrSettlementNotFound)
}

func TestInvalidPostings(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)

	post := func(fn func(ctx context.Context, tx pgx.Tx) error) error { return f.tryTx(fn) }

	// Refund / waiver / release before confirmation: no settlement.
	require.ErrorIs(t, post(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostRefund(ctx, tx, b.BookingID, b.PartnerID, 1)
	}), ledger.ErrSettlementNotFound)
	require.ErrorIs(t, post(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.Release(ctx, tx, b.BookingID)
	}), ledger.ErrSettlementNotFound)

	bad := b
	bad.Commission = 1_000_001
	require.Error(t, post(func(ctx context.Context, tx pgx.Tx) error { return f.svc.PostBookingConfirmed(ctx, tx, bad) }))
	bad.Commission = -1
	require.Error(t, post(func(ctx context.Context, tx pgx.Tx) error { return f.svc.PostBookingConfirmed(ctx, tx, bad) }))

	f.confirm(t, b)
	// Confirming twice is rejected (one settlement per booking).
	require.Error(t, post(func(ctx context.Context, tx pgx.Tx) error { return f.svc.PostBookingConfirmed(ctx, tx, b) }))
	require.Error(t, post(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostRefund(ctx, tx, b.BookingID, b.PartnerID, -1)
	}))
	require.ErrorIs(t, post(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostRefund(ctx, tx, b.BookingID, uuid.New(), 1)
	}), ledger.ErrSettlementNotFound)

	f.release(t, b)
	require.ErrorIs(t, post(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostRefund(ctx, tx, b.BookingID, b.PartnerID, 1)
	}), ledger.ErrAlreadyReleased)
	require.ErrorIs(t, post(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.PostCommissionWaiver(ctx, tx, b.BookingID, b.PartnerID, attraction, 100_000)
	}), ledger.ErrAlreadyReleased)
	f.requireBalance(t, f.partner, 0, 900_000)
}

func TestBalanceWithoutEntries(t *testing.T) {
	f := newFixture(t)
	f.requireBalance(t, f.partner, 0, 0)
	f.requireBalance(t, uuid.New(), 0, 0)
}

func TestVerifyDetectsDrift(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)
	f.confirm(t, b)
	require.NoError(t, f.svc.Verify(context.Background(), f.partner))

	f.mustExec(t, `UPDATE partner_balances SET pending = pending + 1 WHERE partner_id = $1`, f.partner)
	require.Error(t, f.svc.Verify(context.Background(), f.partner))
	f.mustExec(t, `UPDATE partner_balances SET pending = pending - 1, available = 5 WHERE partner_id = $1`, f.partner)
	require.Error(t, f.svc.Verify(context.Background(), f.partner))
	f.mustExec(t, `UPDATE partner_balances SET available = 0, last_seq = 2 WHERE partner_id = $1`, f.partner)
	require.Error(t, f.svc.Verify(context.Background(), f.partner))
}

func TestConcurrentPostsSerializeSeq(t *testing.T) {
	f := newFixture(t)
	const n = 20
	bookings := make([]ledger.BookingMoney, n)
	for i := range bookings {
		bookings[i] = f.newBooking(t, 100_000, 10_000)
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for _, b := range bookings {
		wg.Go(func() {
			errs <- f.tryTx(func(ctx context.Context, tx pgx.Tx) error { return f.svc.PostBookingConfirmed(ctx, tx, b) })
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	got := f.entries(t, f.partner)
	require.Len(t, got, n)
	seen := map[uuid.UUID]bool{}
	for i, e := range got {
		require.Equal(t, int64(i+1), e.Seq)
		require.False(t, seen[e.BookingID])
		seen[e.BookingID] = true
	}
	f.requireBalance(t, f.partner, n*90_000, 0)
}

func TestHTTP_PartnerBalance(t *testing.T) {
	f := newFixture(t)
	b := f.newBooking(t, 1_000_000, 100_000)
	f.confirm(t, b)
	f.release(t, b)
	b2 := f.newBooking(t, 200_000, 20_000)
	f.confirm(t, b2)

	// A second partner without entries sees zeros.
	otherOwner, _ := f.newPartner(t, "beta")

	iss := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, f.clock)
	partnerSvc := partner.NewService(partner.Deps{Pool: f.pool, Clock: f.clock})
	mux := http.NewServeMux()
	f.svc.PartnerRoutes(httpx.NewRecordingRouter(mux), iss, partnerSvc.RequireMember())

	get := func(user uuid.UUID) *httptest.ResponseRecorder {
		tok, _, err := iss.Issue(auth.Principal{UserID: user, Role: auth.RolePartner})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodGet, "/v1/partner/balance", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	rec := get(f.owner)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"pending":180000,"available":900000}`, rec.Body.String())

	rec = get(otherOwner)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"pending":0,"available":0}`, rec.Body.String())

	rec = get(uuid.New()) // partner role without membership
	require.Equal(t, http.StatusForbidden, rec.Code)
	var eb struct {
		Error struct{ Code string } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &eb))
	require.Equal(t, "forbidden", eb.Error.Code)
}
