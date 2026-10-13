package openrails

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

func requireFixture(t *testing.T, name string, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	if *updateWireFixtures {
		require.NoError(t, os.WriteFile("testdata/wire/"+name, append(raw, '\n'), 0o644))
	}
	fixture, err := os.ReadFile("testdata/wire/" + name)
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(string(fixture)), string(raw), "%s drifted from the Go contract", name)
	return raw
}

func TestCurrencyRegistry(t *testing.T) {
	requireFixture(t, "currencies.json", billing.Currencies())
	usd, ok := billing.LookupCurrency(" usd ")
	require.True(t, ok)
	require.Equal(t, billing.CurrencyUnits{Code: "USD", Decimals: 6, MinorDecimals: 2}, usd)
	require.Equal(t, 4, usd.NativeShift())
	jpy, _ := billing.LookupCurrency("JPY")
	require.Equal(t, 4, jpy.NativeShift(), "zero-decimal rails still scale into native units")
	_, ok = billing.LookupCurrency("XYZ")
	require.False(t, ok, "a scale is never guessed")
}

func TestUsageOrderMoneyWireIsLossless(t *testing.T) {
	for _, amount := range []int64{math.MinInt64, -9007199254740993, 0, 9007199254740993, math.MaxInt64} {
		order := billing.Order{Total: amount, Lines: []billing.OrderLine{{Amount: amount, UnitAmount: amount}}, Usage: &billing.OrderUsage{AmountPaid: amount, AmountDue: amount}}
		raw, err := json.Marshal(order)
		require.NoError(t, err)
		var browser map[string]any
		require.NoError(t, json.Unmarshal(raw, &browser))
		require.IsType(t, "", browser["total"])
		require.IsType(t, "", browser["usage"].(map[string]any)["amount_due"])
		var read billing.Order
		require.NoError(t, json.Unmarshal(raw, &read))
		require.Equal(t, order.Total, read.Total)
		require.Equal(t, *order.Usage, *read.Usage)
		require.Equal(t, amount, read.Lines[0].Amount)
	}
}

func TestMerchantConfigurationDocument(t *testing.T) {
	valid := "expected_revision: 3\ndisplay_name: Shop\nprofile:\n  support_url: https://help.example.test\n"
	params, err := billing.ParseMerchantConfigurationYAML([]byte(valid))
	require.NoError(t, err)
	require.Equal(t, "https://help.example.test", params.Profile.SupportURL)
	for _, document := range []string{
		valid + "display_name: Duplicate\n",
		valid + "unexpected: true\n",
		valid + "other: &anchor value\n",
		valid + "---\ndisplay_name: extra\n",
		"expected_revision: -1\n",
		strings.Repeat(" ", billing.MaxMerchantConfigurationBytes+1),
		`{"display_name":"a","display_name":"b"}`,
		`{"expected_revision":1,"profile":{"unknown":1}}`,
		`{"expected_revision":1,"settings":{"profile":{}}}`,
	} {
		_, err := billing.ParseMerchantConfigurationYAML([]byte(document))
		require.Error(t, err, document)
	}
}

// Explicit empty lists mean "clear"; absent lists mean "unchanged".
func TestMerchantConfigurationEmptyListsSurviveTransport(t *testing.T) {
	revision, amount := int64(2), int64(9007199254740993)
	params := billing.UpdateMerchantConfigurationParams{ExpectedRevision: &revision, MerchantSettings: &billing.MerchantSettings{
		CollectionThreshold: &amount,
		BillingPolicies:     []billing.BillingPolicy{}, BillingPolicyBindings: []billing.BillingPolicyBinding{}, DelegatedInvokerWastedSpendLimits: []billing.BudgetWindow{},
	}}
	body, err := json.Marshal(params)
	require.NoError(t, err)
	require.Contains(t, string(body), `"collection_threshold":"9007199254740993"`)
	decoded, err := billing.ParseMerchantConfigurationYAML(body)
	require.NoError(t, err)
	require.Equal(t, amount, *decoded.CollectionThreshold)
	require.NotNil(t, decoded.BillingPolicies)
	require.NotNil(t, decoded.BillingPolicyBindings)
	require.NotNil(t, decoded.DelegatedInvokerWastedSpendLimits)

	params.MerchantSettings = &billing.MerchantSettings{}
	body, err = json.Marshal(params)
	require.NoError(t, err)
	decoded, err = billing.ParseMerchantConfigurationYAML(body)
	require.NoError(t, err)
	require.Nil(t, decoded.MerchantSettings, "no settings named, none changed")
}
