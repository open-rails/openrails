package service

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// requireCurrency is the service-entry-point currency gate. or#864: it used to
// check emptiness only, so "XYZ" passed. It now consults the registry — but
// ONLY for unqualified codes: a qualified custom-credit unit (merchant/name,
// #475) is deliberately not an ISO currency and must keep passing through.
func requireCurrency(currency string) (string, error) {
	currency = strings.TrimSpace(currency)
	if currency == "" {
		return "", fmt.Errorf("currency required")
	}
	if money.IsQualifiedUnit(currency) {
		return currency, nil
	}
	normalized := moneyutil.NormalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(normalized); err != nil {
		return "", err
	}
	return normalized, nil
}
