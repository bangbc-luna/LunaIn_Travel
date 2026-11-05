package booking_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/bangbc-luna/lunain-travel/internal/settings"
)

// testNow is Thursday 2026-11-05 09:00 in Vietnam.
var testNow = time.Date(2026, 11, 5, 9, 0, 0, 0, time.FixedZone("ICT", 7*3600))

const (
	attraction = "attraction_ticket"
	everyDay   = 0b1111111
	// visitDate is two days after testNow; the 08:00 slot starts 47h later.
	visitDate = "2026-11-07"
)

// env wires every real service on a fresh Postgres with payment.Fake and a
// fixed clock, plus one approved attraction listing ready for booking.
type env struct {
	Svc      *booking.Service
	Listing  *listing.Service
	Partner  *partner.Service
	Identity *identity.Service
	Catalog  *catalog.Service
	Payments *payment.Service
	Fake     *payment.Fake
	Ledger   *ledger.Service
	Files    *files.Service
	Issuer   *auth.Issuer
	Pool     *pgxpool.Pool
	Clock    *clock.Fixed
	Handler  http.Handler
	Admin    uuid.UUID

	// The fixture listing: adult 150.000, child 80.000, slot 08:00-17:00
	// every day, policy [{48,10000},{24,5000}], commission 1000 bps.
	P        partner.Partner
	L        listing.Listing
	Adult    listing.TicketType
	Child    listing.TicketType
	Slot     listing.TimeSlot
	Customer uuid.UUID
}

// newBareEnv wires the services without commission rate or fixture listing.
func newBareEnv(t *testing.T) *env {
	t.Helper()
	pool := testutil.Postgres(t)
	clk := &clock.Fixed{T: testNow}
	iss := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk)
	idSvc := identity.NewService(identity.Deps{
		Pool: pool, Issuer: iss, Clock: clk,
		CodePepper: []byte("test-pepper"), RefreshTTL: 30 * 24 * time.Hour,
	})
	cat := catalog.NewService(pool, clk)
	geoSvc := geo.NewService(pool)
	fileSvc := files.NewService(pool, storage.NewMemory(), clk, "http://files.test")
	pSvc := partner.NewService(partner.Deps{
		Pool: pool, Clock: clk, Users: idSvc, Catalog: cat, Geo: geoSvc, Files: fileSvc,
	})
	lSvc := listing.NewService(listing.Deps{
		Pool: pool, Clock: clk, Partners: pSvc, Catalog: cat, Geo: geoSvc, Files: fileSvc,
	})
	fake := payment.NewFake()
	fake.Now = clk.Now
	paySvc := payment.NewService(pool, clk, fake, nil)
	ledgerSvc := ledger.NewService(pool, clk)
	// Never started: emails stay queued in river_job, where tests count them.
	workers := river.NewWorkers()
	river.AddWorker(workers, &mailer.SendEmailWorker{Mailer: &mailer.Recorder{}})
	jobClient, err := jobs.NewClient(pool, workers, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	svc := booking.NewService(booking.Deps{
		Pool: pool, Clock: clk, Listings: lSvc, Catalog: cat, Settings: settings.NewService(pool, clk),
		Identity: idSvc, Payments: paySvc, Ledger: ledgerSvc, Jobs: jobClient,
	})
	mux := http.NewServeMux()
	svc.CustomerRoutes(mux, iss)
	svc.PartnerRoutes(mux, iss, pSvc.RequireMember())
	svc.AdminRoutes(mux, iss)
	paySvc.Routes(mux, payment.RouteDeps{Handler: svc, ClientIP: httpx.ClientIPFunc(0), AppEnv: "test"})
	mux.HandleFunc("/", httpx.NotFound)
	e := &env{
		Svc: svc, Listing: lSvc, Partner: pSvc, Identity: idSvc, Catalog: cat, Payments: paySvc, Fake: fake,
		Ledger: ledgerSvc, Files: fileSvc, Issuer: iss, Pool: pool, Clock: clk,
		Handler: httpx.Chain(mux, httpx.RequestID, httpx.Recover),
	}
	require.NoError(t, db.InTx(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		e.Admin, err = idSvc.CreateInternalUser(context.Background(), tx, "admin@lunain.vn", "adminpass123", auth.RoleSuperAdmin)
		return err
	}))
	return e
}

// newEnv is newBareEnv plus the 1000 bps commission rate, the fixture
// listing and a verified customer with an empty profile.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := newBareEnv(t)
	_, err := e.Catalog.CreateCommissionRate(context.Background(), e.Admin, attraction, 1000, nil)
	require.NoError(t, err)
	e.fixtureListing(t)
	e.Customer = e.customer(t, "khach@gmail.com")
	return e
}

func (e *env) fixtureListing(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	p, err := e.Partner.Create(ctx, e.Admin, partner.CreateInput{
		LegalName: "Công ty Alpha", DisplayName: "Alpha",
		OwnerEmail: "owner@alpha.vn", OwnerPassword: "matkhau123",
		ServiceTypes: []string{attraction},
	})
	require.NoError(t, err)
	e.P = p
	e.L, e.Adult, e.Child, e.Slot = e.approvedListing(t, p, "Vé tham quan Bà Nà")
}

// approvedListing creates an approved (visible) attraction listing of p.
func (e *env) approvedListing(t *testing.T, p partner.Partner, title string) (listing.Listing, listing.TicketType, listing.TicketType, listing.TimeSlot) {
	t.Helper()
	ctx := context.Background()
	var province, ward string
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT province_code, code FROM wards ORDER BY code LIMIT 1`).Scan(&province, &ward))
	l, err := e.Listing.Create(ctx, p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: attraction, Title: title})
	require.NoError(t, err)
	desc, addr := "Mô tả chi tiết", "1 Đường A"
	_, err = e.Listing.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{
		Description: &desc, Address: &addr, ProvinceCode: &province, WardCode: &ward,
	})
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	f, err := e.Files.Upload(ctx, auth.Principal{UserID: p.OwnerUserID, Role: auth.RolePartner}, "listing_image", &buf)
	require.NoError(t, err)
	_, _, err = e.Listing.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{f.ID}, nil)
	require.NoError(t, err)
	adult, err := e.Listing.CreateTicketType(ctx, p.ID, p.OwnerUserID, l.ID,
		listing.TicketTypeInput{Name: "Vé vào cổng", Audience: "adult", Price: 150_000})
	require.NoError(t, err)
	child, err := e.Listing.CreateTicketType(ctx, p.ID, p.OwnerUserID, l.ID,
		listing.TicketTypeInput{Name: "Vé vào cổng", Audience: "child", Price: 80_000, SortOrder: 1})
	require.NoError(t, err)
	slot, err := e.Listing.CreateTimeSlot(ctx, p.ID, p.OwnerUserID, l.ID,
		listing.TimeSlotInput{StartTime: "08:00", EndTime: "17:00", Weekdays: everyDay})
	require.NoError(t, err)
	_, err = e.Listing.SetCancellationPolicy(ctx, p.ID, p.OwnerUserID, l.ID, []listing.CancellationTier{
		{HoursBefore: 48, RefundBps: 10000}, {HoursBefore: 24, RefundBps: 5000},
	})
	require.NoError(t, err)
	_, err = e.Listing.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	l, err = e.Listing.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	return l, adult, child, slot
}

// customer inserts a verified customer with an empty profile.
func (e *env) customer(t *testing.T, email string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7())
	_, err := e.Pool.Exec(ctx, `INSERT INTO users (id, email, role, email_verified_at) VALUES ($1, $2, 'customer', $3)`,
		id, email, testNow)
	require.NoError(t, err)
	_, err = e.Pool.Exec(ctx, `INSERT INTO customer_profiles (user_id) VALUES ($1)`, id)
	require.NoError(t, err)
	return id
}

// input is a valid booking of 2 adult + 1 child tickets on visitDate 08:00.
func (e *env) input() booking.CreateAttractionInput {
	return booking.CreateAttractionInput{
		ListingID: e.L.ID, VisitDate: visitDate, TimeSlotID: e.Slot.ID,
		Items: []booking.TicketLine{
			{TicketTypeID: e.Adult.ID, Quantity: 2},
			{TicketTypeID: e.Child.ID, Quantity: 1},
		},
		ContactName: "Nguyễn Văn An", ContactPhone: "+84 912 345 678", ContactEmail: "An@Gmail.com",
	}
}

func (e *env) create(t *testing.T, key string) booking.BookingView {
	t.Helper()
	v, err := e.Svc.CreateAttraction(context.Background(), e.Customer, key, e.input())
	require.NoError(t, err)
	return v
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.Pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func (e *env) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	_, err := e.Pool.Exec(context.Background(), query, args...)
	require.NoError(t, err)
}

// confirm forces a booking to confirmed with raw SQL (Task 6 does it for real).
func (e *env) confirm(t *testing.T, id uuid.UUID) {
	t.Helper()
	e.exec(t, `UPDATE bookings SET status = 'confirmed', confirmed_at = $2 WHERE id = $1`, id, testNow)
}

func (e *env) token(t *testing.T, userID uuid.UUID, role auth.Role) string {
	t.Helper()
	s, _, err := e.Issuer.Issue(auth.Principal{UserID: userID, Role: role})
	require.NoError(t, err)
	return s
}

// do sends a request through the real mux; headers are key/value pairs.
func (e *env) do(t *testing.T, method, path, tok string, body any, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case nil:
	case string:
		buf.WriteString(b)
	default:
		require.NoError(t, json.NewEncoder(&buf).Encode(b))
	}
	req := httptest.NewRequest(method, path, &buf)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.Handler.ServeHTTP(rec, req)
	return rec
}

func requireCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	var eb struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &eb), rec.Body.String())
	require.Equal(t, code, eb.Error.Code)
}

func requireErr(t *testing.T, err error, status int, code string) {
	t.Helper()
	require.Error(t, err)
	var ae *httpx.AppError
	require.True(t, errors.As(err, &ae), "not an AppError: %v", err)
	require.Equal(t, status, ae.Status, err.Error())
	require.Equal(t, code, ae.Code)
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v), rec.Body.String())
	return v
}
