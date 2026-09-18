package service

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ErrCurrencyUnsupported refuses a currency outside the supported registry.
var ErrCurrencyUnsupported = apperr.New(http.StatusBadRequest, "currency_unsupported", "unknown currency")

// requireCurrency validates and normalizes a supported currency.
func requireCurrency(currency string) (string, error) {
	currency = strings.TrimSpace(currency)
	if currency == "" {
		return "", apperr.Invalidf("currency required")
	}
	normalized := moneyutil.NormalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(normalized); err != nil {
		return "", fmt.Errorf("%w: %q", ErrCurrencyUnsupported.WithParam("currency"), normalized)
	}
	return normalized, nil
}
