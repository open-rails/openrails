package solana

import (
	"fmt"
	"strings"

	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ValidateQuoteCurrency checks the denomination before any mint or price lookup.
// Fiat prices may be converted; a token-denominated price pays in that same
// registered token. A familiar symbol on an unrelated mint is never sufficient.
func ValidateQuoteCurrency(currency, symbol, mint string) error {
	units, ok := moneyutil.LookupCurrency(currency)
	if !ok {
		return fmt.Errorf("unknown quote currency %q", currency)
	}
	if units.Kind == "fiat" {
		return nil
	}
	if units.Code != strings.ToUpper(strings.TrimSpace(symbol)) {
		return fmt.Errorf("%s prices require payment in %s; cross-token conversion is not supported", units.Code, units.Code)
	}
	for _, registered := range []string{solanatokens.DefaultSupportedTokens()[units.Code].Mint, solanatokens.DefaultDevnetTokens()[units.Code].Mint} {
		if registered != "" && registered == strings.TrimSpace(mint) {
			return nil
		}
	}
	return fmt.Errorf("%s price requires its registered token mint", units.Code)
}
