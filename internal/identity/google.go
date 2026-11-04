package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/identity/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

const (
	googleIssuer   = "https://accounts.google.com"
	providerGoogle = "google"
)

// GoogleIdentity is the verified content of a Google ID token.
type GoogleIdentity struct {
	Subject       string
	Email         string
	EmailVerified bool
	// HostedDomain is the optional "hd" claim of Google Workspace accounts.
	HostedDomain string
}

// GoogleVerifier validates a Google ID token and returns its identity.
type GoogleVerifier interface {
	Verify(ctx context.Context, idToken string) (GoogleIdentity, error)
}

type oidcGoogleVerifier struct {
	verifier *oidc.IDTokenVerifier
}

// NewGoogleVerifier returns a verifier for ID tokens issued to clientID. It
// fetches Google's OIDC discovery document, so it needs network access.
func NewGoogleVerifier(ctx context.Context, clientID string) (GoogleVerifier, error) {
	provider, err := oidc.NewProvider(ctx, googleIssuer)
	if err != nil {
		return nil, fmt.Errorf("google oidc provider: %w", err)
	}
	return &oidcGoogleVerifier{verifier: provider.Verifier(&oidc.Config{ClientID: clientID})}, nil
}

func (v *oidcGoogleVerifier) Verify(ctx context.Context, idToken string) (GoogleIdentity, error) {
	tok, err := v.verifier.Verify(ctx, idToken)
	if err != nil {
		return GoogleIdentity{}, err
	}
	var raw json.RawMessage
	if err := tok.Claims(&raw); err != nil {
		return GoogleIdentity{}, err
	}
	return identityFromClaims(tok.Subject, raw)
}

// identityFromClaims extracts the identity from the claims of an already
// verified ID token. email_verified may arrive as a bool or as a string.
func identityFromClaims(subject string, rawClaims []byte) (GoogleIdentity, error) {
	if subject == "" {
		return GoogleIdentity{}, errors.New("google token has no subject")
	}
	var c struct {
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"`
		HostedDomain  string          `json:"hd"`
	}
	if err := json.Unmarshal(rawClaims, &c); err != nil {
		return GoogleIdentity{}, fmt.Errorf("parse google claims: %w", err)
	}
	verified := false
	if len(c.EmailVerified) > 0 {
		var b bool
		if err := json.Unmarshal(c.EmailVerified, &b); err == nil {
			verified = b
		} else {
			var s string
			if err := json.Unmarshal(c.EmailVerified, &s); err != nil {
				return GoogleIdentity{}, errors.New("google email_verified claim has unexpected type")
			}
			b, err := strconv.ParseBool(s)
			if err != nil {
				return GoogleIdentity{}, errors.New("google email_verified claim is not a boolean")
			}
			verified = b
		}
	}
	return GoogleIdentity{Subject: subject, Email: c.Email, EmailVerified: verified, HostedDomain: c.HostedDomain}, nil
}

// errRetryGoogle aborts a LoginWithGoogle transaction that lost a race
// against a concurrent first login, or whose identity was detached (password
// reset) while it waited for the user row lock; the lookup is then repeated.
var errRetryGoogle = errors.New("identity: concurrent google login, retry")

// LoginWithGoogle verifies a Google ID token and starts a new session (a new
// refresh token family).
//
//   - A known (google, sub) identity logs its user in, even if Google now
//     reports a different email.
//   - Otherwise an existing user with the same normalized email is linked. An
//     unverified user becomes verified and its pending signup codes (and the
//     password bound to them) are dropped; password_hash stays NULL. Partner
//     and super_admin accounts are never linked.
//   - Otherwise a verified customer with an empty profile is created.
//
// Locked accounts get ErrAccountLocked. A concurrent first login with the same
// token loses on a unique constraint and re-reads the winner's rows.
func (s *Service) LoginWithGoogle(ctx context.Context, idToken string) (Tokens, error) {
	gid, err := s.google.Verify(ctx, idToken)
	if err != nil {
		// Log the cause (never the token) to diagnose config/clock issues.
		slog.Default().Warn("google token verify failed", "err", err)
		return Tokens{}, ErrGoogleTokenInvalid
	}
	if !gid.EmailVerified || gid.Subject == "" {
		return Tokens{}, ErrGoogleTokenInvalid
	}
	email, err := NormalizeEmail(gid.Email)
	if err != nil {
		return Tokens{}, ErrGoogleTokenInvalid
	}

	// One retry is enough: after a lost race the winner's rows are committed.
	for range 2 {
		var tokens Tokens
		err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			var err error
			tokens, err = s.googleLoginTx(ctx, tx, gid, email)
			return err
		})
		if errors.Is(err, errRetryGoogle) || isUniqueViolation(err, "users_email_key") ||
			isUniqueViolation(err, "user_identities_provider_provider_subject_key") {
			continue
		}
		if err != nil {
			return Tokens{}, err
		}
		return tokens, nil
	}
	return Tokens{}, errors.New("identity: google login kept racing")
}

func (s *Service) googleLoginTx(ctx context.Context, tx pgx.Tx, gid GoogleIdentity, email string) (Tokens, error) {
	q := store.New(tx)
	now := s.clock.Now()

	var u store.User
	ident, err := q.GetIdentity(ctx, store.GetIdentityParams{Provider: providerGoogle, ProviderSubject: gid.Subject})
	switch {
	case err == nil:
		// The identity wins over the email Google reports today. The first
		// read was unlocked (we did not know the user yet); lock the user row
		// (lock order: user first, then dependents) and re-read the identity,
		// since a ResetPassword that held the lock may have detached it in
		// the meantime. If it is gone or moved, rerun the whole lookup so the
		// by-email policy applies.
		u, err = q.GetUserByIDForUpdate(ctx, ident.UserID)
		if err != nil {
			return Tokens{}, fmt.Errorf("lock user: %w", err)
		}
		again, err := q.GetIdentity(ctx, store.GetIdentityParams{Provider: providerGoogle, ProviderSubject: gid.Subject})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && again.UserID != ident.UserID) {
			return Tokens{}, errRetryGoogle
		}
		if err != nil {
			return Tokens{}, fmt.Errorf("recheck identity: %w", err)
		}
	case errors.Is(err, pgx.ErrNoRows):
		u, err = q.GetUserByEmailForUpdate(ctx, email)
		switch {
		case err == nil:
			if isInternalRole(u.Role) {
				return Tokens{}, ErrGoogleTokenInvalid
			}
			if u.Status == userStatusLocked {
				return Tokens{}, ErrAccountLocked
			}
			if !googleEmailTrusted(email, gid.HostedDomain) {
				return Tokens{}, ErrGoogleLinkRequiresPassword
			}
			if err := s.linkGoogle(ctx, q, u, gid.Subject, now); err != nil {
				return Tokens{}, err
			}
		case errors.Is(err, pgx.ErrNoRows):
			u, err = s.createGoogleUser(ctx, q, gid.Subject, email, now)
			if err != nil {
				return Tokens{}, err
			}
		default:
			return Tokens{}, fmt.Errorf("get user: %w", err)
		}
	default:
		return Tokens{}, fmt.Errorf("get identity: %w", err)
	}

	if isInternalRole(u.Role) {
		return Tokens{}, ErrGoogleTokenInvalid
	}
	if u.Status == userStatusLocked {
		return Tokens{}, ErrAccountLocked
	}
	role := auth.Role(u.Role)
	if !role.Valid() {
		return Tokens{}, fmt.Errorf("user %s has invalid role %q", u.ID, u.Role)
	}
	familyID, err := uuid.NewV7()
	if err != nil {
		return Tokens{}, err
	}
	return s.issueSession(ctx, tx, u.ID, role, familyID)
}

// googleEmailTrusted reports whether Google's email_verified claim is reliable
// enough to link an existing account by email: only for @gmail.com addresses
// or Workspace accounts (hd claim). For other addresses the claim can be stale.
func googleEmailTrusted(email, hostedDomain string) bool {
	return hostedDomain != "" || strings.HasSuffix(email, "@gmail.com")
}

func isInternalRole(role string) bool {
	return role == string(auth.RolePartner) || role == string(auth.RoleSuperAdmin)
}

// linkGoogle links (google, sub) to u, whose row the caller has locked. If
// u was unverified, the Google-verified email verifies it and its pending
// signup codes are invalidated.
func (s *Service) linkGoogle(ctx context.Context, q *store.Queries, u store.User, sub string, now time.Time) error {
	n, err := s.insertIdentity(ctx, q, u.ID, sub, now)
	if err != nil {
		return err
	}
	if n == 0 {
		return errRetryGoogle // someone linked this sub meanwhile
	}
	if u.EmailVerifiedAt == nil {
		if err := q.SetEmailVerifiedKeepPassword(ctx, store.SetEmailVerifiedKeepPasswordParams{Now: now, ID: u.ID}); err != nil {
			return fmt.Errorf("verify email: %w", err)
		}
		if err := q.InvalidateCodes(ctx, store.InvalidateCodesParams{
			Now: now, UserID: u.ID, Purpose: purposeSignup,
		}); err != nil {
			return fmt.Errorf("invalidate signup codes: %w", err)
		}
	}
	return nil
}

func (s *Service) createGoogleUser(ctx context.Context, q *store.Queries, sub, email string, now time.Time) (store.User, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return store.User{}, err
	}
	n, err := q.InsertVerifiedUser(ctx, store.InsertVerifiedUserParams{
		ID: id, Email: email, PasswordHash: nil, Now: now, Role: string(auth.RoleCustomer),
	})
	if err != nil {
		return store.User{}, fmt.Errorf("insert user: %w", err)
	}
	if n == 0 {
		return store.User{}, errRetryGoogle // a concurrent first login created it
	}
	if err := q.InsertCustomerProfile(ctx, store.InsertCustomerProfileParams{UserID: id, Now: now}); err != nil {
		return store.User{}, fmt.Errorf("insert customer profile: %w", err)
	}
	n, err = s.insertIdentity(ctx, q, id, sub, now)
	if err != nil {
		return store.User{}, err
	}
	if n == 0 {
		return store.User{}, errRetryGoogle
	}
	return q.GetUserByIDForUpdate(ctx, id)
}

func (s *Service) insertIdentity(ctx context.Context, q *store.Queries, userID uuid.UUID, sub string, now time.Time) (int64, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return 0, err
	}
	n, err := q.InsertIdentity(ctx, store.InsertIdentityParams{
		ID: id, UserID: userID, Provider: providerGoogle, ProviderSubject: sub, Now: now,
	})
	if err != nil {
		return 0, fmt.Errorf("insert identity: %w", err)
	}
	return n, nil
}
