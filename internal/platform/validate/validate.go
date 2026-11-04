// Package validate holds input normalization rules shared by several modules.
package validate

import (
	"regexp"
	"strings"
)

var (
	phoneRe    = regexp.MustCompile(`^0\d{9}$`)
	phoneStrip = strings.NewReplacer(" ", "", ".", "", "-", "")
)

// NormalizePhone normalizes a Vietnamese phone number: spaces, dots and
// dashes are removed and a +84 prefix becomes 0. It reports false unless the
// result is 10 digits starting with 0.
func NormalizePhone(s string) (string, bool) {
	s = phoneStrip.Replace(strings.TrimSpace(s))
	if rest, ok := strings.CutPrefix(s, "+84"); ok {
		s = "0" + rest
	}
	if !phoneRe.MatchString(s) {
		return "", false
	}
	return s, true
}
