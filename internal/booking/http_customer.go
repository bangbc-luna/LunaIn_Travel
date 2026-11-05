package booking

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// listPage is the JSON of a booking list page.
type listPage struct {
	Items      []BookingSummary `json:"items"`
	NextCursor *uuid.UUID       `json:"next_cursor"`
}

// parseListFilter reads ?status, ?limit and ?cursor; bad values are
// httpx.ErrInvalidRequest.
func parseListFilter(q url.Values) (ListFilter, error) {
	var f ListFilter
	if v := q.Get("status"); v != "" {
		f.Status = &v
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return f, httpx.ErrInvalidRequest
		}
		f.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		c, err := uuid.Parse(v)
		if err != nil {
			return f, httpx.ErrInvalidRequest
		}
		f.Cursor = &c
	}
	return f, nil
}

func writeList(w http.ResponseWriter, r *http.Request, items []BookingSummary, next *uuid.UUID, err error) {
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, listPage{Items: items, NextCursor: next})
}

// pathID parses the {id} path value; a bad id is a 404 like an unknown one.
func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, r, ErrBookingNotFound)
		return uuid.Nil, false
	}
	return id, true
}

func writeView(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, status, v)
}

// CustomerRoutes registers the customer booking API. Every route is
// Authenticate -> RequireRole(customer); the customer is the token's user,
// and a booking of another customer is answered 404.
func (s *Service) CustomerRoutes(mux httpx.Router, iss *auth.Issuer) {
	customer := func(h func(w http.ResponseWriter, r *http.Request, userID uuid.UUID)) http.Handler {
		return httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := auth.PrincipalFrom(r.Context())
			h(w, r, p.UserID)
		}), auth.Authenticate(iss), auth.RequireRole(auth.RoleCustomer))
	}

	mux.Handle("POST /v1/bookings/attraction", customer(func(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
		key := r.Header.Get("Idempotency-Key")
		if !validIdemKey(key) {
			httpx.WriteError(w, r, ErrIdempotencyKeyRequired)
			return
		}
		var in CreateAttractionInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		v, err := s.CreateAttraction(r.Context(), userID, key, in)
		writeView(w, r, http.StatusCreated, v, err)
	}))

	mux.Handle("POST /v1/bookings/ride", customer(func(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
		key := r.Header.Get("Idempotency-Key")
		if !validIdemKey(key) {
			httpx.WriteError(w, r, ErrIdempotencyKeyRequired)
			return
		}
		var in CreateRideInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		v, err := s.CreateRide(r.Context(), userID, key, in)
		writeView(w, r, http.StatusCreated, v, err)
	}))

	mux.Handle("GET /v1/bookings", customer(func(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
		f, err := parseListFilter(r.URL.Query())
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		items, next, err := s.ListMine(r.Context(), userID, f)
		writeList(w, r, items, next, err)
	}))

	mux.Handle("GET /v1/bookings/{id}", customer(func(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		v, err := s.GetMine(r.Context(), userID, id)
		writeView(w, r, http.StatusOK, v, err)
	}))

	mux.Handle("POST /v1/bookings/{id}/pay", customer(func(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		v, err := s.Pay(r.Context(), userID, id)
		writeView(w, r, http.StatusOK, v, err)
	}))

	mux.Handle("POST /v1/bookings/{id}/cancel", customer(func(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			BankAccount *BankAccount `json:"bank_account"`
		}
		// The body is optional (also chunked): none cancels without a bank account.
		if _, err := httpx.DecodeOptionalJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		v, err := s.CancelByCustomer(r.Context(), userID, id, in.BankAccount)
		writeView(w, r, http.StatusOK, v, err)
	}))

	mux.Handle("POST /v1/bookings/{id}/reschedule", customer(func(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in RescheduleInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		v, err := s.Reschedule(r.Context(), userID, id, in.VisitDate, in.TimeSlotID)
		writeView(w, r, http.StatusOK, v, err)
	}))

	// Refunds (payment module) of the customer.
	mux.Handle("GET /v1/refunds", customer(func(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
		items, err := s.payments.ListMine(r.Context(), userID)
		writeView(w, r, http.StatusOK, refundList{Items: items}, err)
	}))

	mux.Handle("PUT /v1/refunds/{id}/bank-account", customer(func(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			httpx.WriteError(w, r, payment.ErrRefundNotFound)
			return
		}
		var in BankAccount
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if err := s.payments.SetRefundBankAccount(r.Context(), userID, id, in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.Handle("GET /v1/customer/refund-account", customer(func(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
		acc, err := s.payments.RefundAccount(r.Context(), userID)
		writeView(w, r, http.StatusOK, acc, err)
	}))

	mux.Handle("PUT /v1/customer/refund-account", customer(func(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
		var in BankAccount
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		acc, err := s.payments.SaveRefundAccount(r.Context(), userID, in)
		writeView(w, r, http.StatusOK, acc, err)
	}))
}

// refundList is the JSON of GET /v1/refunds (newest first).
type refundList struct {
	Items []payment.Refund `json:"items"`
}
