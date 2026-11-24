package merchantconfig

import (
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
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
		c.Currency = catalog.Currency(moneyutil.NormalizeCurrency(string(c.Currency)))
		if err := moneyutil.ValidateCurrency(string(c.Currency)); err != nil {
			return in, fmt.Errorf("usage_billing.currencies[%d].currency: %w", i, err)
		}
		if seen[string(c.Currency)] {
			return in, fmt.Errorf("usage_billing.currencies names %s twice", c.Currency)
		}
		seen[string(c.Currency)] = true
		if c.CreditLimit < 0 || c.MinimumInvoiceAmount < 0 || (c.DelinquencyFloor != nil && *c.DelinquencyFloor < 0) {
			return in, fmt.Errorf("usage_billing amounts must be nonnegative")
		}
		if c.CreditLimit > 0 && c.MinimumInvoiceAmount > c.CreditLimit {
			return in, fmt.Errorf("usage_billing minimum_invoice_amount cannot exceed credit_limit")
		}
	}
	return in, nil
}
