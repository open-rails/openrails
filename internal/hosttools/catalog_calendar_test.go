package hosttools

import (
	"testing"

	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestCalendarCatalogDumpCanBeApplied(t *testing.T) {
	document := &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{
		"membership": {DisplayName: catalog.Value("Membership"), Prices: map[string]catalog.ApplyPrice{
			"monthly": {Currency: catalog.Value("USD"), UnitAmount: catalog.Value(int64(20_000_000)), BillingIntervalHours: catalog.Null[int](), BillingIntervalMonths: catalog.Value(1), AccessDurationHours: catalog.Null[int]()},
		}},
	}}
	yaml, err := readableCatalogYAML(document)
	require.NoError(t, err)
	require.Contains(t, string(yaml), "billing_interval: 1 month")
	require.NotContains(t, string(yaml), "access_duration: forever")
	parsed, err := catalog.ParseApplicationYAML(yaml)
	require.NoError(t, err)
	price := parsed.Products["membership"].Prices["monthly"]
	require.Equal(t, catalog.Value(1), price.BillingIntervalMonths)
	require.True(t, price.BillingIntervalHours.Null)
	require.False(t, price.AccessDurationHours.Set, "calendar access still follows each actual period")
}
