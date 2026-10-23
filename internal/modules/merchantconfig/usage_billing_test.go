package merchantconfig

import (
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestUsageBillingConfiguration(t *testing.T) {
	valid := billing.UsageBillingSettings{BillingDay: 13, Currencies: []billing.UsageBillingCurrency{{Currency: "usd", CreditLimit: 100_000_000, MinimumInvoiceAmount: 5_000_000}}}
	normalized, err := NormalizeUsageBilling(valid)
	require.NoError(t, err)
	require.Equal(t, "USD", normalized.Currencies[0].Currency)
	require.Equal(t, "usd", valid.Currencies[0].Currency, "validation must not mutate its caller")
	for _, change := range []func(*billing.UsageBillingSettings){
		func(s *billing.UsageBillingSettings) { s.BillingDay = 32 },
		func(s *billing.UsageBillingSettings) { s.Currencies[0].CreditLimit = -1 },
		func(s *billing.UsageBillingSettings) { s.Currencies[0].MinimumInvoiceAmount = 101_000_000 },
		func(s *billing.UsageBillingSettings) { s.Currencies = append(s.Currencies, s.Currencies[0]) },
	} {
		copy := normalized
		copy.Currencies = append([]billing.UsageBillingCurrency(nil), normalized.Currencies...)
		change(&copy)
		_, err := NormalizeUsageBilling(copy)
		require.Error(t, err)
	}
}
