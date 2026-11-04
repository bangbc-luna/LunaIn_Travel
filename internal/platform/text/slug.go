// Package text holds text helpers shared by several modules.
package text

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Slug lowercases s, strips Vietnamese diacritics (đ becomes d), and joins
// the remaining runs of ASCII letters and digits with "-".
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case r == 'đ' || r == 'Đ':
			r = 'd'
		default:
			r = unicode.ToLower(r)
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
	}
	return b.String()
}
