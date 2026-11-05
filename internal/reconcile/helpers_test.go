package reconcile_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/jobs"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
	"github.com/bangbc-luna/lunain-travel/internal/platform/storage"
	"github.com/bangbc-luna/lunain-travel/internal/platform/testutil"
	"github.com/bangbc-luna/lunain-travel/internal/reconcile"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

// testNow is 2026-11-05 02:30 in Vietnam (2026-11-04 19:30 UTC).
var testNow = time.Date(2026, 11, 5, 2, 30, 0, 0, time.FixedZone("ICT", 7*3600))

const adminEmail = "admin@lunain.vn"

type env struct {
	Pool      *pgxpool.Pool
	Clock     *clock.Fixed
	Issuer    *auth.Issuer
	Fake      *payment.Fake
	Payments  *payment.Service
	Svc       *reconcile.Service
	Handler   http.Handler
	Admin     uuid.UUID
	PartnerID uuid.UUID
	Owner     uuid.UUID
	Booking   *booking.Service
	Listings  *listing.Service
	Files     *files.Service
	Catalog   *catalog.Service

	listingID, adult, slot, customer uuid.UUID
}

// newEnv wires the real services with payment.Fake, a fixed clock and a
// never-started job client (emails stay in river_job).
func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	pool := testutil.Postgres(t)
	clk := &clock.Fixed{T: testNow}
	iss := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk)
	log := slog.New(slog.DiscardHandler)
	idSvc := identity.NewService(identity.Deps{
		Pool: pool, Issuer: iss, Clock: clk, CodePepper: []byte("test-pepper"), RefreshTTL: 30 * 24 * time.Hour,
	})
	cat := catalog.NewService(pool, clk)
	geoSvc := geo.NewService(pool)
	fileSvc := files.NewService(pool, storage.NewMemory(), clk, "http://files.test")
	pSvc := partner.NewService(partner.Deps{Pool: pool, Clock: clk, Users: idSvc, Catalog: cat, Geo: geoSvc, Files: fileSvc})
	lSvc := listing.NewService(listing.Deps{
		Pool: pool, Clock: clk, Partners: pSvc, Catalog: cat, Geo: geoSvc, Files: fileSvc, BookingGuard: booking.Guard{},
	})
	fake := payment.NewFake()
	fake.Now = clk.Now
	paySvc := payment.NewService(pool, clk, fake, log)
	ledgerSvc := ledger.NewService(pool, clk)
	workers := river.NewWorkers()
	river.AddWorker(workers, &mailer.SendEmailWorker{Mailer: &mailer.Recorder{}})
	jobClient, err := jobs.NewClient(pool, workers, log)
	require.NoError(t, err)
	paySvc.SetOps(payment.OpsDeps{Jobs: jobClient, Users: idSvc, Files: fileSvc})
	bSvc := booking.NewService(booking.Deps{
		Pool: pool, Clock: clk, Listings: lSvc, Catalog: cat, Settings: settings.NewService(pool, clk),
		Identity: idSvc, Payments: paySvc, Ledger: ledgerSvc, Jobs: jobClient, Log: log,
	})
	svc := reconcile.NewService(reconcile.Deps{
		Pool: pool, Clock: clk, Payments: paySvc, Ledger: ledgerSvc, Handler: bSvc,
		Identity: idSvc, Jobs: jobClient, Log: log,
	})
	mux := http.NewServeMux()
	svc.AdminRoutes(mux, iss)
	mux.HandleFunc("/", httpx.NotFound)
	e := &env{
		Pool: pool, Clock: clk, Issuer: iss, Fake: fake, Payments: paySvc, Svc: svc,
		Handler: httpx.Chain(mux, httpx.RequestID, httpx.Recover),
	}
	require.NoError(t, db.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		e.Admin, err = idSvc.CreateInternalUser(ctx, tx, adminEmail, "adminpass123", auth.RoleSuperAdmin)
		return err
	}))
	p, err := pSvc.Create(ctx, e.Admin, partner.CreateInput{
		LegalName: "Công ty Alpha", DisplayName: "Alpha", OwnerEmail: "owner@alpha.vn", OwnerPassword: "matkhau123",
		ServiceTypes: []string{"attraction_ticket"},
	})
	require.NoError(t, err)
	e.PartnerID, e.Owner = p.ID, p.OwnerUserID
	e.Booking, e.Listings, e.Files, e.Catalog = bSvc, lSvc, fileSvc, cat
	return e
}

// book creates a bookable listing (once) and a pending 150.000 booking with
// an activated payment link; it returns the payment's order code.
func (e *env) book(t *testing.T, key string) int64 {
	t.Helper()
	ctx := context.Background()
	if e.listingID == uuid.Nil {
		var province, ward string
		require.NoError(t, e.Pool.QueryRow(ctx, `SELECT province_code, code FROM wards ORDER BY code LIMIT 1`).Scan(&province, &ward))
		l, err := e.Listings.Create(ctx, e.PartnerID, e.Owner, listing.CreateInput{ServiceType: "attraction_ticket", Title: "Vé tham quan"})
		require.NoError(t, err)
		desc, addr := "Mô tả chi tiết", "1 Đường A"
		_, err = e.Listings.Update(ctx, e.PartnerID, e.Owner, l.ID, listing.UpdateInput{
			Description: &desc, Address: &addr, ProvinceCode: &province, WardCode: &ward,
		})
		require.NoError(t, err)
		var buf bytes.Buffer
		require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
		f, err := e.Files.Upload(ctx, auth.Principal{UserID: e.Owner, Role: auth.RolePartner}, "listing_image", &buf)
		require.NoError(t, err)
		_, _, err = e.Listings.SetImages(ctx, e.PartnerID, e.Owner, l.ID, []uuid.UUID{f.ID}, nil)
		require.NoError(t, err)
		adult, err := e.Listings.CreateTicketType(ctx, e.PartnerID, e.Owner, l.ID,
			listing.TicketTypeInput{Name: "Vé vào cổng", Audience: "adult", Price: 150_000})
		require.NoError(t, err)
		slot, err := e.Listings.CreateTimeSlot(ctx, e.PartnerID, e.Owner, l.ID,
			listing.TimeSlotInput{StartTime: "08:00", EndTime: "17:00", Weekdays: 0b1111111})
		require.NoError(t, err)
		_, err = e.Listings.SetCancellationPolicy(ctx, e.PartnerID, e.Owner, l.ID, []listing.CancellationTier{
			{HoursBefore: 48, RefundBps: 10000},
		})
		require.NoError(t, err)
		_, err = e.Catalog.CreateCommissionRate(ctx, e.Admin, "attraction_ticket", 1000, nil)
		require.NoError(t, err)
		_, err = e.Listings.Submit(ctx, e.PartnerID, e.Owner, l.ID)
		require.NoError(t, err)
		_, err = e.Listings.Approve(ctx, e.Admin, l.ID)
		require.NoError(t, err)
		e.listingID, e.adult, e.slot = l.ID, adult.ID, slot.ID
		e.customer = uuid.Must(uuid.NewV7())
		e.exec(t, `INSERT INTO users (id, email, role, email_verified_at) VALUES ($1, 'khach@gmail.com', 'customer', $2)`,
			e.customer, testNow)
		e.exec(t, `INSERT INTO customer_profiles (user_id) VALUES ($1)`, e.customer)
	}
	v, err := e.Booking.CreateAttraction(ctx, e.customer, key, booking.CreateAttractionInput{
		ListingID: e.listingID, VisitDate: "2026-11-10", TimeSlotID: e.slot,
		Items:       []booking.TicketLine{{TicketTypeID: e.adult, Quantity: 1}},
		ContactName: "Nguyễn Văn An", ContactPhone: "+84 912 345 678", ContactEmail: "an@gmail.com",
	})
	require.NoError(t, err)
	var oc int64
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT p.provider_order_code FROM payments p
		JOIN bookings b ON b.checkout_id = p.checkout_id WHERE b.id = $1 AND p.status = 'pending'`, v.ID).Scan(&oc))
	return oc
}

func (e *env) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	_, err := e.Pool.Exec(context.Background(), q, args...)
	require.NoError(t, err)
}

func (e *env) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.Pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func (e *env) token(t *testing.T, userID uuid.UUID, role auth.Role) string {
	t.Helper()
	s, _, err := e.Issuer.Issue(auth.Principal{UserID: userID, Role: role})
	require.NoError(t, err)
	return s
}

func (e *env) do(t *testing.T, method, path, tok, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	e.Handler.ServeHTTP(rec, req)
	return rec
}

// breakBalance makes the partner's cached balance disagree with its ledger.
func (e *env) breakBalance(t *testing.T) {
	t.Helper()
	e.exec(t, `INSERT INTO partner_balances (partner_id, available) VALUES ($1, 5)
		ON CONFLICT (partner_id) DO UPDATE SET available = partner_balances.available + 5`, e.PartnerID)
}
