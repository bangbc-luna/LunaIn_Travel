package text_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/platform/text"
)

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Thành phố Hồ Chí Minh":    "thanh-pho-ho-chi-minh",
		"Tỉnh Đắk Lắk":             "tinh-dak-lak",
		"Đặc khu Bạch Long Vĩ":     "dac-khu-bach-long-vi",
		"  Xã  Ia H'Drai ":         "xa-ia-h-drai",
		"Phường 1":                 "phuong-1",
		"Vé VinWonders Nha Trang!": "ve-vinwonders-nha-trang",
		"!!!":                      "",
		"":                         "",
	}
	for in, want := range cases {
		require.Equal(t, want, text.Slug(in), in)
	}
}
