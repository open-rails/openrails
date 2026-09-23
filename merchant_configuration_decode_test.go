package openrails

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMerchantConfigurationDocument(t *testing.T) {
	valid := "application_id: initial\nexpected_revision: revision\ndisplay_name: Shop\nsettings:\n  profile:\n    support_url: https://help.example.test\n"
	params, err := ParseMerchantConfigurationYAML([]byte(valid))
	require.NoError(t, err)
	require.Equal(t, "https://help.example.test", params.Settings.Profile.SupportURL)
	for _, document := range []string{
		valid + "display_name: Duplicate\n",
		valid + "unexpected: true\n",
		valid + "other: &anchor value\n",
		valid + "---\napplication_id: extra\n",
		"application_id: missing-revision\n",
		strings.Repeat(" ", MaxMerchantConfigurationBytes+1),
		`{"application_id":"a","application_id":"b","expected_revision":"r"}`,
		`{"application_id":"a","expected_revision":"r","settings":{"profile":{"unknown":1}}}`,
	} {
		_, err := ParseMerchantConfigurationYAML([]byte(document))
		require.Error(t, err)
	}
	_, err = ParseMerchantConfigurationJSON([]byte(`{"application_id":"a","application_id":"b","expected_revision":"r"}`))
	require.Error(t, err)
}

func TestMerchantConfigurationExplicitEmptyListsSurviveTransport(t *testing.T) {
	revision := "before"
	params := MerchantConfigurationApplyParams{ApplicationID: "clear", ExpectedRevision: &revision, Settings: &MerchantSettings{BillingPolicies: []BillingPolicyInput{}, BillingPolicyBindings: []BillingPolicyBindingInput{}, DelegatedInvokerWastedSpendLimits: []BudgetWindowInput{}}}
	amount := int64(9007199254740993)
	params.Settings.InvoiceCollectionThreshold = &amount
	body, err := json.Marshal(params)
	require.NoError(t, err)
	require.Contains(t, string(body), `"collection_threshold":"9007199254740993"`)
	decoded, err := ParseMerchantConfigurationJSON(body)
	require.NoError(t, err)
	require.Equal(t, amount, *decoded.Settings.InvoiceCollectionThreshold)
	require.NotNil(t, decoded.Settings.BillingPolicies)
	require.NotNil(t, decoded.Settings.BillingPolicyBindings)
	require.NotNil(t, decoded.Settings.DelegatedInvokerWastedSpendLimits)
	params.Settings = &MerchantSettings{}
	body, err = json.Marshal(params)
	require.NoError(t, err)
	decoded, err = ParseMerchantConfigurationJSON(body)
	require.NoError(t, err)
	require.Nil(t, decoded.Settings.BillingPolicies)
}
