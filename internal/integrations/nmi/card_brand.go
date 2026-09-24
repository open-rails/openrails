package nmi

import (
	"strings"
	"unicode"
)

// CardBrandFromMaskedPAN names the card network from the leading digits NMI
// leaves unmasked ("" when they do not identify one).
func CardBrandFromMaskedPAN(masked string) string {
	var lead strings.Builder
	for _, r := range strings.TrimSpace(masked) {
		if !unicode.IsDigit(r) {
			break
		}
		lead.WriteRune(r)
	}
	bin := lead.String()
	prefix := func(n int) int {
		if len(bin) < n {
			return -1
		}
		v := 0
		for _, r := range bin[:n] {
			v = v*10 + int(r-'0')
		}
		return v
	}
	switch p1, p2, p3, p4 := prefix(1), prefix(2), prefix(3), prefix(4); {
	case p1 == 4:
		return "visa"
	case p2 == 34 || p2 == 37:
		return "amex"
	case p2 >= 51 && p2 <= 55, p4 >= 2221 && p4 <= 2720:
		return "mastercard"
	case p4 == 6011, p2 == 65, p3 >= 644 && p3 <= 649:
		return "discover"
	case p2 == 35:
		return "jcb"
	case p2 == 36 || p2 == 38 || (p3 >= 300 && p3 <= 305):
		return "diners"
	}
	return ""
}
