package embed_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/embed"
)

func TestParseMerchantConfig(t *testing.T) {
	m, err := embed.ParseMerchantConfig([]byte(`
display_name: Cozy Art
profile:
  from_email: billing@example.com
rail_merchant_accounts:
  mobius:
    nmi:
      environment: live
      account_id: "579145"
  ccbill:
    ccbill:
      account_id: "945280/0000"
`))
	require.NoError(t, err)
	require.Equal(t, "Cozy Art", m.DisplayName)
	require.Equal(t, "billing@example.com", m.Profile.FromEmail)
	require.Len(t, m.RailMerchantAccounts, 2)
	require.Equal(t, "579145", m.RailMerchantAccounts["mobius"]["nmi"].AccountID)
	require.Equal(t, "945280/0000", m.RailMerchantAccounts["ccbill"]["ccbill"].AccountID)
}
