package config

import (
	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestUsageBillingYAMLConfiguration(t *testing.T) {
	var host Config
	require.NoError(t, yaml.UnmarshalWithOptions([]byte("usage_billing_enabled: true\n"), &host, yaml.DisallowUnknownField()))
	require.True(t, host.UsageBillingEnabled)
	merchant, err := ParseMerchantDeclaration([]byte(`slug: api
usage_billing:
  billing_day: 13
  currencies:
    - {currency: USD, credit_limit: 100000000, minimum_invoice_amount: 5000000}
`))
	require.NoError(t, err)
	require.NotNil(t, merchant.UsageBilling)
	require.Equal(t, 13, merchant.UsageBilling.BillingDay)
	require.EqualValues(t, 100_000_000, merchant.UsageBilling.Currencies[0].CreditLimit)
}
