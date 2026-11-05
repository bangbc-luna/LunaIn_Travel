package validate_test

import (
	"strings"
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

func TestNormalizeAccountHolder(t *testing.T) {
	require.Equal(t, "NGUYEN VAN DUNG", validate.NormalizeAccountHolder("  Nguyễn   Văn Dũng "))
	require.Equal(t, "DANG THI DIEP", validate.NormalizeAccountHolder("Đặng thị điệp"))
}

func TestNormalizeBankAccount(t *testing.T) {
	got, msg := validate.NormalizeBankAccount(" 970436 ", " 0011001234567 ", "trần  thị bé")
	require.Empty(t, msg)
	require.Equal(t, validate.BankAccount{BIN: "970436", Number: "0011001234567", Holder: "TRAN THI BE"}, got)

	for _, c := range []struct{ bin, number, holder, field string }{
		{"97043", "0011001234567", "An", "bank_bin"},
		{"97043a", "0011001234567", "An", "bank_bin"},
		{"970436", "12345", "An", "account_number"},
		{"970436", "12345678901234567890", "An", "account_number"},
		{"970436", "12345a78", "An", "account_number"},
		{"970436", "123456", "   ", "account_holder"},
		{"970436", "123456", strings.Repeat("A", 201), "account_holder"},
	} {
		_, msg := validate.NormalizeBankAccount(c.bin, c.number, c.holder)
		require.Contains(t, msg, c.field, c)
	}
}
