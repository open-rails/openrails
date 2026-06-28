package embed_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/embed"
)

func TestParseMerchantConfig(t *testing.T) {
	mc, err := embed.ParseMerchantConfig([]byte(`
profile:
  display_name: Cozy Art
  from_email: billing@example.com
provider_accounts:
  - provider_type: nmi
    environment: live
    account_id: "579145"
  - provider_type: ccbill
    account_id: "945280/0000"
`))
	require.NoError(t, err)
	require.NotNil(t, mc.Profile)
	require.Equal(t, "Cozy Art", mc.Profile.DisplayName)
	require.Equal(t, "billing@example.com", mc.Profile.FromEmail)
	require.Len(t, mc.ProviderAccounts, 2)
	require.Equal(t, "nmi", mc.ProviderAccounts[0].ProviderType)
	require.Equal(t, "579145", mc.ProviderAccounts[0].AccountID)
	require.Equal(t, "945280/0000", mc.ProviderAccounts[1].AccountID)
}
