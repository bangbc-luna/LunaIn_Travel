package validate

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// MaxAccountHolderRunes bounds a normalized account holder name.
const MaxAccountHolderRunes = 200

var (
	bankBINRe       = regexp.MustCompile(`^[0-9]{6}$`)
	accountNumberRe = regexp.MustCompile(`^[0-9]{6,19}$`)
	holderRe        = regexp.MustCompile(`^[A-Z]+( [A-Z]+)*$`)
)

// BankAccount is a normalized Vietnamese bank account (partner payouts and
// customer refunds).
type BankAccount struct {
	BIN    string // 6 digits (NAPAS bank BIN)
	Number string // 6 to 19 digits
	Holder string // upper case, without diacritics
}

// NormalizeAccountHolder renders a name the way it is printed on a bank card:
// upper case without Vietnamese diacritics (đ becomes D) and with runs of
// whitespace collapsed to one space.
func NormalizeAccountHolder(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case r == 'đ' || r == 'Đ':
			r = 'D'
		default:
			r = unicode.ToUpper(r)
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// NormalizeBankAccount trims the BIN and the account number and normalizes
// the holder name. It returns a Vietnamese message naming the first invalid
// field, or "" when the account is valid.
func NormalizeBankAccount(bin, number, holder string) (BankAccount, string) {
	out := BankAccount{
		BIN:    strings.TrimSpace(bin),
		Number: strings.TrimSpace(number),
		Holder: NormalizeAccountHolder(holder),
	}
	switch {
	case !bankBINRe.MatchString(out.BIN):
		return out, "bank_bin phải gồm đúng 6 chữ số"
	case !accountNumberRe.MatchString(out.Number):
		return out, "account_number phải gồm 6 đến 19 chữ số"
	case out.Holder == "" || utf8.RuneCountInString(out.Holder) > MaxAccountHolderRunes:
		return out, "account_holder không được để trống (tối đa 200 ký tự)"
	case !holderRe.MatchString(out.Holder):
		return out, "account_holder chỉ gồm chữ cái A–Z không dấu và khoảng trắng"
	}
	return out, ""
}
