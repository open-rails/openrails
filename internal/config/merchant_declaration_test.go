package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A typo'd key must fail loudly instead of declaring a merchant with no PSPs.
func TestParseMerchantDeclarationIsStrict(t *testing.T) {
	m, err := ParseMerchantDeclaration([]byte(`
display_name: Host One
psps:
  mobius:
    nmi:
      account_id: "100001"
      secrets: {security_key: sk, webhook_signing_secret: whs}
`))
	require.NoError(t, err)
	require.Equal(t, "100001", m.PSPs["mobius"]["nmi"].AccountID)

	for doc, want := range map[string]string{
		"display_name: X\nacounts: {}\n":                "acounts",
		"display_name: X\nrail_merchant_accounts: {}\n": "rail_merchant_accounts was renamed to psps",
		"display_name: X\nprovider_accounts: {}\n":      "provider_accounts was renamed to psps",
		"slug: x\n": "slug",
	} {
		_, err := ParseMerchantDeclaration([]byte(doc))
		require.ErrorContains(t, err, want)
	}
}
