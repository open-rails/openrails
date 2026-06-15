package service

import (
	"fmt"
	"strings"
)

func requireCurrency(currency string) (string, error) {
	currency = strings.TrimSpace(currency)
	if currency == "" {
		return "", fmt.Errorf("currency required")
	}
	return currency, nil
}
