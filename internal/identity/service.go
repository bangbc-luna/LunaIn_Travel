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
	"sync"
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
	resetSubject        = "Đặt lại mật khẩu LunaIn Travel"
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

// Lock order rule: every identity transaction locks the users row first
// (SELECT ... FOR NO KEY UPDATE) and only then reads/locks/updates dependent
// rows (email_verifications, refresh_tokens, user_identities, profiles). Keep
// it user → dependents to avoid deadlocks between Register,
// ResendVerification, VerifyEmail, Refresh and friends. FOR NO KEY UPDATE
// still serializes identity transactions on the same user but, unlike FOR
// UPDATE, does not block other modules inserting rows that reference users.
//
// The password submitted with a signup is stored (as argon2id) on the signup
// code row, not on the user. It becomes users.password_hash only when that
// code is verified together with the same password, so a re-registration by
// someone else cannot replace the password of a pending account.

// Register creates an unverified customer (without a password) and emails a
// signup code bound to the submitted password. If the email belongs to an
// unverified active customer, a new code bound to the new password is sent
// (subject to the resend cooldown), since the earlier signup may have been
// someone mistyping their own address.
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
				ID: id, Email: e, PasswordHash: nil, Role: string(auth.RoleCustomer), Now: now,
			}); err != nil {
				return fmt.Errorf("insert user: %w", err)
			}
			if err := q.InsertCustomerProfile(ctx, store.InsertCustomerProfileParams{UserID: id, Now: now}); err != nil {
				return fmt.Errorf("insert customer profile: %w", err)
			}
			return s.sendSignupCodeTx(ctx, tx, q, id, e, hash, now)
		}
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		if !pendingCustomer(u) {
			return ErrEmailTaken
		}
		last, err := latestSignupCode(ctx, q, u.ID)
		if err != nil {
			return err
		}
		if inCooldown(last, now) {
			return ErrResendTooSoon
		}
		return capAsTooSoon(s.sendSignupCodeTx(ctx, tx, q, u.ID, e, hash, now))
	})
	if isUniqueViolation(err, "users_email_key") {
		// A concurrent signup for the same email just created the user and
		// sent it a code.
		return ErrResendTooSoon
	}
	return err
}

// ResendVerification emails a new signup code bound to the same pending
// password as the latest signup code. It returns nil without sending anything
// when the email is invalid, unknown, already verified or not an active
// customer, so it does not reveal which emails have accounts.
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
		if !pendingCustomer(u) {
			return nil
		}
		last, err := latestSignupCode(ctx, q, u.ID)
		if err != nil {
			return err
		}
		if last == nil || last.PasswordHash == nil {
			return nil
		}
		if inCooldown(last, now) {
			return ErrResendTooSoon
		}
		return capAsTooSoon(s.sendSignupCodeTx(ctx, tx, q, u.ID, e, *last.PasswordHash, now))
	})
}

// VerifyEmail checks the latest signup code together with the password that
// was submitted with that signup. If both match, the pending password becomes
// the account password, the email is marked verified and a session starts.
// Any failure is reported as ErrInvalidCode, without saying which part was
// wrong.
func (s *Service) VerifyEmail(ctx context.Context, email, code, password string) (Tokens, error) {
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
		// Lock order: user row first, then the code row.
		u, err := q.GetUserByEmailForUpdate(ctx, e)
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
		if !codeUsable(c, now) {
			return ErrInvalidCode
		}
		// Always evaluate both checks so timing does not reveal which failed.
		codeOK := s.codeMatches(c, purposeSignup, u.ID, code)
		passwordOK := false
		if c.PasswordHash != nil {
			ok, err := VerifyPassword(*c.PasswordHash, password)
			if err != nil {
				return fmt.Errorf("verify pending password: %w", err)
			}
			passwordOK = ok
		}
		if !codeOK || !passwordOK {
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
		if err := q.MarkEmailVerified(ctx, store.MarkEmailVerifiedParams{
			Now: now, PasswordHash: c.PasswordHash, ID: u.ID,
		}); err != nil {
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

// RequestPasswordReset emails a password_reset code. It returns nil without
// sending anything when the email is invalid, unknown, belongs to a locked
// account, a code was sent less than resendCooldown ago, or maxCodesPer24h
// reset codes were already sent in the last 24 hours, so the response
// never reveals which emails have accounts. It works for any unlocked user,
// including unverified customers and Google-only users.
func (s *Service) RequestPasswordReset(ctx context.Context, email string) error {
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
		if u.Status == userStatusLocked {
			return nil
		}
		last, err := latestCode(ctx, q, u.ID, purposePasswordReset)
		if err != nil {
			return err
		}
		if inCooldown(last, now) {
			return nil
		}
		err = s.sendCodeTx(ctx, tx, q, u.ID, e, purposePasswordReset, nil, mailer.Message{
			Subject: resetSubject,
			Text: "Xin chào,\n\n" +
				"Mã đặt lại mật khẩu LunaIn Travel của bạn là: %s\n\n" +
				"Mã có hiệu lực trong 10 phút.\n\n" +
				"Nếu bạn không yêu cầu đặt lại mật khẩu, hãy bỏ qua email này.\n",
		}, now)
		if errors.Is(err, errCodeCapReached) {
			return nil // silent, like the cooldown
		}
		return err
	})
}

// ResetPassword checks the latest password_reset code and, if it matches,
// sets newPassword, marks the email verified (the code proves ownership),
// detaches every external identity (e.g. a Google account that created this
// user before the email owner proved control) and revokes every session. Any failure after the password is validated is
// reported as ErrInvalidCode; a wrong guess still counts as an attempt.
func (s *Service) ResetPassword(ctx context.Context, email, code, newPassword string) error {
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	e, err := NormalizeEmail(email)
	if err != nil {
		return ErrInvalidCode
	}
	// Hash before taking any lock (argon2 is slow).
	hash, err := HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	now := s.clock.Now()

	var wrong bool
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		// Lock order: user row first, then the code row.
		u, err := q.GetUserByEmailForUpdate(ctx, e)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCode
		}
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		c, err := q.GetLatestActiveCodeForUpdate(ctx, store.GetLatestActiveCodeForUpdateParams{
			UserID: u.ID, Purpose: purposePasswordReset,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCode
		}
		if err != nil {
			return fmt.Errorf("get code: %w", err)
		}
		if !codeUsable(c, now) {
			return ErrInvalidCode
		}
		if !s.codeMatches(c, purposePasswordReset, u.ID, code) {
			// Commit the failed attempt; the error is returned after commit.
			wrong = true
			return q.IncrementCodeAttempts(ctx, store.IncrementCodeAttemptsParams{Now: now, ID: c.ID})
		}
		if u.Status == userStatusLocked {
			return ErrInvalidCode
		}
		if err := q.ConsumeCode(ctx, store.ConsumeCodeParams{Now: now, ID: c.ID}); err != nil {
			return fmt.Errorf("consume code: %w", err)
		}
		if err := q.SetUserPassword(ctx, store.SetUserPasswordParams{Now: now, PasswordHash: &hash, ID: u.ID}); err != nil {
			return fmt.Errorf("set password: %w", err)
		}
		// A pending signup (and the password bound to it) is superseded.
		if err := q.InvalidateCodes(ctx, store.InvalidateCodesParams{
			Now: now, UserID: u.ID, Purpose: purposeSignup,
		}); err != nil {
			return fmt.Errorf("invalidate signup codes: %w", err)
		}
		// The email owner just proved control; a linked external identity
		// may belong to someone else, so it must sign in again and re-link.
		if err := q.DeleteUserIdentities(ctx, u.ID); err != nil {
			return fmt.Errorf("detach identities: %w", err)
		}
		return s.RevokeAllSessions(ctx, tx, u.ID)
	})
	if err != nil {
		return err
	}
	if wrong {
		return ErrInvalidCode
	}
	return nil
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

// pendingCustomer reports whether u is an active, unverified customer: the
// only kind of account a public signup or resend may act on.
func pendingCustomer(u store.User) bool {
	return u.EmailVerifiedAt == nil && u.Role == string(auth.RoleCustomer) && u.Status != userStatusLocked
}

// latestSignupCode returns the user's most recent signup code (consumed or
// not), or nil if there is none. Caller must already hold the user row lock.
func latestSignupCode(ctx context.Context, q *store.Queries, userID uuid.UUID) (*store.EmailVerification, error) {
	return latestCode(ctx, q, userID, purposeSignup)
}

// latestCode returns the user's most recent code of purpose (consumed or
// not), or nil if there is none. Caller must already hold the user row lock.
func latestCode(ctx context.Context, q *store.Queries, userID uuid.UUID, purpose string) (*store.EmailVerification, error) {
	c, err := q.GetLatestCode(ctx, store.GetLatestCodeParams{UserID: userID, Purpose: purpose})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest code: %w", err)
	}
	return &c, nil
}

// codeUsable reports whether c is still within its TTL and attempt budget.
func codeUsable(c store.EmailVerification, now time.Time) bool {
	return now.Before(c.ExpiresAt) && c.Attempts < maxCodeAttempts
}

// codeMatches reports whether code is the one stored on c for userID.
func (s *Service) codeMatches(c store.EmailVerification, purpose string, userID uuid.UUID, code string) bool {
	return hmac.Equal(hashCode(s.pepper, purpose, userID.String(), code), c.CodeHash)
}

// inCooldown reports whether last was created less than resendCooldown ago.
func inCooldown(last *store.EmailVerification, now time.Time) bool {
	return last != nil && now.Before(last.CreatedAt.Add(resendCooldown))
}

// sendSignupCodeTx invalidates older signup codes, stores a new one bound to
// passwordHash and enqueues its email, all inside tx.
func (s *Service) sendSignupCodeTx(ctx context.Context, tx pgx.Tx, q *store.Queries, userID uuid.UUID, email, passwordHash string, now time.Time) error {
	return s.sendCodeTx(ctx, tx, q, userID, email, purposeSignup, &passwordHash, mailer.Message{
		Subject: verificationSubject,
		Text: "Xin chào,\n\n" +
			"Mã xác minh tài khoản LunaIn Travel của bạn là: %s\n\n" +
			"Mã có hiệu lực trong 10 phút.\n\n" +
			"Nếu bạn không đăng ký tài khoản, hãy bỏ qua email này.\n",
	}, now)
}

// errCodeCapReached is returned by sendCodeTx when the user already got
// maxCodesPer24h codes of that purpose in the last 24 hours.
var errCodeCapReached = errors.New("identity: daily verification code cap reached")

// capAsTooSoon reports the daily code cap to signup callers as ErrResendTooSoon.
func capAsTooSoon(err error) error {
	if errors.Is(err, errCodeCapReached) {
		return ErrResendTooSoon
	}
	return err
}

// sendCodeTx invalidates older codes of purpose, stores a new one (with the
// optional pending passwordHash) and enqueues msg to email, all inside tx.
// msg.Text must contain one %s, which is replaced by the code. The caller
// must hold the user row lock. It returns errCodeCapReached, writing nothing,
// when maxCodesPer24h codes of purpose were created in the last 24 hours:
// together with maxCodeAttempts this bounds guesses per account per day.
func (s *Service) sendCodeTx(ctx context.Context, tx pgx.Tx, q *store.Queries, userID uuid.UUID, email, purpose string, passwordHash *string, msg mailer.Message, now time.Time) error {
	n, err := q.CountCodesSince(ctx, store.CountCodesSinceParams{
		UserID: userID, Purpose: purpose, Since: now.Add(-codeCapWindow),
	})
	if err != nil {
		return fmt.Errorf("count codes: %w", err)
	}
	if n >= maxCodesPer24h {
		return errCodeCapReached
	}
	if err := q.InvalidateCodes(ctx, store.InvalidateCodesParams{
		Now: now, UserID: userID, Purpose: purpose,
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
		ID:           id,
		UserID:       userID,
		Purpose:      purpose,
		CodeHash:     hashCode(s.pepper, purpose, userID.String(), code),
		PasswordHash: passwordHash,
		ExpiresAt:    now.Add(codeTTL),
		Now:          now,
	}); err != nil {
		return fmt.Errorf("insert verification: %w", err)
	}
	msg.To = email
	msg.Text = fmt.Sprintf(msg.Text, code)
	// A code expires in codeTTL, so retrying its email for long is useless.
	msg.MaxAttempts = otpMailMaxAttempts
	return mailer.EnqueueTx(ctx, tx, s.jobs, msg)
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}

// errNotInternalRole is returned by CreateInternalUser for any role other
// than partner or super_admin. Customers only sign up publicly.
var errNotInternalRole = errors.New("identity: internal users must be partner or super_admin")

// dummyPasswordHash is a fixed argon2id hash that Login verifies against when
// there is no real hash to check, so response time does not reveal whether an
// email has an account.
var dummyPasswordHash = sync.OnceValue(func() string {
	h, err := HashPassword("lunain-dummy-password-0")
	if err != nil {
		panic(fmt.Sprintf("identity: hash dummy password: %v", err))
	}
	return h
})

// burnPasswordCheck spends the same argon2id work as a real password check.
func burnPasswordCheck(password string) {
	_, _ = VerifyPassword(dummyPasswordHash(), password)
}

// Login checks email and password and starts a new session (a new refresh
// token family). Unknown emails, wrong passwords and accounts without a
// password all fail with ErrInvalidCredentials. ErrEmailNotVerified and
// ErrAccountLocked are only reported once the password is known to be right.
// For an unverified signup the password is checked against the pending
// password on its latest signup code (consumed or not).
func (s *Service) Login(ctx context.Context, email, password string) (Tokens, error) {
	e, err := NormalizeEmail(email)
	if err != nil {
		burnPasswordCheck(password)
		return Tokens{}, ErrInvalidCredentials
	}
	q := store.New(s.pool)
	u, err := q.GetUserByEmail(ctx, e)
	if errors.Is(err, pgx.ErrNoRows) {
		burnPasswordCheck(password)
		return Tokens{}, ErrInvalidCredentials
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("get user: %w", err)
	}

	hash := u.PasswordHash
	if u.EmailVerifiedAt == nil {
		// A pending signup's password lives on its latest signup code.
		last, err := latestSignupCode(ctx, q, u.ID)
		if err != nil {
			return Tokens{}, err
		}
		hash = nil
		if last != nil {
			hash = last.PasswordHash
		}
	}
	if hash == nil {
		burnPasswordCheck(password)
		return Tokens{}, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(*hash, password)
	if err != nil {
		return Tokens{}, fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		return Tokens{}, ErrInvalidCredentials
	}
	if u.Status == userStatusLocked {
		return Tokens{}, ErrAccountLocked
	}
	if u.EmailVerifiedAt == nil {
		return Tokens{}, ErrEmailNotVerified
	}

	// The password was checked without holding a lock (argon2 is slow).
	// Re-check under the user lock that the account did not change meanwhile.
	var tokens Tokens
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		u, err := store.New(tx).GetUserByIDForUpdate(ctx, u.ID)
		if err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		if u.PasswordHash == nil || *u.PasswordHash != *hash || u.EmailVerifiedAt == nil {
			return ErrInvalidCredentials
		}
		if u.Status == userStatusLocked {
			return ErrAccountLocked
		}
		role := auth.Role(u.Role)
		if !role.Valid() {
			return fmt.Errorf("user %s has invalid role %q", u.ID, u.Role)
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
	return tokens, nil
}

// Refresh rotates a refresh token: the presented token is marked used and a
// new one in the same family is issued, with an access token carrying the
// user's current role. Presenting an already used token means it was copied
// (or sent twice), so its whole family is revoked. A locked user's family is
// revoked too. These revocations are committed even though an error is
// returned.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	if refreshToken == "" {
		return Tokens{}, ErrInvalidRefreshToken
	}
	// Unlocked lookup only to learn the user; state is re-checked under lock.
	found, err := store.New(s.pool).GetRefreshTokenByHash(ctx, hashRefreshToken(refreshToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return Tokens{}, ErrInvalidRefreshToken
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("get refresh token: %w", err)
	}
	now := s.clock.Now()

	var (
		tokens  Tokens
		outcome error // returned after the transaction commits
	)
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		// Lock order: user row first, then the token row.
		u, err := q.GetUserByIDForUpdate(ctx, found.UserID)
		if err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		t, err := q.GetRefreshTokenForUpdate(ctx, found.ID)
		if err != nil {
			return fmt.Errorf("lock refresh token: %w", err)
		}
		revokeFamily := func() error {
			return q.RevokeRefreshTokenFamily(ctx, store.RevokeRefreshTokenFamilyParams{Now: now, FamilyID: t.FamilyID})
		}
		switch {
		case t.RevokedAt != nil:
			outcome = ErrInvalidRefreshToken
			return nil
		case t.UsedAt != nil:
			outcome = ErrInvalidRefreshToken
			return revokeFamily()
		case !now.Before(t.ExpiresAt):
			outcome = ErrInvalidRefreshToken
			return nil
		case u.Status == userStatusLocked:
			outcome = ErrAccountLocked
			return revokeFamily()
		}
		role := auth.Role(u.Role)
		if !role.Valid() {
			return fmt.Errorf("user %s has invalid role %q", u.ID, u.Role)
		}
		if err := q.MarkRefreshTokenUsed(ctx, store.MarkRefreshTokenUsedParams{Now: now, ID: t.ID}); err != nil {
			return fmt.Errorf("mark refresh token used: %w", err)
		}
		tokens, err = s.issueSession(ctx, tx, u.ID, role, t.FamilyID)
		return err
	})
	if err != nil {
		return Tokens{}, err
	}
	if outcome != nil {
		return Tokens{}, outcome
	}
	return tokens, nil
}

// Logout revokes the whole family of refreshToken. Unknown or already
// revoked tokens are ignored, so Logout is idempotent.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	found, err := store.New(s.pool).GetRefreshTokenByHash(ctx, hashRefreshToken(refreshToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get refresh token: %w", err)
	}
	now := s.clock.Now()
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		// Lock order: user row first, then token rows.
		if _, err := q.GetUserByIDForUpdate(ctx, found.UserID); err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		return q.RevokeRefreshTokenFamily(ctx, store.RevokeRefreshTokenFamilyParams{Now: now, FamilyID: found.FamilyID})
	})
}

// RevokeAllSessions revokes every refresh token of userID inside tx. It locks
// the user row first (a no-op if tx already holds it), following the lock
// order rule, so it serializes with Refresh: a rotation in flight either
// commits before this runs (and its new token is revoked here) or waits and
// then sees its token revoked.
func (s *Service) RevokeAllSessions(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	q := store.New(tx)
	if _, err := q.GetUserByIDForUpdate(ctx, userID); err != nil {
		return fmt.Errorf("lock user: %w", err)
	}
	if err := q.RevokeUserRefreshTokens(ctx, store.RevokeUserRefreshTokensParams{
		Now: s.clock.Now(), UserID: userID,
	}); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	return nil
}

// CreateInternalUser creates an already verified partner or super_admin
// account with a password, inside the caller's tx (no customer profile). It
// returns ErrEmailTaken if the email is used by any account, verified or not.
func (s *Service) CreateInternalUser(ctx context.Context, tx pgx.Tx, email, password string, role auth.Role) (uuid.UUID, error) {
	if role != auth.RolePartner && role != auth.RoleSuperAdmin {
		return uuid.Nil, fmt.Errorf("%w: got %q", errNotInternalRole, role)
	}
	e, err := NormalizeEmail(email)
	if err != nil {
		return uuid.Nil, err
	}
	if err := ValidatePassword(password); err != nil {
		return uuid.Nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return uuid.Nil, fmt.Errorf("hash password: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	// ON CONFLICT DO NOTHING keeps the caller's tx usable on a duplicate.
	n, err := store.New(tx).InsertVerifiedUser(ctx, store.InsertVerifiedUserParams{
		ID: id, Email: e, PasswordHash: &hash, Now: s.clock.Now(), Role: string(role),
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert user: %w", err)
	}
	if n == 0 {
		return uuid.Nil, ErrEmailTaken
	}
	return id, nil
}
