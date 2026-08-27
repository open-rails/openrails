package checkout

import (
	"fmt"
	"net/mail"
	"strings"
)

// validateCCBillBillingIdentity validates only the customer data CCBill needs
// before opening its hosted credit-card FlexForm. Street, city, and state are
// optional in that hosted CCBill contract and must not be replaced with
// placeholders.
func validateCCBillBillingIdentity(name, postalCode, country string, user *UserIdentity) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name_on_card is required for CCBill payments")
	}
	if strings.TrimSpace(postalCode) == "" {
		return fmt.Errorf("zip is required for CCBill payments")
	}
	country = strings.TrimSpace(country)
	if len(country) != 2 || !isASCIIAlpha(country[0]) || !isASCIIAlpha(country[1]) {
		return fmt.Errorf("country must be ISO-3166 alpha-2 for CCBill payments")
	}
	if user == nil || user.Email == nil || strings.TrimSpace(*user.Email) == "" {
		return fmt.Errorf("verified email required for CCBill payments")
	}
	if _, err := mail.ParseAddress(strings.TrimSpace(*user.Email)); err != nil {
		return fmt.Errorf("verified email is invalid for CCBill payments")
	}
	return nil
}

func isASCIIAlpha(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}
