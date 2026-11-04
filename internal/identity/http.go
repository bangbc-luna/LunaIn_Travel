package identity

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// Me is the caller's own account summary.
type Me struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	Role  auth.Role `json:"role"`
}

// GetMe returns the account of userID; a missing user is httpx.ErrNotFound.
func (s *Service) GetMe(ctx context.Context, userID uuid.UUID) (Me, error) {
	var m Me
	var role string
	err := s.pool.QueryRow(ctx, `SELECT id, email, role FROM users WHERE id = $1`, userID).
		Scan(&m.ID, &m.Email, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Me{}, httpx.ErrNotFound
	}
	if err != nil {
		return Me{}, err
	}
	m.Role = auth.Role(role)
	return m, nil
}

type emailReq struct {
	Email string `json:"email"`
}

type credentialsReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type verifyReq struct {
	Email    string `json:"email"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

type googleReq struct {
	IDToken string `json:"id_token"`
}

type refreshReq struct {
	RefreshToken string `json:"refresh_token"`
}

type resetReq struct {
	Email       string `json:"email"`
	Code        string `json:"code"`
	NewPassword string `json:"new_password"`
}

// Routes registers the identity HTTP API on mux. clientIP is the rate-limit
// key (see httpx.ClientIPFunc). Each public route has its own limiter whose
// burst equals its per-minute rate.
func (s *Service) Routes(mux httpx.Router, iss *auth.Issuer, clientIP func(*http.Request) string) {
	limited := func(perMinute int, h http.HandlerFunc) http.Handler {
		return httpx.RateLimit(perMinute, perMinute, clientIP)(h)
	}
	empty := struct{}{}

	mux.Handle("POST /v1/auth/register", limited(10, func(w http.ResponseWriter, r *http.Request) {
		var req credentialsReq
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if err := s.Register(r.Context(), req.Email, req.Password); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusAccepted, empty)
	}))

	mux.Handle("POST /v1/auth/resend-verification", limited(5, func(w http.ResponseWriter, r *http.Request) {
		var req emailReq
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if err := s.ResendVerification(r.Context(), req.Email); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusAccepted, empty)
	}))

	mux.Handle("POST /v1/auth/verify-email", limited(20, func(w http.ResponseWriter, r *http.Request) {
		var req verifyReq
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		tokens, err := s.VerifyEmail(r.Context(), req.Email, req.Code, req.Password)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, tokens)
	}))

	mux.Handle("POST /v1/auth/login", limited(20, func(w http.ResponseWriter, r *http.Request) {
		var req credentialsReq
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		tokens, err := s.Login(r.Context(), req.Email, req.Password)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, tokens)
	}))

	mux.Handle("POST /v1/auth/google", limited(20, func(w http.ResponseWriter, r *http.Request) {
		var req googleReq
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		tokens, err := s.LoginWithGoogle(r.Context(), req.IDToken)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, tokens)
	}))

	mux.Handle("POST /v1/auth/refresh", limited(60, func(w http.ResponseWriter, r *http.Request) {
		var req refreshReq
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		tokens, err := s.Refresh(r.Context(), req.RefreshToken)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, tokens)
	}))

	mux.Handle("POST /v1/auth/logout", limited(60, func(w http.ResponseWriter, r *http.Request) {
		var req refreshReq
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if err := s.Logout(r.Context(), req.RefreshToken); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.Handle("POST /v1/auth/password/forgot", limited(5, func(w http.ResponseWriter, r *http.Request) {
		var req emailReq
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if err := s.RequestPasswordReset(r.Context(), req.Email); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusAccepted, empty)
	}))

	mux.Handle("POST /v1/auth/password/reset", limited(10, func(w http.ResponseWriter, r *http.Request) {
		var req resetReq
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if err := s.ResetPassword(r.Context(), req.Email, req.Code, req.NewPassword); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	authed := auth.Authenticate(iss)
	anyRole := auth.RequireRole(auth.RoleCustomer, auth.RolePartner, auth.RoleSuperAdmin)
	customer := auth.RequireRole(auth.RoleCustomer)

	mux.Handle("GET /v1/me", authed(anyRole(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		me, err := s.GetMe(r.Context(), p.UserID)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, me)
	}))))

	mux.Handle("GET /v1/customer/profile", authed(customer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		prof, err := s.GetProfile(r.Context(), p.UserID)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, prof)
	}))))

	mux.Handle("PATCH /v1/customer/profile", authed(customer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		var req ProfileUpdate
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		prof, err := s.UpdateProfile(r.Context(), p.UserID, req)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, prof)
	}))))
}
