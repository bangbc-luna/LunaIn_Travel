package identity

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHashVerify_RoundTrip(t *testing.T) {
	h, err := HashPassword("abc12345")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$"), h)

	ok, err := VerifyPassword(h, "abc12345")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestVerify_WrongPassword(t *testing.T) {
	h, err := HashPassword("abc12345")
	require.NoError(t, err)

	ok, err := VerifyPassword(h, "abc12346")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestVerify_MalformedHash(t *testing.T) {
	_, err := VerifyPassword("not-a-hash", "abc12345")
	require.Error(t, err)
}

func TestHash_DifferentSalts(t *testing.T) {
	a, err := HashPassword("abc12345")
	require.NoError(t, err)
	b, err := HashPassword("abc12345")
	require.NoError(t, err)
	require.NotEqual(t, a, b)
}

func TestValidatePassword(t *testing.T) {
	require.NoError(t, ValidatePassword("abc12345"))
	require.ErrorIs(t, ValidatePassword("abcdefgh"), ErrWeakPassword)
	require.ErrorIs(t, ValidatePassword("1234567"), ErrWeakPassword)
	require.ErrorIs(t, ValidatePassword("12345678"), ErrWeakPassword)
	require.ErrorIs(t, ValidatePassword("a1"+strings.Repeat("x", 127)), ErrWeakPassword)
	require.NoError(t, ValidatePassword("a1"+strings.Repeat("x", 126)))
}

func TestNormalizeEmail(t *testing.T) {
	got, err := NormalizeEmail("  An@Gmail.COM ")
	require.NoError(t, err)
	require.Equal(t, "an@gmail.com", got)

	_, err = NormalizeEmail("not-an-email")
	require.ErrorIs(t, err, ErrInvalidEmail)

	_, err = NormalizeEmail("A <a@b.com>")
	require.ErrorIs(t, err, ErrInvalidEmail)

	_, err = NormalizeEmail("")
	require.ErrorIs(t, err, ErrInvalidEmail)
}

func TestArgonSemaphore_BoundsAndAllowsConcurrentVerify(t *testing.T) {
	require.GreaterOrEqual(t, cap(argonSem), 2)

	h, err := HashPassword("abc12345")
	require.NoError(t, err)
	const n = 16
	results := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = VerifyPassword(h, "abc12345")
		}()
	}
	wg.Wait()
	for i := range n {
		require.NoError(t, errs[i])
		require.True(t, results[i], "goroutine %d", i)
	}
	require.Zero(t, len(argonSem), "all slots released")
}
