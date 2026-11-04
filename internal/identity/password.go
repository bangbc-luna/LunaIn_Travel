package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (OWASP-style: 64 MiB, 3 passes, 2 lanes).
const (
	argonMemory  uint32 = 64 * 1024
	argonTime    uint32 = 3
	argonThreads uint8  = 2
	argonSaltLen        = 16
	argonKeyLen  uint32 = 32

	minPasswordLen = 8
	maxPasswordLen = 128
	maxEmailLen    = 254
)

var errMalformedHash = errors.New("identity: malformed password hash")

// HashPassword hashes pw with argon2id and returns a PHC string
// "$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>".
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether pw matches the PHC-encoded argon2id hash.
// It returns an error only when encoded is malformed.
func VerifyPassword(encoded, pw string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errMalformedHash
	}
	var (
		m, t uint32
		p    uint8
	)
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m == 0 || t == 0 || p == 0 {
		return false, errMalformedHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false, errMalformedHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errMalformedHash
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// ValidatePassword requires 8–128 characters with at least one letter and
// one digit.
func ValidatePassword(pw string) error {
	if !utf8.ValidString(pw) {
		return ErrWeakPassword
	}
	n := utf8.RuneCountInString(pw)
	if n < minPasswordLen || n > maxPasswordLen {
		return ErrWeakPassword
	}
	var hasLetter, hasDigit bool
	for _, r := range pw {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	if !hasLetter || !hasDigit {
		return ErrWeakPassword
	}
	return nil
}

// NormalizeEmail trims and lowercases s and requires it to be a bare address
// (no display name, comments or angle brackets).
func NormalizeEmail(s string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(s))
	if e == "" || len(e) > maxEmailLen {
		return "", ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e {
		return "", ErrInvalidEmail
	}
	return e, nil
}
