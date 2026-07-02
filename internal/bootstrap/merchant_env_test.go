package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMerchantBillingEnvKey(t *testing.T) {
	tests := map[string]string{
		"BILLING_VERSION":                              "version",
		"BILLING_MERCHANTS_DOUJINS_DISPLAY_NAME":       "merchants.doujins.display_name",
		"BILLING_MERCHANTS_DOUJINS_PROFILE_FROM_EMAIL": "merchants.doujins.profile.from_email",
		"BILLING_MERCHANTS_DOUJINS_RAIL_MERCHANT_ACCOUNTS_MOBIUS_NMI_SECRETS_SECURITY_KEY":              "merchants.doujins.rail_merchant_accounts.mobius.nmi.secrets.security_key",
		"BILLING_MERCHANTS_DOUJINS_RAIL_MERCHANT_ACCOUNTS_MOBIUS_SANDBOX_NMI_SETTINGS_TOKENIZATION_URL": "merchants.doujins.rail_merchant_accounts.mobius-sandbox.nmi.settings.tokenization_url",
		"BILLING_MERCHANTS_DOUJINS_RAIL_MERCHANT_ACCOUNTS_MOBIUS_SANDBOX_NMI_SETTINGS_TOKENIZATION_KEY": "merchants.doujins.rail_merchant_accounts.mobius-sandbox.nmi.settings.tokenization_key",
		"BILLING_MERCHANTS_DOUJINS_RAIL_MERCHANT_ACCOUNTS_SOLANA_SOLANA_SIGNER_MODE":                    "merchants.doujins.rail_merchant_accounts.solana.solana.signer.mode",
	}
	for envName, want := range tests {
		t.Run(envName, func(t *testing.T) {
			require.Equal(t, want, MerchantBillingEnvKey(envName))
		})
	}
	require.Empty(t, MerchantBillingEnvKey("DB_URL"))
}

func TestLoadMerchantConfigManifestFilesMergesStructuredOverlay(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "merchants.yaml")
	overlay := filepath.Join(dir, "secrets.yaml")
	require.NoError(t, os.WriteFile(base, []byte(`
version: 1
merchants:
  cozy-art:
    display_name: Cozy Art
    rail_merchant_accounts:
      mobius:
        nmi:
          environment: live
          account_id: "579145"
          secrets:
            security_key: public-placeholder
`), 0o600))
	require.NoError(t, os.WriteFile(overlay, []byte(`
merchants:
  cozy-art:
    rail_merchant_accounts:
      mobius:
        nmi:
          secrets:
            security_key: private-overlay-value
          settings:
            tokenization_url: https://secure.networkmerchants.com/token/Collect.js
            tokenization_key: public-tokenization-key
`), 0o600))

	manifest, err := LoadMerchantConfigManifestFiles(base, overlay)
	require.NoError(t, err)
	account := manifest.Merchants["cozy-art"].RailMerchantAccounts["mobius"]["nmi"]
	require.Equal(t, "private-overlay-value", account.Secrets["security_key"])
	require.Equal(t, "https://secure.networkmerchants.com/token/Collect.js", account.Settings["tokenization_url"])
	require.Equal(t, "public-tokenization-key", account.Settings["tokenization_key"])
}

func TestLoadMerchantConfigManifestBytesMergesEnvOverlay(t *testing.T) {
	t.Setenv("BILLING_MERCHANTS_COHOST_RAIL_MERCHANT_ACCOUNTS_MOBIUS_SANDBOX_NMI_SECRETS_SECURITY_KEY", "private-env-value")

	manifest, err := LoadMerchantConfigManifestBytes([]byte(`
version: 1
merchants:
  cohost:
    display_name: Cohost
    rail_merchant_accounts:
      mobius-sandbox:
        nmi:
          environment: test
          account_id: "681902"
          secrets:
            security_key: public-placeholder
`))
	require.NoError(t, err)
	account := manifest.Merchants["cohost"].RailMerchantAccounts["mobius-sandbox"]["nmi"]
	require.Equal(t, "private-env-value", account.Secrets["security_key"])
}
