package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A typo'd key must fail loudly instead of declaring a merchant with no PSPs,
// and the document names its merchant.
func TestParseMerchantDeclarationIsStrict(t *testing.T) {
	m, err := ParseMerchantDeclaration([]byte(`
slug: Host-One
display_name: Host One
psps:
  mobius:
    rail: nmi
    account_id: "100001"
    secrets: {security_key: sk, webhook_signing_secret: whs}
`))
	require.NoError(t, err)
	require.Equal(t, "host-one", m.Slug)
	require.Equal(t, "100001", m.PSPs["mobius"].AccountID)

	for doc, want := range map[string]string{
		"slug: x\ndisplay_name: X\nacounts: {}\n":                "acounts",
		"slug: x\ndisplay_name: X\nrail_merchant_accounts: {}\n": "rail_merchant_accounts was renamed to psps",
		"slug: x\ndisplay_name: X\nprovider_accounts: {}\n":      "provider_accounts was renamed to psps",
		"display_name: X\n":              "slug is required",
		"slug: \" \"\ndisplay_name: X\n": "slug is required",
		"slug: -x-\ndisplay_name: X\n":   "invalid merchant slug",
	} {
		_, err := ParseMerchantDeclaration([]byte(doc))
		require.ErrorContains(t, err, want)
	}
}
