package booking_test

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

var (
	bookingCodeRe  = regexp.MustCompile(`^LT[0-9A-HJKMNP-TV-Z]{8}$`)
	checkoutCodeRe = regexp.MustCompile(`^CO[0-9A-HJKMNP-TV-Z]{8}$`)
)

func TestCreate_PendingWithMoneySnapshot(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v := e.create(t, "key-1")

	require.Regexp(t, bookingCodeRe, v.Code)
	require.Equal(t, "pending_payment", v.Status)
	require.Equal(t, int64(380_000), v.GrossAmount)
	require.Equal(t, e.L.ID, v.Listing.ID)
	require.Equal(t, e.L.Slug, v.Listing.Slug)
	require.Equal(t, visitDate, v.VisitDate)
	require.Equal(t, booking.SlotView{ID: e.Slot.ID, StartTime: "08:00", EndTime: "17:00"}, v.TimeSlot)
	require.Nil(t, v.QRToken, "no qr_token before confirmation")
	require.False(t, v.CanCancel)
	require.False(t, v.CanReschedule)
	require.Equal(t, []listing.CancellationTier{{HoursBefore: 48, RefundBps: 10000}, {HoursBefore: 24, RefundBps: 5000}}, v.CancellationPolicy)
	require.Len(t, v.Items, 2)
	require.Equal(t, "Vé vào cổng (Người lớn)", v.Items[0].Description)
	require.Equal(t, int64(150_000), v.Items[0].UnitPrice)
	require.Equal(t, float64(2), v.Items[0].Quantity)
	require.Equal(t, int64(300_000), v.Items[0].LineTotal)
	require.Equal(t, e.Adult.ID, *v.Items[0].TicketTypeID)
	require.Equal(t, "Vé vào cổng (Trẻ em)", v.Items[1].Description)
	require.Equal(t, int64(80_000), v.Items[1].LineTotal)

	// Money, hold and time snapshot on the row.
	var (
		gross, commission                  int64
		rateBps                            int
		holdExp, holdRel, start, end, orig time.Time
		snapshot                           string
		name, phone, email                 string
		checkoutID                         uuid.UUID
	)
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT gross_amount, commission_rate_bps, commission_amount,
		hold_expires_at, hold_release_at, service_start_at, service_end_at, original_service_start_at,
		cancellation_policy_snapshot::text, contact_name, contact_phone, contact_email, checkout_id
		FROM bookings WHERE id = $1`, v.ID).Scan(&gross, &rateBps, &commission, &holdExp, &holdRel, &start, &end, &orig,
		&snapshot, &name, &phone, &email, &checkoutID))
	require.Equal(t, int64(380_000), gross)
	require.Equal(t, 1000, rateBps)
	require.Equal(t, int64(38_000), commission)
	require.True(t, testNow.Add(15*time.Minute).Equal(holdExp), holdExp)
	require.True(t, testNow.Add(20*time.Minute).Equal(holdRel), holdRel)
	wantStart := time.Date(2026, 11, 7, 8, 0, 0, 0, clock.VN)
	require.True(t, wantStart.Equal(start), start)
	require.True(t, wantStart.Equal(orig), orig)
	require.True(t, time.Date(2026, 11, 7, 17, 0, 0, 0, clock.VN).Equal(end), end)
	require.JSONEq(t, `[{"hours_before":48,"refund_bps":10000},{"hours_before":24,"refund_bps":5000}]`, snapshot)
	require.Equal(t, "Nguyễn Văn An", name)
	require.Equal(t, "0912345678", phone)
	require.Equal(t, "an@gmail.com", email)
	require.NotNil(t, v.HoldExpiresAt)
	require.True(t, holdExp.Equal(*v.HoldExpiresAt))

	// Checkout, children and history.
	var coCode string
	var coTotal int64
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT code, total_amount FROM checkouts WHERE id = $1`, checkoutID).Scan(&coCode, &coTotal))
	require.Regexp(t, checkoutCodeRe, coCode)
	require.Equal(t, int64(380_000), coTotal)
	require.Equal(t, 2, e.count(t, `SELECT count(*) FROM booking_ticket_lines WHERE booking_id = $1`, v.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_attraction_details
		WHERE booking_id = $1 AND visit_date = '2026-11-07' AND time_slot_id = $2 AND slot_start = '08:00' AND slot_end = '17:00'`, v.ID, e.Slot.ID))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM booking_status_history
		WHERE booking_id = $1 AND from_status IS NULL AND to_status = 'pending_payment'
		AND actor_kind = 'customer' AND actor_user_id = $2`, v.ID, e.Customer))

	// A pending payment with the link created after commit.
	var (
		payStatus  string
		payAmount  int64
		payExpires time.Time
		orderCode  int64
		url        *string
	)
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT status, amount, expires_at, provider_order_code, checkout_url
		FROM payments WHERE checkout_id = $1`, checkoutID).Scan(&payStatus, &payAmount, &payExpires, &orderCode, &url))
	require.Equal(t, "pending", payStatus)
	require.Equal(t, int64(380_000), payAmount)
	require.True(t, holdExp.Equal(payExpires))
	require.NotNil(t, url)
	require.NotNil(t, v.Payment)
	require.Equal(t, *url, *v.Payment.CheckoutURL)
	require.NotNil(t, v.Payment.QRCode)
	req, ok := e.Fake.Request(orderCode)
	require.True(t, ok)
	desc, err := payment.DescriptionForCheckout(coCode)
	require.NoError(t, err)
	require.Equal(t, desc, req.Description)
	require.Equal(t, int64(380_000), req.Amount)
}

func TestCreate_Idempotent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first := e.create(t, "same-key")
	again := e.create(t, "same-key")
	require.Equal(t, first.ID, again.ID)
	require.Equal(t, first.Code, again.Code)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM bookings`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM checkouts`))
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments`))

	// Same key, different body: 422 and nothing new.
	other := e.input()
	other.Items[0].Quantity = 3
	_, err := e.Svc.CreateAttraction(ctx, e.Customer, "same-key", other)
	requireErr(t, err, http.StatusUnprocessableEntity, "idempotency_key_reused")
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM bookings`))

	// Keys are per customer.
	b := e.customer(t, "b@gmail.com")
	vb, err := e.Svc.CreateAttraction(ctx, b, "same-key", e.input())
	require.NoError(t, err)
	require.NotEqual(t, first.ID, vb.ID)

	// Two concurrent clicks with one key create one booking.
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() {
			v, err := e.Svc.CreateAttraction(ctx, e.Customer, "double-click", e.input())
			ids[i], errs[i] = v.ID, err
		})
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, ids[0], ids[1])
	require.Equal(t, 3, e.count(t, `SELECT count(*) FROM bookings`))

	// HTTP: missing header is 400; replay returns the same booking.
	tok := e.token(t, e.Customer, auth.RoleCustomer)
	body := map[string]any{
		"listing_id": e.L.ID, "visit_date": visitDate, "time_slot_id": e.Slot.ID,
		"items":        []map[string]any{{"ticket_type_id": e.Adult.ID, "quantity": 1}},
		"contact_name": "An", "contact_phone": "0912345678", "contact_email": "an@gmail.com",
	}
	requireCode(t, e.do(t, "POST", "/v1/bookings/attraction", tok, body), http.StatusBadRequest, "idempotency_key_required")
	rec := e.do(t, "POST", "/v1/bookings/attraction", tok, body, "Idempotency-Key", "http-key")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	r1 := decode[booking.BookingView](t, rec)
	rec = e.do(t, "POST", "/v1/bookings/attraction", tok, body, "Idempotency-Key", "http-key")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, r1.ID, decode[booking.BookingView](t, rec).ID)
	body["contact_name"] = "Bình"
	requireCode(t, e.do(t, "POST", "/v1/bookings/attraction", tok, body, "Idempotency-Key", "http-key"),
		http.StatusUnprocessableEntity, "idempotency_key_reused")
}

func TestCreate_NoCommissionRate422(t *testing.T) {
	e := newBareEnv(t)
	e.fixtureListing(t)
	e.Customer = e.customer(t, "khach@gmail.com")
	_, err := e.Svc.CreateAttraction(context.Background(), e.Customer, "k", e.input())
	requireErr(t, err, http.StatusUnprocessableEntity, "no_commission_rate")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM bookings`))
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM idempotency_keys`), "a failed request does not keep its key")
}

func TestCreate_PastDateOrStartedSlot422(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	in := e.input()
	in.VisitDate = "2026-11-04"
	_, err := e.Svc.CreateAttraction(ctx, e.Customer, "past", in)
	requireErr(t, err, http.StatusUnprocessableEntity, "visit_date_past")

	// Today's 08:00 slot started at 08:00; it is 09:00.
	in.VisitDate = "2026-11-05"
	_, err = e.Svc.CreateAttraction(ctx, e.Customer, "started", in)
	requireErr(t, err, http.StatusUnprocessableEntity, "slot_started")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM bookings`))
}

func TestCreate_ClosedDate422(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	require.NoError(t, e.Listing.AddClosedDate(ctx, e.P.ID, e.P.OwnerUserID, e.L.ID, time.Date(2026, 11, 7, 0, 0, 0, 0, clock.VN), nil))
	_, err := e.Svc.CreateAttraction(ctx, e.Customer, "closed", e.input())
	requireErr(t, err, http.StatusUnprocessableEntity, "slot_unavailable")

	// A slot id of no slot of this listing.
	in := e.input()
	in.VisitDate = "2026-11-08"
	in.TimeSlotID = uuid.Must(uuid.NewV7())
	_, err = e.Svc.CreateAttraction(ctx, e.Customer, "other-slot", in)
	requireErr(t, err, http.StatusUnprocessableEntity, "slot_unavailable")
}

func TestCreate_InactiveTicket422(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	inactive := false
	_, err := e.Listing.UpdateTicketType(ctx, e.P.ID, e.P.OwnerUserID, e.L.ID, e.Child.ID, listing.TicketTypePatch{Active: &inactive})
	require.NoError(t, err)
	_, err = e.Svc.CreateAttraction(ctx, e.Customer, "inactive", e.input())
	requireErr(t, err, http.StatusUnprocessableEntity, "invalid_ticket_type")
}

func TestCreate_NotBookable422(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	require.NoError(t, e.Partner.Suspend(ctx, e.Admin, e.P.ID, "Vi phạm chính sách"))
	_, err := e.Svc.CreateAttraction(ctx, e.Customer, "suspended", e.input())
	requireErr(t, err, http.StatusUnprocessableEntity, "listing_not_bookable")
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM bookings`))
}

func TestCreate_InvalidInput422(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	lines := func(n, qty int) []booking.TicketLine {
		out := make([]booking.TicketLine, n)
		for i := range out {
			out[i] = booking.TicketLine{TicketTypeID: uuid.Must(uuid.NewV7()), Quantity: qty}
		}
		return out
	}
	cases := map[string]func(in *booking.CreateAttractionInput){
		"no lines":       func(in *booking.CreateAttractionInput) { in.Items = nil },
		"11 lines":       func(in *booking.CreateAttractionInput) { in.Items = lines(11, 1) },
		"quantity 0":     func(in *booking.CreateAttractionInput) { in.Items[0].Quantity = 0 },
		"quantity 51":    func(in *booking.CreateAttractionInput) { in.Items[0].Quantity = 51 },
		"over 100":       func(in *booking.CreateAttractionInput) { in.Items = lines(3, 34) },
		"duplicate type": func(in *booking.CreateAttractionInput) { in.Items[1].TicketTypeID = in.Items[0].TicketTypeID },
		"empty name":     func(in *booking.CreateAttractionInput) { in.ContactName = "  " },
		"long name":      func(in *booking.CreateAttractionInput) { in.ContactName = strings.Repeat("á", 101) },
		"bad phone":      func(in *booking.CreateAttractionInput) { in.ContactPhone = "12345" },
		"bad email":      func(in *booking.CreateAttractionInput) { in.ContactEmail = "an@" },
		"bad date":       func(in *booking.CreateAttractionInput) { in.VisitDate = "07/11/2026" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := e.input()
			mutate(&in)
			_, err := e.Svc.CreateAttraction(ctx, e.Customer, "invalid-"+name, in)
			requireErr(t, err, http.StatusUnprocessableEntity, "invalid_booking")
		})
	}
	require.Equal(t, 0, e.count(t, `SELECT count(*) FROM bookings`))

	tok := e.token(t, e.Customer, auth.RoleCustomer)
	requireCode(t, e.do(t, "POST", "/v1/bookings/attraction", tok, `{"listing_id":"x","price":1}`, "Idempotency-Key", "k"),
		http.StatusBadRequest, "invalid_request")
}

func TestCreate_ProviderDownStillCreatesBooking(t *testing.T) {
	e := newEnv(t)
	e.Fake.FailCreateLink(errors.New("payos down"))
	v := e.create(t, "down")
	require.Equal(t, "pending_payment", v.Status)
	require.NotNil(t, v.Payment)
	require.Nil(t, v.Payment.CheckoutURL)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments WHERE status = 'pending' AND checkout_url IS NULL`))

	tok := e.token(t, e.Customer, auth.RoleCustomer)
	requireCode(t, e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/pay", tok, nil),
		http.StatusBadGateway, "payment_provider_unavailable")

	e.Fake.FailCreateLink(nil)
	rec := e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/pay", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decode[booking.BookingView](t, rec)
	require.NotNil(t, got.Payment)
	require.NotNil(t, got.Payment.CheckoutURL)
	require.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments WHERE status = 'pending' AND checkout_url IS NOT NULL`))

	// Paying again returns the same link.
	rec = e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/pay", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, *got.Payment.CheckoutURL, *decode[booking.BookingView](t, rec).Payment.CheckoutURL)

	// Once the hold expired there is nothing to pay.
	e.Clock.Advance(16 * time.Minute)
	tok = e.token(t, e.Customer, auth.RoleCustomer) // the access token TTL is 15 minutes
	requireCode(t, e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/pay", tok, nil), http.StatusConflict, "hold_expired")
	e.confirm(t, v.ID)
	requireCode(t, e.do(t, "POST", "/v1/bookings/"+v.ID.String()+"/pay", tok, nil), http.StatusConflict, "booking_not_payable")
}

func TestCreate_FillsEmptyProfile(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.create(t, "profile")
	var name, phone *string
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT full_name, phone FROM customer_profiles WHERE user_id = $1`, e.Customer).Scan(&name, &phone))
	require.NotNil(t, name)
	require.Equal(t, "Nguyễn Văn An", *name)
	require.NotNil(t, phone)
	require.Equal(t, "0912345678", *phone)

	// A filled profile is not overwritten.
	in := e.input()
	in.ContactName, in.ContactPhone = "Người Khác", "0987654321"
	_, err := e.Svc.CreateAttraction(ctx, e.Customer, "profile-2", in)
	require.NoError(t, err)
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT full_name, phone FROM customer_profiles WHERE user_id = $1`, e.Customer).Scan(&name, &phone))
	require.Equal(t, "Nguyễn Văn An", *name)
	require.Equal(t, "0912345678", *phone)
}
