package merchantconfig

import (
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func NormalizeUsageBilling(in billing.UsageBillingSettings) (billing.UsageBillingSettings, error) {
	if in.BillingDay == 0 {
		in.BillingDay = 1
	}
	if in.BillingDay < 1 || in.BillingDay > 31 {
		return in, fmt.Errorf("usage_billing.billing_day must be between 1 and 31")
	}
	in.Currencies = append([]billing.UsageBillingCurrency(nil), in.Currencies...)
	seen := map[string]bool{}
	for i := range in.Currencies {
		c := &in.Currencies[i]
		c.Currency = moneyutil.NormalizeCurrency(c.Currency)
		if err := moneyutil.ValidateCurrency(c.Currency); err != nil {
			return in, fmt.Errorf("usage_billing.currencies[%d].currency: %w", i, err)
		}
		if seen[c.Currency] {
			return in, fmt.Errorf("usage_billing.currencies names %s twice", c.Currency)
		}
		seen[c.Currency] = true
		if c.CreditLimit < 0 || c.MinimumInvoiceAmount < 0 {
			return in, fmt.Errorf("usage_billing amounts must be nonnegative")
		}
		if c.CreditLimit > 0 && c.MinimumInvoiceAmount > c.CreditLimit {
			return in, fmt.Errorf("usage_billing minimum_invoice_amount cannot exceed credit_limit")
		}
	}
	return in, nil
}
