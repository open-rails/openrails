package service

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// requireCurrency validates and normalizes a supported currency.
func requireCurrency(currency string) (string, error) {
	currency = strings.TrimSpace(currency)
	if currency == "" {
		return "", fmt.Errorf("currency required")
	}
	normalized := moneyutil.NormalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(normalized); err != nil {
		return "", err
	}
	return normalized, nil
}
