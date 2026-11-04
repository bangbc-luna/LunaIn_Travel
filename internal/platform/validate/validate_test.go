package validate_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/validate"
)

func TestNormalizePhone(t *testing.T) {
	for in, want := range map[string]string{
		"0901234567":       "0901234567",
		" 090 123 4567 ":   "0901234567",
		"090.123-4567":     "0901234567",
		"+84901234567":     "0901234567",
		"+84 90 123 45 67": "0901234567",
	} {
		got, ok := validate.NormalizePhone(in)
		require.True(t, ok, in)
		require.Equal(t, want, got, in)
	}
	for _, in := range []string{"", "12345", "1901234567", "090123456", "09012345678", "090123456a", "+8490123456"} {
		_, ok := validate.NormalizePhone(in)
		require.False(t, ok, in)
	}
}
