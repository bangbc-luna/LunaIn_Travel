package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
	"time"
)

const (
	codeTTL         = 10 * time.Minute
	maxCodeAttempts = 5
	resendCooldown  = 60 * time.Second
	// maxCodesPer24h caps new codes per user and purpose within codeCapWindow,
	// so an attacker gets at most maxCodesPer24h*maxCodeAttempts guesses/day.
	maxCodesPer24h = 5
	codeCapWindow  = 24 * time.Hour
	// otpMailMaxAttempts is the River delivery attempts for code emails.
	otpMailMaxAttempts = 3

	purposeSignup        = "signup"
	purposePasswordReset = "password_reset"
)

var codeSpace = big.NewInt(1_000_000)

// newCode returns a uniformly random 6-digit code from crypto/rand.
func newCode() (string, error) {
	n, err := rand.Int(rand.Reader, codeSpace)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// hashCode binds a code to its purpose and user with HMAC-SHA256 keyed by
// the server-side pepper, so a leaked table cannot be brute-forced offline.
func hashCode(pepper []byte, purpose, userID, code string) []byte {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(purpose))
	mac.Write([]byte{0})
	mac.Write([]byte(userID))
	mac.Write([]byte{0})
	mac.Write([]byte(code))
	return mac.Sum(nil)
}
