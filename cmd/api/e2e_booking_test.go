package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
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
	"github.com/bangbc-luna/lunain-travel/internal/payout"
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

// e2eApp is the real mux wired like main (payment.Fake, BookingGuard,
// release, no-show and complete-rides workers, payment ops, payout and reconcile services) over a fresh Postgres with a movable clock. The job client
// is never started: emails stay in river_job, where the test reads the
// verification code.
type e2eApp struct {
	handler  http.Handler
	pool     *pgxpool.Pool
	clock    *clock.Fixed
	ledger   *ledger.Service
	release  *booking.ReleaseWorker
	noShow   *booking.NoShowWorker
	complete *booking.CompleteRidesWorker
}

func newE2EApp(t *testing.T, now time.Time) *e2eApp {
	t.Helper()
	pool := testutil.Postgres(t)
	clk := &clock.Fixed{T: now}
	workers := river.NewWorkers()
	river.AddWorker(workers, &mailer.SendEmailWorker{Mailer: &mailer.Recorder{}})
	jobClient, err := jobs.NewClient(pool, workers, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	issuer := auth.NewIssuer([]byte("test-secret-test-secret-test-secret"), 15*time.Minute, clk)
	idSvc := identity.NewService(identity.Deps{
		Pool: pool, Jobs: jobClient, Issuer: issuer, Clock: clk,
		CodePepper: []byte("test-pepper"), RefreshTTL: 30 * 24 * time.Hour,
	})
	fileSvc := files.NewService(pool, storage.NewMemory(), clk, "http://files.test")
	geoSvc := geo.NewService(pool)
	catSvc := catalog.NewService(pool, clk)
	partnerSvc := partner.NewService(partner.Deps{
		Pool: pool, Clock: clk, Users: idSvc, Catalog: catSvc, Geo: geoSvc, Files: fileSvc,
	})
	listingSvc := listing.NewService(listing.Deps{
		Pool: pool, Clock: clk, Partners: partnerSvc, Catalog: catSvc, Geo: geoSvc, Files: fileSvc,
		BookingGuard: booking.Guard{},
	})
	settingsSvc := settings.NewService(pool, clk)
	fake := payment.NewFake()
	fake.Now = clk.Now
	paymentSvc := payment.NewService(pool, clk, fake, nil)
	ledgerSvc := ledger.NewService(pool, clk)
	bookingSvc := booking.NewService(booking.Deps{
		Pool: pool, Clock: clk, Listings: listingSvc, Catalog: catSvc, Settings: settingsSvc,
		Identity: idSvc, Payments: paymentSvc, Ledger: ledgerSvc, Partners: partnerSvc, Jobs: jobClient,
		Log: slog.New(slog.DiscardHandler),
	})
	paymentSvc.SetOps(payment.OpsDeps{Jobs: jobClient, Users: idSvc, Files: fileSvc})
	payoutSvc := payout.NewService(payout.Deps{
		Pool: pool, Clock: clk, Ledger: ledgerSvc, Partners: partnerSvc, Settings: settingsSvc,
		Files: fileSvc, Jobs: jobClient, Log: slog.New(slog.DiscardHandler),
	})
	reconcileSvc := reconcile.NewService(reconcile.Deps{
		Pool: pool, Clock: clk, Payments: paymentSvc, Ledger: ledgerSvc, Handler: bookingSvc,
		Identity: idSvc, Jobs: jobClient, Log: slog.New(slog.DiscardHandler),
	})
	mux, _ := buildMux(muxDeps{
		Pool: pool, Issuer: issuer, ClientIP: httpx.ClientIPFunc(0),
		Identity: idSvc, Settings: settingsSvc, Files: fileSvc,
		Geo: geoSvc, Catalog: catSvc, Partner: partnerSvc, Listing: listingSvc,
		Ledger: ledgerSvc, Payment: paymentSvc, Booking: bookingSvc, Payout: payoutSvc, Reconcile: reconcileSvc,
		AppEnv: "test",
	})
	require.NoError(t, db.InTx(context.Background(), pool, func(tx pgx.Tx) error {
		_, err := idSvc.CreateInternalUser(context.Background(), tx, "admin@lunain.vn", "adminpass123", auth.RoleSuperAdmin)
		return err
	}))
	return &e2eApp{
		handler: httpx.Chain(mux, httpx.RequestID, httpx.Recover), pool: pool, clock: clk,
		ledger: ledgerSvc, release: &booking.ReleaseWorker{Service: bookingSvc},
		noShow: &booking.NoShowWorker{Service: bookingSvc}, complete: &booking.CompleteRidesWorker{Service: bookingSvc},
	}
}

// call sends a JSON request (body nil = none) and requires status; it
// decodes the response into out when out is not nil.
func (a *e2eApp) call(t *testing.T, method, path, tok string, body any, status int, out any, headers ...string) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	a.send(t, req, tok, status, out, headers...)
}

func (a *e2eApp) send(t *testing.T, req *http.Request, tok string, status int, out any, headers ...string) {
	t.Helper()
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	require.Equal(t, status, rec.Code, "%s %s: %s", req.Method, req.URL.Path, rec.Body.String())
	if out != nil {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), out), rec.Body.String())
	}
}

func (a *e2eApp) login(t *testing.T, email, password string) string {
	t.Helper()
	var tk identity.Tokens
	a.call(t, "POST", "/v1/auth/login", "", map[string]string{"email": email, "password": password}, http.StatusOK, &tk)
	return tk.AccessToken
}

func (a *e2eApp) balance(t *testing.T, tok string) ledger.Balance {
	t.Helper()
	var b ledger.Balance
	a.call(t, "GET", "/v1/partner/balance", tok, nil, http.StatusOK, &b)
	return b
}

var (
	sixDigits = regexp.MustCompile(`\b\d{6}\b`)
	fakePayOC = regexp.MustCompile(`/fake-pay/(\d+)$`)
)

// orderCode is the order code in the Fake checkout URL of a pending booking.
func orderCode(t *testing.T, v booking.BookingView) string {
	t.Helper()
	require.NotNil(t, v.Payment)
	require.NotNil(t, v.Payment.CheckoutURL)
	m := fakePayOC.FindStringSubmatch(*v.Payment.CheckoutURL)
	require.Len(t, m, 2, *v.Payment.CheckoutURL)
	return m[1]
}

const (
	e2eAdminEmail, e2eAdminPass       = "admin@lunain.vn", "adminpass123"
	e2eOwnerEmail, e2eOwnerPass       = "owner@alpha.vn", "matkhau123"
	e2eCustomerEmail, e2eCustomerPass = "khach@gmail.com", "Passw0rd123"
)

// e2eWorld is the setup the e2e flows share: an admin, a partner (commission
// 10%) with an approved attraction listing (adult 150.000, child 80.000, one
// daily slot, refund 100% from 48h and 50% from 24h) and a verified customer.
// The tokens are refreshed by relogin.
type e2eWorld struct {
	admin, pt, ct string
	partner       partner.Partner
	listing       listing.Listing
	slot          listing.TimeSlot
	adult, child  listing.TicketType
}

// uploadPNG uploads a 1×1 PNG for purpose through POST /v1/files.
func (a *e2eApp) uploadPNG(t *testing.T, tok, purpose string) uuid.UUID {
	t.Helper()
	var img bytes.Buffer
	require.NoError(t, png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	require.NoError(t, mw.WriteField("purpose", purpose))
	fw, err := mw.CreateFormFile("file", "a.png")
	require.NoError(t, err)
	_, err = fw.Write(img.Bytes())
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req := httptest.NewRequest("POST", "/v1/files", &form)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var f struct {
		ID uuid.UUID `json:"id"`
	}
	a.send(t, req, tok, http.StatusCreated, &f)
	return f.ID
}

// setupWorld builds the e2eWorld through the API only.
func (a *e2eApp) setupWorld(t *testing.T) *e2eWorld {
	t.Helper()
	ctx := context.Background()
	w := &e2eWorld{}

	// Admin sets the commission (1000 bps) and creates the partner.
	w.admin = a.login(t, e2eAdminEmail, e2eAdminPass)
	a.call(t, "POST", "/v1/admin/commission-rates", w.admin,
		map[string]any{"service_type": "attraction_ticket", "rate_bps": 1000}, http.StatusCreated, nil)
	a.call(t, "POST", "/v1/admin/partners", w.admin, map[string]any{
		"legal_name": "Công ty Alpha", "display_name": "Alpha",
		"owner_email": e2eOwnerEmail, "owner_password": e2eOwnerPass,
		"service_types": []string{"attraction_ticket"},
	}, http.StatusCreated, &w.partner)
	w.pt = a.login(t, e2eOwnerEmail, e2eOwnerPass)

	// The partner builds an attraction listing and submits it; the admin
	// approves it.
	var province, ward string
	require.NoError(t, a.pool.QueryRow(ctx, `SELECT province_code, code FROM wards ORDER BY code LIMIT 1`).Scan(&province, &ward))
	a.call(t, "POST", "/v1/partner/listings", w.pt,
		map[string]any{"service_type": "attraction_ticket", "title": "Vé tham quan Bà Nà"}, http.StatusCreated, &w.listing)
	lp := "/v1/partner/listings/" + w.listing.ID.String()
	a.call(t, "PATCH", lp, w.pt, map[string]any{
		"description": "Mô tả chi tiết", "address": "1 Đường A", "province_code": province, "ward_code": ward,
	}, http.StatusOK, nil)
	img := a.uploadPNG(t, w.pt, "listing_image")
	a.call(t, "PUT", lp+"/images", w.pt, map[string]any{"file_ids": []uuid.UUID{img}}, http.StatusOK, nil)
	a.call(t, "POST", lp+"/ticket-types", w.pt,
		map[string]any{"name": "Vé vào cổng", "audience": "adult", "price": 150_000}, http.StatusCreated, &w.adult)
	a.call(t, "POST", lp+"/ticket-types", w.pt,
		map[string]any{"name": "Vé vào cổng", "audience": "child", "price": 80_000, "sort_order": 1}, http.StatusCreated, &w.child)
	a.call(t, "POST", lp+"/time-slots", w.pt,
		map[string]any{"start_time": "08:00", "end_time": "17:00", "weekdays": 0b1111111}, http.StatusCreated, &w.slot)
	a.call(t, "PUT", lp+"/cancellation-policy", w.pt, map[string]any{"tiers": []map[string]int{
		{"hours_before": 48, "refund_bps": 10000}, {"hours_before": 24, "refund_bps": 5000},
	}}, http.StatusOK, nil)
	a.call(t, "POST", lp+"/submit", w.pt, nil, http.StatusOK, nil)
	a.call(t, "POST", "/v1/admin/listings/"+w.listing.ID.String()+"/approve", w.admin, nil, http.StatusOK, nil)

	// A customer registers and verifies the email.
	a.call(t, "POST", "/v1/auth/register", "",
		map[string]string{"email": e2eCustomerEmail, "password": e2eCustomerPass}, http.StatusAccepted, nil)
	var mail string
	require.NoError(t, a.pool.QueryRow(ctx, `SELECT args->>'text' FROM river_job
		WHERE kind = 'send_email' AND args->>'to' = $1 ORDER BY id DESC LIMIT 1`, e2eCustomerEmail).Scan(&mail))
	code := sixDigits.FindString(mail)
	require.NotEmpty(t, code, mail)
	var tk identity.Tokens
	a.call(t, "POST", "/v1/auth/verify-email", "",
		map[string]string{"email": e2eCustomerEmail, "code": code, "password": e2eCustomerPass}, http.StatusOK, &tk)
	w.ct = tk.AccessToken
	return w
}

// relogin refreshes the access tokens: they live 15 minutes, so call it after
// every clock jump.
func (a *e2eApp) relogin(t *testing.T, w *e2eWorld) {
	t.Helper()
	w.admin = a.login(t, e2eAdminEmail, e2eAdminPass)
	w.pt = a.login(t, e2eOwnerEmail, e2eOwnerPass)
	w.ct = a.login(t, e2eCustomerEmail, e2eCustomerPass)
}

// book creates an attraction booking as the customer with Idempotency-Key key.
func (a *e2eApp) book(t *testing.T, w *e2eWorld, key, visit string, items []map[string]any) booking.BookingView {
	t.Helper()
	var v booking.BookingView
	a.call(t, "POST", "/v1/bookings/attraction", w.ct, map[string]any{
		"listing_id": w.listing.ID, "visit_date": visit, "time_slot_id": w.slot.ID, "items": items,
		"contact_name": "Nguyễn Văn An", "contact_phone": "0912345678", "contact_email": e2eCustomerEmail,
	}, http.StatusCreated, &v, "Idempotency-Key", key)
	return v
}

// TestE2E_AttractionBooking runs the whole booking flow through the real mux
// with payment.Fake: setup, book, pay, check in, release; plus a second
// booking cancelled more than 48h ahead (100% refund, commission kept).
func TestE2E_AttractionBooking(t *testing.T) {
	// Thursday 2026-11-05 09:00 in Vietnam.
	a := newE2EApp(t, time.Date(2026, 11, 5, 9, 0, 0, 0, clock.VN))
	ctx := context.Background()

	// 1–3. Admin, partner, approved listing and verified customer.
	w := a.setupWorld(t)
	p, adult, child := w.partner, w.adult, w.child
	lp := "/v1/partner/listings/" + w.listing.ID.String()

	// The customer books (Idempotency-Key).
	items := []map[string]any{{"ticket_type_id": adult.ID, "quantity": 2}, {"ticket_type_id": child.ID, "quantity": 1}}
	const gross, commission = int64(380_000), int64(38_000) // 2×150.000 + 80.000, 10%
	v := a.book(t, w, "e2e-1", "2026-11-07", items)
	require.Equal(t, booking.StatusPendingPayment, v.Status)
	require.Equal(t, gross, v.GrossAmount)
	again := a.book(t, w, "e2e-1", "2026-11-07", items)
	require.Equal(t, v.ID, again.ID, "same Idempotency-Key, same booking")
	var n int
	require.NoError(t, a.pool.QueryRow(ctx, `SELECT count(*) FROM bookings`).Scan(&n))
	require.Equal(t, 1, n)

	// The partner cannot close the booked date (BookingGuard).
	a.call(t, "PUT", lp+"/closed-dates/2026-11-07", w.pt, nil, http.StatusConflict, nil)

	// 4. The Fake webhook reports the payment: the booking is confirmed.
	a.call(t, "POST", "/v1/dev/payments/"+orderCode(t, v)+"/simulate-paid", "", map[string]any{}, http.StatusOK, nil)
	bp := "/v1/bookings/" + v.ID.String()
	a.call(t, "GET", bp, w.ct, nil, http.StatusOK, &v)
	require.Equal(t, booking.StatusConfirmed, v.Status)
	require.NotNil(t, v.QRToken)
	require.Equal(t, ledger.Balance{Pending: gross - commission}, a.balance(t, w.pt))

	// 5. On the visit date the partner checks in by QR: completed.
	a.clock.T = time.Date(2026, 11, 7, 8, 30, 0, 0, clock.VN)
	a.relogin(t, w)
	var res booking.CheckInResult
	a.call(t, "POST", "/v1/partner/check-ins", w.pt, map[string]string{"qr_token": *v.QRToken}, http.StatusOK, &res)
	require.False(t, res.Already)
	require.Equal(t, booking.StatusCompleted, res.Booking.Status)
	a.call(t, "GET", bp, w.ct, nil, http.StatusOK, &v)
	require.Equal(t, booking.StatusCompleted, v.Status)

	// 6. Before the release delay nothing moves; after it the ReleaseWorker
	// makes gross − commission available.
	require.NoError(t, a.release.Work(ctx, nil))
	require.Equal(t, ledger.Balance{Pending: gross - commission}, a.balance(t, w.pt))
	a.clock.T = a.clock.T.Add(25 * time.Hour) // release_delay_days = 1
	a.relogin(t, w)
	require.NoError(t, a.release.Work(ctx, nil))
	require.Equal(t, ledger.Balance{Available: gross - commission}, a.balance(t, w.pt))

	// Cancel branch: another booking cancelled more than 48h ahead gets a
	// 100% refund; the commission is kept, so the partner nets −commission.
	const gross2, commission2 = int64(150_000), int64(15_000)
	v2 := a.book(t, w, "e2e-2", "2026-11-20", []map[string]any{{"ticket_type_id": adult.ID, "quantity": 1}})
	require.Equal(t, gross2, v2.GrossAmount)
	a.call(t, "POST", "/v1/dev/payments/"+orderCode(t, v2)+"/simulate-paid", "", map[string]any{}, http.StatusOK, nil)
	require.Equal(t, ledger.Balance{Pending: gross2 - commission2, Available: gross - commission}, a.balance(t, w.pt))
	a.call(t, "POST", "/v1/bookings/"+v2.ID.String()+"/cancel", w.ct, nil, http.StatusOK, &v2)
	require.Equal(t, booking.StatusCancelled, v2.Status)
	var refunds struct {
		Items []payment.Refund `json:"items"`
	}
	a.call(t, "GET", "/v1/refunds", w.ct, nil, http.StatusOK, &refunds)
	require.Len(t, refunds.Items, 1)
	require.Equal(t, gross2, refunds.Items[0].Amount)
	require.Equal(t, v2.ID, *refunds.Items[0].BookingID)
	require.Equal(t, ledger.Balance{Available: gross - commission - commission2}, a.balance(t, w.pt))

	require.NoError(t, a.ledger.Verify(ctx, p.ID))
	var platform int64
	require.NoError(t, a.pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::bigint FROM platform_commission_entries`).Scan(&platform))
	require.Equal(t, commission+commission2, platform)
}
