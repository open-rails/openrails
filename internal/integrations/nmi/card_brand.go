package nmi

import (
	"strings"
	"unicode"

	"github.com/open-rails/openrails/internal/cardguard"
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
	return cardguard.BrandFromLeadingDigits(lead.String())
}
