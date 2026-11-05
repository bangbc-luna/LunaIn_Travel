package payout

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// markPaidInput is the POST /v1/admin/payouts/{id}/mark-paid body.
type markPaidInput struct {
	TransferReference string     `json:"transfer_reference"`
	ProofFileID       *uuid.UUID `json:"proof_file_id"`
}

// AdminRoutes registers the Super Admin payout API
// (Authenticate -> RequireRole(super_admin)).
func (s *Service) AdminRoutes(mux httpx.Router, iss *auth.Issuer) {
	admin := func(h http.HandlerFunc) http.Handler {
		return httpx.Chain(h, auth.Authenticate(iss), auth.RequireRole(auth.RoleSuperAdmin))
	}

	mux.Handle("GET /v1/admin/payouts", admin(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		lf, err := parseListFilter(q)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		f := AdminFilter{Status: lf.Status, Limit: lf.Limit, Cursor: lf.Cursor}
		if v := q.Get("partner_id"); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				httpx.WriteError(w, r, httpx.ErrInvalidRequest)
				return
			}
			f.PartnerID = &id
		}
		items, next, err := s.AdminList(r.Context(), f)
		write(w, r, http.StatusOK, listPage{Items: items, NextCursor: next}, err)
	}))

	mux.Handle("GET /v1/admin/payouts/{id}", admin(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		v, err := s.AdminGet(r.Context(), id)
		write(w, r, http.StatusOK, v, err)
	}))

	mux.Handle("POST /v1/admin/payouts/{id}/approve", admin(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		// The body is optional: empty or {}.
		var in struct{}
		if _, err := httpx.DecodeOptionalJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		v, err := s.Approve(r.Context(), p.UserID, id)
		write(w, r, http.StatusOK, v, err)
	}))

	mux.Handle("POST /v1/admin/payouts/{id}/mark-paid", admin(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in markPaidInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		mi := MarkPaidInput{TransferReference: in.TransferReference}
		if in.ProofFileID != nil {
			mi.ProofFileID = *in.ProofFileID
		}
		p, _ := auth.PrincipalFrom(r.Context())
		v, err := s.MarkPaid(r.Context(), p.UserID, id, mi)
		write(w, r, http.StatusOK, v, err)
	}))

	mux.Handle("POST /v1/admin/payouts/{id}/reject", admin(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			Reason string `json:"reason"`
		}
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		v, err := s.Reject(r.Context(), p.UserID, id, in.Reason)
		write(w, r, http.StatusOK, v, err)
	}))
}
