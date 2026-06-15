package service

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/modules/money"
)

func requireCurrency(currency string) (string, error) {
	currency = strings.TrimSpace(currency)
	if currency == "" {
		return "", fmt.Errorf("currency required")
	}
	if money.IsQualifiedUnit(currency) {
		return currency, nil
	}
	return money.NormalizeCurrency(currency), nil
}
