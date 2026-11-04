// Package identity handles accounts, email verification and sessions.
package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/bangbc-luna/lunain-travel/internal/identity/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
)

const (
	userStatusLocked    = "locked"
	refreshTokenBytes   = 32
	uniqueViolation     = "23505"
	verificationSubject = "Mã xác minh LunaIn Travel"
)

// Deps are the collaborators of Service.
type Deps struct {
	Pool       *pgxpool.Pool
	Jobs       *river.Client[pgx.Tx]
	Issuer     *auth.Issuer
	Clock      clock.Clock
	CodePepper []byte
	Google     GoogleVerifier
	RefreshTTL time.Duration
}

// Service implements the identity use cases.
type Service struct {
	pool       *pgxpool.Pool
	jobs       *river.Client[pgx.Tx]
	issuer     *auth.Issuer
	clock      clock.Clock
	pepper     []byte
	google     GoogleVerifier
	refreshTTL time.Duration
}

// NewService creates a Service.
func NewService(deps Deps) *Service {
	return &Service{
		pool:       deps.Pool,
		jobs:       deps.Jobs,
		issuer:     deps.Issuer,
		clock:      deps.Clock,
		pepper:     deps.CodePepper,
		google:     deps.Google,
		refreshTTL: deps.RefreshTTL,
	}
}

// Tokens is a freshly issued session.
type Tokens struct {
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	Role             auth.Role `json:"role"`
}

// Register creates an unverified customer and emails a signup code. If the
// email belongs to an unverified customer, the password is replaced and a new
// code is sent (subject to the resend cooldown), since the earlier signup may
// have been someone mistyping their own address.
func (s *Service) Register(ctx context.Context, email, password string) error {
	e, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	if err := ValidatePassword(password); err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	now := s.clock.Now()

	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		u, err := q.GetUserByEmailForUpdate(ctx, e)
		if errors.Is(err, pgx.ErrNoRows) {
			id, err := uuid.NewV7()
			if err != nil {
				return err
			}
			if err := q.InsertUser(ctx, store.InsertUserParams{
				ID: id, Email: e, PasswordHash: &hash, Role: string(auth.RoleCustomer), Now: now,
			}); err != nil {
				return fmt.Errorf("insert user: %w", err)
			}
			if err := q.InsertCustomerProfile(ctx, store.InsertCustomerProfileParams{UserID: id, Now: now}); err != nil {
				return fmt.Errorf("insert customer profile: %w", err)
			}
			return s.sendSignupCodeTx(ctx, tx, q, id, e, now)
		}
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		// Only a pending customer signup may be taken over by a new signup.
		if u.EmailVerifiedAt != nil || u.Role != string(auth.RoleCustomer) || u.Status == userStatusLocked {
			return ErrEmailTaken
		}
		if err := s.checkCooldown(ctx, q, u.ID, now); err != nil {
			return err
		}
		if err := q.UpdateUserPassword(ctx, store.UpdateUserPasswordParams{
			PasswordHash: &hash, Now: now, ID: u.ID,
		}); err != nil {
			return fmt.Errorf("update password: %w", err)
		}
		return s.sendSignupCodeTx(ctx, tx, q, u.ID, e, now)
	})
	if isUniqueViolation(err, "users_email_key") {
		// A concurrent signup for the same email just created the user and
		// sent it a code.
		return ErrResendTooSoon
	}
	return err
}

// ResendVerification emails a new signup code. It returns nil without sending
// anything when the email is invalid, unknown or already verified, so it does
// not reveal which emails have accounts.
func (s *Service) ResendVerification(ctx context.Context, email string) error {
	e, err := NormalizeEmail(email)
	if err != nil {
		return nil
	}
	now := s.clock.Now()
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		u, err := q.GetUserByEmailForUpdate(ctx, e)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		if u.EmailVerifiedAt != nil {
			return nil
		}
		if err := s.checkCooldown(ctx, q, u.ID, now); err != nil {
			return err
		}
		return s.sendSignupCodeTx(ctx, tx, q, u.ID, e, now)
	})
}

// VerifyEmail checks the latest signup code and, if correct, marks the email
// verified and starts a session. Any failure is reported as ErrInvalidCode.
func (s *Service) VerifyEmail(ctx context.Context, email, code string) (Tokens, error) {
	e, err := NormalizeEmail(email)
	if err != nil {
		return Tokens{}, ErrInvalidCode
	}
	now := s.clock.Now()

	var (
		tokens Tokens
		wrong  bool
	)
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		u, err := q.GetUserByEmail(ctx, e)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCode
		}
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		c, err := q.GetLatestActiveCodeForUpdate(ctx, store.GetLatestActiveCodeForUpdateParams{
			UserID: u.ID, Purpose: purposeSignup,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCode
		}
		if err != nil {
			return fmt.Errorf("get code: %w", err)
		}
		if !now.Before(c.ExpiresAt) || c.Attempts >= maxCodeAttempts {
			return ErrInvalidCode
		}
		if !hmac.Equal(hashCode(s.pepper, purposeSignup, u.ID.String(), code), c.CodeHash) {
			// Commit the failed attempt; the error is returned after commit.
			wrong = true
			return q.IncrementCodeAttempts(ctx, store.IncrementCodeAttemptsParams{Now: now, ID: c.ID})
		}
		if u.Status == userStatusLocked {
			return ErrAccountLocked
		}
		role := auth.Role(u.Role)
		if !role.Valid() {
			return fmt.Errorf("user %s has invalid role %q", u.ID, u.Role)
		}
		if err := q.ConsumeCode(ctx, store.ConsumeCodeParams{Now: now, ID: c.ID}); err != nil {
			return fmt.Errorf("consume code: %w", err)
		}
		if err := q.MarkEmailVerified(ctx, store.MarkEmailVerifiedParams{Now: now, ID: u.ID}); err != nil {
			return fmt.Errorf("mark verified: %w", err)
		}
		familyID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		tokens, err = s.issueSession(ctx, tx, u.ID, role, familyID)
		return err
	})
	if err != nil {
		return Tokens{}, err
	}
	if wrong {
		return Tokens{}, ErrInvalidCode
	}
	return tokens, nil
}

// issueSession creates an access token and a new refresh token in familyID.
// Only the SHA-256 of the refresh token is stored.
func (s *Service) issueSession(ctx context.Context, tx pgx.Tx, userID uuid.UUID, role auth.Role, familyID uuid.UUID) (Tokens, error) {
	now := s.clock.Now()
	raw := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return Tokens{}, err
	}
	refresh := base64.RawURLEncoding.EncodeToString(raw)
	id, err := uuid.NewV7()
	if err != nil {
		return Tokens{}, err
	}
	refreshExp := now.Add(s.refreshTTL)
	if err := store.New(tx).InsertRefreshToken(ctx, store.InsertRefreshTokenParams{
		ID:        id,
		UserID:    userID,
		FamilyID:  familyID,
		TokenHash: hashRefreshToken(refresh),
		ExpiresAt: refreshExp,
		Now:       now,
	}); err != nil {
		return Tokens{}, fmt.Errorf("insert refresh token: %w", err)
	}
	access, accessExp, err := s.issuer.Issue(auth.Principal{UserID: userID, Role: role})
	if err != nil {
		return Tokens{}, fmt.Errorf("issue access token: %w", err)
	}
	return Tokens{
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExp,
		Role:             role,
	}, nil
}

func hashRefreshToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// checkCooldown rejects a new code while the user's latest signup code is
// younger than resendCooldown.
func (s *Service) checkCooldown(ctx context.Context, q *store.Queries, userID uuid.UUID, now time.Time) error {
	last, err := q.GetLatestCodeCreatedAt(ctx, store.GetLatestCodeCreatedAtParams{
		UserID: userID, Purpose: purposeSignup,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get latest code: %w", err)
	}
	if now.Before(last.Add(resendCooldown)) {
		return ErrResendTooSoon
	}
	return nil
}

// sendSignupCodeTx invalidates older signup codes, stores a new one and
// enqueues its email, all inside tx.
func (s *Service) sendSignupCodeTx(ctx context.Context, tx pgx.Tx, q *store.Queries, userID uuid.UUID, email string, now time.Time) error {
	if err := q.InvalidateCodes(ctx, store.InvalidateCodesParams{
		Now: now, UserID: userID, Purpose: purposeSignup,
	}); err != nil {
		return fmt.Errorf("invalidate codes: %w", err)
	}
	code, err := newCode()
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if err := q.InsertVerification(ctx, store.InsertVerificationParams{
		ID:        id,
		UserID:    userID,
		Purpose:   purposeSignup,
		CodeHash:  hashCode(s.pepper, purposeSignup, userID.String(), code),
		ExpiresAt: now.Add(codeTTL),
		Now:       now,
	}); err != nil {
		return fmt.Errorf("insert verification: %w", err)
	}
	return mailer.EnqueueTx(ctx, tx, s.jobs, mailer.Message{
		To:      email,
		Subject: verificationSubject,
		Text: "Xin chào,\n\n" +
			"Mã xác minh tài khoản LunaIn Travel của bạn là: " + code + "\n\n" +
			"Mã có hiệu lực trong 10 phút.\n\n" +
			"Nếu bạn không đăng ký tài khoản, hãy bỏ qua email này.\n",
	})
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}
