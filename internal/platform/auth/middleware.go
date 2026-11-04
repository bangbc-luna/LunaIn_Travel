package auth

import (
	"net/http"
	"slices"
	"strings"

	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

// Authenticate reads "Authorization: Bearer <token>". A request without the
// header passes through unauthenticated; a malformed header or invalid token
// is rejected with 401.
func Authenticate(iss *Issuer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if h == "" {
				next.ServeHTTP(w, r)
				return
			}
			scheme, token, ok := strings.Cut(h, " ")
			token = strings.TrimSpace(token)
			if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
				httpx.WriteError(w, r, httpx.ErrUnauthorized)
				return
			}
			p, err := iss.Verify(token)
			if err != nil {
				httpx.WriteError(w, r, httpx.ErrUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// RequireRole allows only authenticated callers holding one of roles:
// 401 without a Principal, 403 with the wrong role.
func RequireRole(roles ...Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				httpx.WriteError(w, r, httpx.ErrUnauthorized)
				return
			}
			if !slices.Contains(roles, p.Role) {
				httpx.WriteError(w, r, httpx.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
