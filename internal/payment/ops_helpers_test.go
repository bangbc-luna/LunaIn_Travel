package payment_test

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
	"strconv"
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

// opsNow is Thursday 2026-11-05 09:00 in Vietnam.
var opsNow = time.Date(2026, 11, 5, 9, 0, 0, 0, time.FixedZone("ICT", 7*3600))

const (
	// ops fixture: 2 adult (150.000) + 1 child (80.000) = 380.000.
	opsGross = int64(380_000)
	// opsVisitOver48h: the 08:00 slot is 71h after opsNow (100% refund).
	opsVisitOver48h = "2026-11-08"
	customerEmail   = "khach@gmail.com"
)

// opsEnv wires every real service (booking included) with payment.Fake, a
// fixed clock, the payment ops deps and a never-started job client (emails
// stay in river_job where tests count them).
type opsEnv struct {
	Pool      *pgxpool.Pool
	Clock     *clock.Fixed
	Issuer    *auth.Issuer
	Fake      *payment.Fake
	Payments  *payment.Service
	Booking   *booking.Service
	Ledger    *ledger.Service
	Files     *files.Service
	Handler   http.Handler
	Admin     uuid.UUID
	Customer  uuid.UUID
	PartnerID uuid.UUID
	ListingID uuid.UUID
	Adult     uuid.UUID
	Child     uuid.UUID
	SlotID    uuid.UUID
}

func newOpsEnv(t *testing.T) *opsEnv {
	t.Helper()
	ctx := context.Background()
	pool := testutil.Postgres(t)
	clk := &clock.Fixed{T: opsNow}
	iss := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk)
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
	paySvc := payment.NewService(pool, clk, fake, slog.New(slog.DiscardHandler))
	ledgerSvc := ledger.NewService(pool, clk)
	workers := river.NewWorkers()
	river.AddWorker(workers, &mailer.SendEmailWorker{Mailer: &mailer.Recorder{}})
	jobClient, err := jobs.NewClient(pool, workers, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	paySvc.SetOps(payment.OpsDeps{Jobs: jobClient, Users: idSvc, Files: fileSvc})
	bSvc := booking.NewService(booking.Deps{
		Pool: pool, Clock: clk, Listings: lSvc, Catalog: cat, Settings: settings.NewService(pool, clk),
		Identity: idSvc, Payments: paySvc, Ledger: ledgerSvc, Jobs: jobClient, Log: slog.New(slog.DiscardHandler),
	})
	mux := http.NewServeMux()
	bSvc.CustomerRoutes(mux, iss)
	paySvc.Routes(mux, payment.RouteDeps{Handler: bSvc, ClientIP: httpx.ClientIPFunc(0), AppEnv: "test"})
	paySvc.AdminRoutes(mux, iss)
	mux.HandleFunc("/", httpx.NotFound)
	e := &opsEnv{
		Pool: pool, Clock: clk, Issuer: iss, Fake: fake, Payments: paySvc, Booking: bSvc, Ledger: ledgerSvc,
		Files: fileSvc, Handler: httpx.Chain(mux, httpx.RequestID, httpx.Recover),
	}
	require.NoError(t, db.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		e.Admin, err = idSvc.CreateInternalUser(ctx, tx, "admin@lunain.vn", "adminpass123", auth.RoleSuperAdmin)
		return err
	}))
	_, err = cat.CreateCommissionRate(ctx, e.Admin, "attraction_ticket", 1000, nil)
	require.NoError(t, err)

	p, err := pSvc.Create(ctx, e.Admin, partner.CreateInput{
		LegalName: "Công ty Alpha", DisplayName: "Alpha", OwnerEmail: "owner@alpha.vn", OwnerPassword: "matkhau123",
		ServiceTypes: []string{"attraction_ticket"},
	})
	require.NoError(t, err)
	e.PartnerID = p.ID
	var province, ward string
	require.NoError(t, pool.QueryRow(ctx, `SELECT province_code, code FROM wards ORDER BY code LIMIT 1`).Scan(&province, &ward))
	l, err := lSvc.Create(ctx, p.ID, p.OwnerUserID, listing.CreateInput{ServiceType: "attraction_ticket", Title: "Vé tham quan"})
	require.NoError(t, err)
	desc, addr := "Mô tả chi tiết", "1 Đường A"
	_, err = lSvc.Update(ctx, p.ID, p.OwnerUserID, l.ID, listing.UpdateInput{
		Description: &desc, Address: &addr, ProvinceCode: &province, WardCode: &ward,
	})
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	f, err := fileSvc.Upload(ctx, auth.Principal{UserID: p.OwnerUserID, Role: auth.RolePartner}, "listing_image", &buf)
	require.NoError(t, err)
	_, _, err = lSvc.SetImages(ctx, p.ID, p.OwnerUserID, l.ID, []uuid.UUID{f.ID}, nil)
	require.NoError(t, err)
	adult, err := lSvc.CreateTicketType(ctx, p.ID, p.OwnerUserID, l.ID,
		listing.TicketTypeInput{Name: "Vé vào cổng", Audience: "adult", Price: 150_000})
	require.NoError(t, err)
	child, err := lSvc.CreateTicketType(ctx, p.ID, p.OwnerUserID, l.ID,
		listing.TicketTypeInput{Name: "Vé vào cổng", Audience: "child", Price: 80_000, SortOrder: 1})
	require.NoError(t, err)
	slot, err := lSvc.CreateTimeSlot(ctx, p.ID, p.OwnerUserID, l.ID,
		listing.TimeSlotInput{StartTime: "08:00", EndTime: "17:00", Weekdays: 0b1111111})
	require.NoError(t, err)
	_, err = lSvc.SetCancellationPolicy(ctx, p.ID, p.OwnerUserID, l.ID, []listing.CancellationTier{
		{HoursBefore: 48, RefundBps: 10000}, {HoursBefore: 24, RefundBps: 5000},
	})
	require.NoError(t, err)
	_, err = lSvc.Submit(ctx, p.ID, p.OwnerUserID, l.ID)
	require.NoError(t, err)
	_, err = lSvc.Approve(ctx, e.Admin, l.ID)
	require.NoError(t, err)
	e.ListingID, e.Adult, e.Child, e.SlotID = l.ID, adult.ID, child.ID, slot.ID

	e.Customer = uuid.Must(uuid.NewV7())
	e.exec(t, `INSERT INTO users (id, email, role, email_verified_at) VALUES ($1, $2, 'customer', $3)`,
		e.Customer, customerEmail, opsNow)
	e.exec(t, `INSERT INTO customer_profiles (user_id) VALUES ($1)`, e.Customer)
	return e
}

func (e *opsEnv) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	_, err := e.Pool.Exec(context.Background(), q, args...)
	require.NoError(t, err)
}

func (e *opsEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.Pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func (e *opsEnv) str(t *testing.T, q string, args ...any) string {
	t.Helper()
	var s string
	require.NoError(t, e.Pool.QueryRow(context.Background(), q, args...).Scan(&s))
	return s
}

// emailsTo counts the queued emails to one address.
func (e *opsEnv) emailsTo(t *testing.T, to string) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM river_job WHERE kind = 'send_email' AND args->>'to' = $1`, to)
}

func (e *opsEnv) audits(t *testing.T, action string, target uuid.UUID) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM admin_audit_log WHERE action = $1 AND target_id = $2`, action, target)
}

// book creates a 380.000 booking (visit date given) and returns it with the
// order code of its pending payment.
func (e *opsEnv) book(t *testing.T, key, visit string) (booking.BookingView, int64) {
	t.Helper()
	v, err := e.Booking.CreateAttraction(context.Background(), e.Customer, key, booking.CreateAttractionInput{
		ListingID: e.ListingID, VisitDate: visit, TimeSlotID: e.SlotID,
		Items:       []booking.TicketLine{{TicketTypeID: e.Adult, Quantity: 2}, {TicketTypeID: e.Child, Quantity: 1}},
		ContactName: "Nguyễn Văn An", ContactPhone: "+84 912 345 678", ContactEmail: "An@Gmail.com",
	})
	require.NoError(t, err)
	require.Equal(t, opsGross, v.GrossAmount)
	var oc int64
	require.NoError(t, e.Pool.QueryRow(context.Background(), `SELECT p.provider_order_code FROM payments p
		JOIN bookings b ON b.checkout_id = p.checkout_id WHERE b.id = $1 AND p.status = 'pending'`, v.ID).Scan(&oc))
	return v, oc
}

// pay posts a signed webhook of amount (reference FAKE-REF-<oc>).
func (e *opsEnv) pay(t *testing.T, oc, amount int64) {
	t.Helper()
	rec := e.webhook(t, e.Fake.MarkPaid(oc, amount))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func (e *opsEnv) webhook(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, payment.WebhookPath, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	e.Handler.ServeHTTP(rec, req)
	return rec
}

// cancelledRefund books, pays and cancels (more than 48h ahead) a booking:
// a 380.000 customer_cancel refund, pending with the saved account when
// withAccount, else awaiting_bank_info.
func (e *opsEnv) cancelledRefund(t *testing.T, key string, withAccount bool) payment.Refund {
	t.Helper()
	ctx := context.Background()
	if withAccount {
		_, err := e.Payments.SaveRefundAccount(ctx, e.Customer, payment.BankAccount{
			BankBIN: "970436", AccountNumber: "0011001234567", AccountHolder: "Nguyễn Văn An",
		})
		require.NoError(t, err)
	}
	v, oc := e.book(t, key, opsVisitOver48h)
	e.pay(t, oc, opsGross)
	_, err := e.Booking.CancelByCustomer(ctx, e.Customer, v.ID, nil)
	require.NoError(t, err)
	var id uuid.UUID
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT id FROM refunds WHERE booking_id = $1`, v.ID).Scan(&id))
	r, err := e.Payments.AdminGetRefund(ctx, id)
	require.NoError(t, err)
	require.Equal(t, payment.RefundCustomerCancel, r.Reason)
	return r
}

// paymentID is the id of the payment with order code oc.
func (e *opsEnv) paymentID(t *testing.T, oc int64) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT id FROM payments WHERE provider_order_code = $1`, oc).Scan(&id))
	return id
}

func (e *opsEnv) token(t *testing.T, userID uuid.UUID, role auth.Role) string {
	t.Helper()
	s, _, err := e.Issuer.Issue(auth.Principal{UserID: userID, Role: role})
	require.NoError(t, err)
	return s
}

func (e *opsEnv) do(t *testing.T, method, path, tok, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	e.Handler.ServeHTTP(rec, req)
	return rec
}

func requireAppErr(t *testing.T, err error, status int, code string) {
	t.Helper()
	require.Error(t, err)
	var ae *httpx.AppError
	require.True(t, errors.As(err, &ae), "not an AppError: %v", err)
	require.Equal(t, status, ae.Status, err.Error())
	require.Equal(t, code, ae.Code)
}

func requireHTTPCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
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

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
