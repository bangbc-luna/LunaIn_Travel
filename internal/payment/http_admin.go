package payment

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// adminRefund is a refund as the Super Admin sees it.
type adminRefund struct {
	Refund
	CheckoutID        uuid.UUID  `json:"checkout_id"`
	ProviderReference *string    `json:"provider_reference"`
	ProofFileID       *uuid.UUID `json:"proof_file_id"`
	CompletedBy       *uuid.UUID `json:"completed_by"`
}

func toAdminRefund(r Refund) adminRefund {
	return adminRefund{
		Refund: r, CheckoutID: r.CheckoutID, ProviderReference: r.ProviderReference,
		ProofFileID: r.ProofFileID, CompletedBy: r.CompletedBy,
	}
}

func toAdminRefunds(rs []Refund) []adminRefund {
	out := make([]adminRefund, len(rs))
	for i, r := range rs {
		out[i] = toAdminRefund(r)
	}
	return out
}

// adminPaymentJSON is a payment as the Super Admin sees it (review_state
// comes from the embedded Payment).
type adminPaymentJSON struct {
	Payment
	CheckoutID        uuid.UUID  `json:"checkout_id"`
	ProviderReference *string    `json:"provider_reference"`
	PaidAt            *time.Time `json:"paid_at"`
	CreatedAt         time.Time  `json:"created_at"`
	ReviewSeenAt      *time.Time `json:"review_seen_at"`
	ReviewSeenBy      *uuid.UUID `json:"review_seen_by"`
	ReviewResolvedAt  *time.Time `json:"review_resolved_at"`
	ReviewResolvedBy  *uuid.UUID `json:"review_resolved_by"`
	ReviewNote        *string    `json:"review_note"`
}

func toAdminPayment(p Payment) adminPaymentJSON {
	return adminPaymentJSON{
		Payment: p, CheckoutID: p.CheckoutID, ProviderReference: p.ProviderReference, PaidAt: p.PaidAt,
		CreatedAt: p.CreatedAt, ReviewSeenAt: p.ReviewSeenAt, ReviewSeenBy: p.ReviewSeenBy,
		ReviewResolvedAt: p.ReviewResolvedAt, ReviewResolvedBy: p.ReviewResolvedBy, ReviewNote: p.ReviewNote,
	}
}

type adminPaymentDetailJSON struct {
	adminPaymentJSON
	Events  []EventSummary `json:"events"`
	Refunds []adminRefund  `json:"refunds"`
}

type page[T any] struct {
	Items      []T        `json:"items"`
	NextCursor *uuid.UUID `json:"next_cursor"`
}

// queryUUID reads an optional uuid query parameter; a bad one is
// httpx.ErrInvalidRequest.
func queryUUID(q url.Values, key string) (*uuid.UUID, error) {
	v := q.Get(key)
	if v == "" {
		return nil, nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil, httpx.ErrInvalidRequest
	}
	return &id, nil
}

func queryString(q url.Values, key string) *string {
	if v := q.Get(key); v != "" {
		return &v
	}
	return nil
}

// queryPage reads ?limit (>= 1) and ?cursor.
func queryPage(q url.Values) (int, *uuid.UUID, error) {
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return 0, nil, httpx.ErrInvalidRequest
		}
		limit = n
	}
	cursor, err := queryUUID(q, "cursor")
	return limit, cursor, err
}

// pathID parses {id}; a malformed id is notFound, like an unknown one.
func pathID(w http.ResponseWriter, r *http.Request, notFound error) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, r, notFound)
		return uuid.Nil, false
	}
	return id, true
}

// AdminRoutes registers the Super Admin refund and payment review API
// (Authenticate -> RequireRole(super_admin)).
func (s *Service) AdminRoutes(mux httpx.Router, iss *auth.Issuer) {
	admin := func(h func(w http.ResponseWriter, r *http.Request, actor uuid.UUID)) http.Handler {
		return httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := auth.PrincipalFrom(r.Context())
			h(w, r, p.UserID)
		}), auth.Authenticate(iss), auth.RequireRole(auth.RoleSuperAdmin))
	}
	write := func(w http.ResponseWriter, r *http.Request, v any, err error) {
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, v)
	}

	mux.Handle("GET /v1/admin/refunds", admin(func(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
		q := r.URL.Query()
		f := RefundFilter{Status: queryString(q, "status"), Reason: queryString(q, "reason")}
		var err error
		if f.BookingID, err = queryUUID(q, "booking_id"); err == nil {
			if f.PaymentID, err = queryUUID(q, "payment_id"); err == nil {
				f.Limit, f.Cursor, err = queryPage(q)
			}
		}
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		items, next, err := s.AdminListRefunds(r.Context(), f)
		write(w, r, page[adminRefund]{Items: toAdminRefunds(items), NextCursor: next}, err)
	}))

	mux.Handle("GET /v1/admin/refunds/{id}", admin(func(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
		id, ok := pathID(w, r, ErrRefundNotFound)
		if !ok {
			return
		}
		ref, err := s.AdminGetRefund(r.Context(), id)
		write(w, r, toAdminRefund(ref), err)
	}))

	mux.Handle("POST /v1/admin/refunds/{id}/complete", admin(func(w http.ResponseWriter, r *http.Request, actor uuid.UUID) {
		id, ok := pathID(w, r, ErrRefundNotFound)
		if !ok {
			return
		}
		var in struct {
			TransferReference string     `json:"transfer_reference"`
			ProofFileID       *uuid.UUID `json:"proof_file_id"`
		}
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		ref, err := s.CompleteRefund(r.Context(), actor, id, CompleteRefundInput{
			TransferReference: in.TransferReference, ProofFileID: in.ProofFileID,
		})
		write(w, r, toAdminRefund(ref), err)
	}))

	mux.Handle("GET /v1/admin/payments", admin(func(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
		q := r.URL.Query()
		f := PaymentFilter{Status: queryString(q, "status")}
		if v := q.Get("open_review"); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				httpx.WriteError(w, r, httpx.ErrInvalidRequest)
				return
			}
			f.OpenReview = b
		}
		var err error
		if f.CheckoutID, err = queryUUID(q, "checkout_id"); err == nil {
			f.Limit, f.Cursor, err = queryPage(q)
		}
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		items, next, err := s.AdminListPayments(r.Context(), f)
		out := make([]adminPaymentJSON, len(items))
		for i, p := range items {
			out[i] = toAdminPayment(p)
		}
		write(w, r, page[adminPaymentJSON]{Items: out, NextCursor: next}, err)
	}))

	mux.Handle("GET /v1/admin/payments/{id}", admin(func(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
		id, ok := pathID(w, r, ErrPaymentNotFound)
		if !ok {
			return
		}
		d, err := s.AdminGetPayment(r.Context(), id)
		write(w, r, adminPaymentDetailJSON{
			adminPaymentJSON: toAdminPayment(d.Payment), Events: d.Events, Refunds: toAdminRefunds(d.Refunds),
		}, err)
	}))

	mux.Handle("POST /v1/admin/payments/{id}/seen", admin(func(w http.ResponseWriter, r *http.Request, actor uuid.UUID) {
		id, ok := pathID(w, r, ErrPaymentNotFound)
		if !ok {
			return
		}
		// The body is optional: empty or {}.
		var in struct{}
		if _, err := httpx.DecodeOptionalJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, err := s.MarkReviewSeen(r.Context(), actor, id)
		write(w, r, toAdminPayment(p), err)
	}))

	mux.Handle("POST /v1/admin/payments/{id}/resolve", admin(func(w http.ResponseWriter, r *http.Request, actor uuid.UUID) {
		id, ok := pathID(w, r, ErrPaymentNotFound)
		if !ok {
			return
		}
		// The body is optional: empty, {} or {"note": "..."}.
		var in struct {
			Note *string `json:"note"`
		}
		if _, err := httpx.DecodeOptionalJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, err := s.ResolveReview(r.Context(), actor, id, in.Note)
		write(w, r, toAdminPayment(p), err)
	}))
}
