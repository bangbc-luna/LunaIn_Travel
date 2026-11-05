package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/booking/store"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// ListingRef identifies the booked listing.
type ListingRef struct {
	ID    uuid.UUID `json:"id"`
	Title string    `json:"title"`
	Slug  string    `json:"slug"`
}

// SlotView is the booked time slot ("HH:MM", Vietnam time).
type SlotView struct {
	ID        uuid.UUID `json:"id"`
	StartTime string    `json:"start_time"`
	EndTime   string    `json:"end_time"`
}

// ItemView is one booking line.
type ItemView struct {
	ID           uuid.UUID  `json:"id"`
	TicketTypeID *uuid.UUID `json:"ticket_type_id"`
	Description  string     `json:"description"`
	UnitPrice    int64      `json:"unit_price"`
	Quantity     float64    `json:"quantity"`
	LineTotal    int64      `json:"line_total"`
}

// ContactView is who the partner contacts about the booking.
type ContactView struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
	Email string `json:"email"`
}

// PaymentView is the payment link of a pending_payment booking.
type PaymentView struct {
	CheckoutURL *string   `json:"checkout_url"`
	QRCode      *string   `json:"qr_code"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// BookingView is one booking as its customer, its partner or the admin sees
// it. QRToken is only ever set for the owning customer of a confirmed or
// completed booking (omitted otherwise); Payment only for the owning
// customer while pending_payment;
// CommissionRateBps/CommissionAmount only for the partner and the admin.
type BookingView struct {
	ID                 uuid.UUID                  `json:"id"`
	Code               string                     `json:"code"`
	Status             string                     `json:"status"`
	ServiceType        string                     `json:"service_type"`
	Listing            ListingRef                 `json:"listing"`
	VisitDate          string                     `json:"visit_date"`
	TimeSlot           SlotView                   `json:"time_slot"`
	ServiceStartAt     time.Time                  `json:"service_start_at"`
	ServiceEndAt       time.Time                  `json:"service_end_at"`
	Items              []ItemView                 `json:"items"`
	GrossAmount        int64                      `json:"gross_amount"`
	CommissionRateBps  *int                       `json:"commission_rate_bps,omitempty"`
	CommissionAmount   *int64                     `json:"commission_amount,omitempty"`
	Contact            ContactView                `json:"contact"`
	Note               *string                    `json:"note"`
	HoldExpiresAt      *time.Time                 `json:"hold_expires_at"`
	Payment            *PaymentView               `json:"payment"`
	QRToken            *string                    `json:"qr_token,omitempty"`
	CancellationPolicy []listing.CancellationTier `json:"cancellation_policy"`
	CanCancel          bool                       `json:"can_cancel"`
	CanReschedule      bool                       `json:"can_reschedule"`
	ConfirmedAt        *time.Time                 `json:"confirmed_at"`
	CancelledAt        *time.Time                 `json:"cancelled_at"`
	CreatedAt          time.Time                  `json:"created_at"`
}

// StatusEntry is one booking_status_history row.
type StatusEntry struct {
	FromStatus  *string    `json:"from_status"`
	ToStatus    string     `json:"to_status"`
	ActorKind   string     `json:"actor_kind"`
	ActorUserID *uuid.UUID `json:"actor_user_id"`
	Reason      *string    `json:"reason"`
	CreatedAt   time.Time  `json:"created_at"`
}

// AdminBookingView is BookingView plus who is involved and the history.
type AdminBookingView struct {
	BookingView
	CustomerID uuid.UUID     `json:"customer_id"`
	PartnerID  uuid.UUID     `json:"partner_id"`
	History    []StatusEntry `json:"status_history"`
}

// BookingSummary is one row of a booking list.
type BookingSummary struct {
	ID             uuid.UUID  `json:"id"`
	Code           string     `json:"code"`
	Status         string     `json:"status"`
	ServiceType    string     `json:"service_type"`
	Listing        ListingRef `json:"listing"`
	ServiceStartAt time.Time  `json:"service_start_at"`
	ServiceEndAt   time.Time  `json:"service_end_at"`
	GrossAmount    int64      `json:"gross_amount"`
	HoldExpiresAt  *time.Time `json:"hold_expires_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// ListFilter pages a booking list (newest first). Status is optional.
type ListFilter struct {
	Status *string
	Limit  int
	Cursor *uuid.UUID
}

// AdminFilter is ListFilter plus admin-only criteria.
type AdminFilter struct {
	ListFilter
	PartnerID  *uuid.UUID
	CustomerID *uuid.UUID
	Code       *string
}

type audience int

const (
	forCustomer audience = iota
	forPartner
	forAdmin
)

func validStatus(s string) bool {
	switch s {
	case "requested", "awaiting_payment", StatusPendingPayment, StatusConfirmed, "checked_in", "in_service",
		StatusCompleted, "no_show", "cancelled", "expired", "rejected":
		return true
	}
	return false
}

// load reads a booking and builds the view for aud; the caller has already
// checked who may see it.
func (s *Service) load(ctx context.Context, row store.GetBookingDetailRow, aud audience) (BookingView, error) {
	b := row.Booking
	q := store.New(s.pool)
	v := BookingView{
		ID: b.ID, Code: b.Code, Status: b.Status, ServiceType: b.ServiceType,
		Listing:        ListingRef{ID: b.ListingID, Title: row.ListingTitle, Slug: row.ListingSlug},
		ServiceStartAt: b.ServiceStartAt, ServiceEndAt: b.ServiceEndAt,
		GrossAmount: b.GrossAmount,
		Contact:     ContactView{Name: b.ContactName, Phone: b.ContactPhone, Email: b.ContactEmail},
		Note:        b.CustomerNote, HoldExpiresAt: b.HoldExpiresAt,
		ConfirmedAt: b.ConfirmedAt, CancelledAt: b.CancelledAt, CreatedAt: b.CreatedAt,
	}
	if err := json.Unmarshal(b.CancellationPolicySnapshot, &v.CancellationPolicy); err != nil {
		return v, fmt.Errorf("decode policy snapshot: %w", err)
	}
	if b.ServiceType == serviceTypeAttraction {
		d, err := q.GetAttractionDetail(ctx, b.ID)
		if err != nil {
			return v, fmt.Errorf("get attraction detail: %w", err)
		}
		v.VisitDate = d.VisitDate
		v.TimeSlot = SlotView{ID: d.TimeSlotID, StartTime: d.SlotStart, EndTime: d.SlotEnd}
	}
	items, err := q.ListBookingItems(ctx, b.ID)
	if err != nil {
		return v, fmt.Errorf("list booking items: %w", err)
	}
	v.Items = make([]ItemView, len(items))
	for i, it := range items {
		v.Items[i] = ItemView{
			ID: it.ID, TicketTypeID: it.TicketTypeID, Description: it.Description,
			UnitPrice: it.UnitPrice, Quantity: it.Quantity, LineTotal: it.LineTotal,
		}
	}
	// The payment link is the customer's only.
	if aud == forCustomer && b.Status == StatusPendingPayment {
		p, err := s.payments.PendingForCheckout(ctx, b.CheckoutID)
		if err != nil {
			return v, err
		}
		if p != nil {
			v.Payment = &PaymentView{CheckoutURL: p.CheckoutURL, QRCode: p.QRCode, ExpiresAt: p.ExpiresAt}
		}
	}
	switch aud {
	case forCustomer:
		if b.Status == StatusConfirmed || b.Status == StatusCompleted {
			tok := b.QrToken
			v.QRToken = &tok
		}
	case forPartner, forAdmin:
		rate, amount := int(b.CommissionRateBps), b.CommissionAmount
		v.CommissionRateBps, v.CommissionAmount = &rate, &amount
	}
	// The same predicates as CancelByCustomer and Reschedule.
	v.CanCancel = canCancel(b, s.clock.Now())
	if b.Status == StatusConfirmed && b.ServiceType == serviceTypeAttraction {
		cfg, err := s.settings.Get(ctx)
		if err != nil {
			return v, err
		}
		msg, err := s.rescheduleState(ctx, q, b, cfg.RescheduleWindowDays, s.clock.Now())
		if err != nil {
			return v, err
		}
		v.CanReschedule = msg == ""
	}
	return v, nil
}

func (s *Service) detail(ctx context.Context, id uuid.UUID) (store.GetBookingDetailRow, error) {
	row, err := store.New(s.pool).GetBookingDetail(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrBookingNotFound
	}
	if err != nil {
		return row, fmt.Errorf("get booking: %w", err)
	}
	return row, nil
}

// GetMine returns the customer's own booking; any other booking is
// ErrBookingNotFound.
func (s *Service) GetMine(ctx context.Context, customerID, id uuid.UUID) (BookingView, error) {
	row, err := s.detail(ctx, id)
	if err != nil {
		return BookingView{}, err
	}
	if row.Booking.CustomerID != customerID {
		return BookingView{}, ErrBookingNotFound
	}
	return s.load(ctx, row, forCustomer)
}

// GetForPartner returns a booking of the partner (never its qr_token);
// another partner's booking is ErrBookingNotFound.
func (s *Service) GetForPartner(ctx context.Context, partnerID, id uuid.UUID) (BookingView, error) {
	row, err := s.detail(ctx, id)
	if err != nil {
		return BookingView{}, err
	}
	if row.Booking.PartnerID != partnerID {
		return BookingView{}, ErrBookingNotFound
	}
	return s.load(ctx, row, forPartner)
}

// AdminGet returns any booking with its status history (no qr_token).
func (s *Service) AdminGet(ctx context.Context, id uuid.UUID) (AdminBookingView, error) {
	row, err := s.detail(ctx, id)
	if err != nil {
		return AdminBookingView{}, err
	}
	v, err := s.load(ctx, row, forAdmin)
	if err != nil {
		return AdminBookingView{}, err
	}
	hist, err := store.New(s.pool).ListStatusHistory(ctx, id)
	if err != nil {
		return AdminBookingView{}, fmt.Errorf("list status history: %w", err)
	}
	out := AdminBookingView{
		BookingView: v, CustomerID: row.Booking.CustomerID, PartnerID: row.Booking.PartnerID,
		History: make([]StatusEntry, len(hist)),
	}
	for i, h := range hist {
		out.History[i] = StatusEntry{
			FromStatus: h.FromStatus, ToStatus: h.ToStatus, ActorKind: h.ActorKind,
			ActorUserID: h.ActorUserID, Reason: h.Reason, CreatedAt: h.CreatedAt,
		}
	}
	return out, nil
}

func (s *Service) list(ctx context.Context, p store.ListBookingsParams, f ListFilter) ([]BookingSummary, *uuid.UUID, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	limit = min(limit, maxPageSize)
	if f.Status != nil && !validStatus(*f.Status) {
		return nil, nil, httpx.ErrInvalidRequest
	}
	p.Status, p.Cursor, p.Lim = f.Status, f.Cursor, int32(limit+1)
	rows, err := store.New(s.pool).ListBookings(ctx, p)
	if err != nil {
		return nil, nil, fmt.Errorf("list bookings: %w", err)
	}
	var next *uuid.UUID
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1].ID
		next = &last
	}
	out := make([]BookingSummary, len(rows))
	for i, r := range rows {
		out[i] = BookingSummary{
			ID: r.ID, Code: r.Code, Status: r.Status, ServiceType: r.ServiceType,
			Listing:        ListingRef{ID: r.ListingID, Title: r.ListingTitle, Slug: r.ListingSlug},
			ServiceStartAt: r.ServiceStartAt, ServiceEndAt: r.ServiceEndAt,
			GrossAmount: r.GrossAmount, HoldExpiresAt: r.HoldExpiresAt, CreatedAt: r.CreatedAt,
		}
	}
	return out, next, nil
}

// ListMine pages the customer's bookings.
func (s *Service) ListMine(ctx context.Context, customerID uuid.UUID, f ListFilter) ([]BookingSummary, *uuid.UUID, error) {
	return s.list(ctx, store.ListBookingsParams{CustomerID: &customerID}, f)
}

// ListForPartner pages the partner's bookings.
func (s *Service) ListForPartner(ctx context.Context, partnerID uuid.UUID, f ListFilter) ([]BookingSummary, *uuid.UUID, error) {
	return s.list(ctx, store.ListBookingsParams{PartnerID: &partnerID}, f)
}

// AdminList pages all bookings.
func (s *Service) AdminList(ctx context.Context, f AdminFilter) ([]BookingSummary, *uuid.UUID, error) {
	return s.list(ctx, store.ListBookingsParams{PartnerID: f.PartnerID, CustomerID: f.CustomerID, Code: f.Code}, f.ListFilter)
}
