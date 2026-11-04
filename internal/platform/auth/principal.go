package auth

import (
	"context"

	"github.com/google/uuid"
)

// Role is a user role in the system.
type Role string

const (
	RoleCustomer   Role = "customer"
	RolePartner    Role = "partner"
	RoleSuperAdmin Role = "super_admin"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleCustomer, RolePartner, RoleSuperAdmin:
		return true
	}
	return false
}

// Principal is the authenticated caller.
type Principal struct {
	UserID uuid.UUID
	Role   Role
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom extracts the Principal from ctx, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
