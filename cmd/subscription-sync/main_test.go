package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeProcessorListUsesFallbackOnlyWhenExplicitListEmpty(t *testing.T) {
	require.Equal(t, []string{"ccbill"}, normalizeProcessorList([]string{"ccbill"}, "mobius"))
	require.Equal(t, []string{"ccbill", "mobius"}, normalizeProcessorList([]string{"ccbill,mobius"}, "stripe"))
	require.Equal(t, []string{"mobius"}, normalizeProcessorList(nil, "mobius"))
}

func TestParseRecurringSubscriptionsRejectsNMIErrorResponses(t *testing.T) {
	_, err := parseRecurringSubscriptions(`<nm_response><response>3</response><responsetext>Invalid Security Key</responsetext></nm_response>`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Invalid Security Key")

	_, err = parseRecurringSubscriptions(`<nm_response><error>Invalid report request</error></nm_response>`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Invalid report request")
}

func TestParseRecurringSubscriptionsAcceptsSuccessfulResponses(t *testing.T) {
	subs, err := parseRecurringSubscriptions(`<nm_response><response>1</response><subscription><subscription_id> 123 </subscription_id></subscription><subscription><subscription_id></subscription_id></subscription></nm_response>`)
	require.NoError(t, err)
	require.Equal(t, []string{"123"}, subs)
}

func TestRefuseEmptyRemoteApply(t *testing.T) {
	err := refuseEmptyRemoteApply("ccbill", map[string]struct{}{}, []string{"0125217202000000017"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "remote active set is empty")

	err = refuseEmptyRemoteApply("ccbill", map[string]struct{}{"0125217202000000017": {}}, []string{"other"})
	require.NoError(t, err)

	err = refuseEmptyRemoteApply("ccbill", map[string]struct{}{}, nil)
	require.NoError(t, err)
}
