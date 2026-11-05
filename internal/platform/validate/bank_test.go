package validate_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/validate"
)

func TestNormalizeBankAccount_HolderLettersOnly(t *testing.T) {
	got, msg := validate.NormalizeBankAccount("970436", "0011001234567", "Nguyễn Văn Đức")
	require.Empty(t, msg)
	require.Equal(t, "NGUYEN VAN DUC", got.Holder)

	for _, holder := range []string{"NGUYEN VAN A2", "CTY TNHH. ABC", "O'NEIL"} {
		_, msg := validate.NormalizeBankAccount("970436", "0011001234567", holder)
		require.Equal(t, "account_holder chỉ gồm chữ cái A–Z không dấu và khoảng trắng", msg, holder)
	}
}
