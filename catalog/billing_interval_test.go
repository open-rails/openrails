package catalog

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBillingIntervalUnits(t *testing.T) {
	for _, test := range []struct {
		value         string
		hours, months int
	}{
		{"30 days", 720, 0}, {"1 month", 0, 1}, {"2 months", 0, 2}, {"1 year", 0, 12}, {"365 days", 8760, 0},
	} {
		hours, months, err := ParseBillingInterval(test.value)
		require.NoError(t, err)
		require.Equal(t, test.hours, hours)
		require.Equal(t, test.months, months)
	}
	for _, text := range []string{"0 months", "-1 month", "1.5 months", "1201 months", "101 years"} {
		_, _, err := ParseBillingInterval(text)
		require.Error(t, err)
	}
	var price ApplyPrice
	require.NoError(t, json.Unmarshal([]byte(`{"billing_interval":"1 month"}`), &price))
	require.Equal(t, Value(1), price.BillingIntervalMonths)
	require.True(t, price.BillingIntervalHours.Null)
	require.Error(t, json.Unmarshal([]byte(`{"billing_interval":"1 month","billing_interval_hours":720}`), &price))
	require.Error(t, json.Unmarshal([]byte(`{"billing_interval":"30 days","billing_interval_months":1}`), &price))
	// Keep the normalized document of old fixed-duration aliases unchanged.
	var text, numeric ApplyPrice
	require.NoError(t, json.Unmarshal([]byte(`{"billing_interval":"30 days"}`), &text))
	require.NoError(t, json.Unmarshal([]byte(`{"billing_interval_hours":720}`), &numeric))
	require.Equal(t, numeric, text)
}
