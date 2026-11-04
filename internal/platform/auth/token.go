package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
)

const tokenIssuer = "lunain"

// ErrInvalidToken is returned by Verify for any unacceptable token.
var ErrInvalidToken = errors.New("invalid token")

type claims struct {
	Role Role `json:"role"`
	jwt.RegisteredClaims
}

// Issuer signs and verifies HS256 access tokens.
type Issuer struct {
	secret []byte
	ttl    time.Duration
	clock  clock.Clock
}

// NewIssuer creates an Issuer.
func NewIssuer(secret []byte, ttl time.Duration, c clock.Clock) *Issuer {
	return &Issuer{secret: secret, ttl: ttl, clock: c}
}

// Issue creates a signed access token for p and returns its expiry.
func (i *Issuer) Issue(p Principal) (string, time.Time, error) {
	now := i.clock.Now()
	exp := now.Add(i.ttl)
	cl := claims{
		Role: p.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   p.UserID.String(),
			Issuer:    tokenIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, cl).SignedString(i.secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return s, exp, nil
}

// Verify validates token and returns its Principal, or ErrInvalidToken.
func (i *Issuer) Verify(token string) (Principal, error) {
	var cl claims
	_, err := jwt.ParseWithClaims(token, &cl, func(*jwt.Token) (any, error) {
		return i.secret, nil
	},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithTimeFunc(i.clock.Now),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return Principal{}, ErrInvalidToken
	}
	id, err := uuid.Parse(cl.Subject)
	if err != nil || !cl.Role.Valid() {
		return Principal{}, ErrInvalidToken
	}
	return Principal{UserID: id, Role: cl.Role}, nil
}
