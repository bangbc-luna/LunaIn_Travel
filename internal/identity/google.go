package identity

import "context"

// GoogleIdentity is the verified content of a Google ID token.
type GoogleIdentity struct {
	Subject       string
	Email         string
	EmailVerified bool
}

// GoogleVerifier validates a Google ID token and returns its identity.
type GoogleVerifier interface {
	Verify(ctx context.Context, idToken string) (GoogleIdentity, error)
}
