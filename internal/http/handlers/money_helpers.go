package handlers

import "github.com/open-rails/openrails/internal/modules/money"

func currencyDecimals(currency string) int {
	if scale, ok := money.CurrencyScale(currency); ok {
		return scale
	}
	return 0
}
