package handlers

import "github.com/open-rails/openrails/internal/shared/moneyutil"

func currencyDecimals(currency string) int {
	if scale, ok := moneyutil.CurrencyScale(currency); ok {
		return scale
	}
	return 0
}
