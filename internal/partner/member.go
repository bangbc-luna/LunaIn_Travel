package partner

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/partner/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
)

type partnerIDKey struct{}

// IDFrom returns the partner bound to the request by RequireMember. It is the
// only way partner-facing handlers obtain a partner_id: it never comes from
// the client (spec §14).
func IDFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(partnerIDKey{}).(uuid.UUID)
	return id, ok
}

// RequireMember resolves the caller's partner from partner_members and puts
// it in the context. Mount it after auth.Authenticate and
// auth.RequireRole(auth.RolePartner); a caller without membership gets 403.
func (s *Service) RequireMember() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.PrincipalFrom(r.Context())
			if !ok {
				httpx.WriteError(w, r, httpx.ErrUnauthorized)
				return
			}
			id, err := store.New(s.pool).MemberPartnerID(r.Context(), p.UserID)
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.WriteError(w, r, httpx.ErrForbidden)
				return
			}
			if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), partnerIDKey{}, id)))
		})
	}
}
