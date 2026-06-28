package embed_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/embed"
)

func TestParseMerchantConfig(t *testing.T) {
	m, err := embed.ParseMerchantConfig([]byte(`
slug: cozy-art
display_name: Cozy Art
profile:
  from_email: billing@example.com
provider_accounts:
  - provider_type: nmi
    environment: live
    account_id: "579145"
  - provider_type: ccbill
    account_id: "945280/0000"
`))
	require.NoError(t, err)
	require.Equal(t, "cozy-art", m.Slug)
	require.Equal(t, "Cozy Art", m.DisplayName)
	require.Equal(t, "billing@example.com", m.Profile.FromEmail)
	require.Len(t, m.ProviderAccounts, 2)
	require.Equal(t, "nmi", m.ProviderAccounts[0].ProviderType)
	require.Equal(t, "579145", m.ProviderAccounts[0].AccountID)
	require.Equal(t, "945280/0000", m.ProviderAccounts[1].AccountID)
}
