package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMerchantBillingEnvKey(t *testing.T) {
	tests := map[string]string{
		"BILLING_VERSION":                                                             "version",
		"BILLING_MERCHANTS_DOUJINS_DISPLAY_NAME":                                      "merchants.doujins.display_name",
		"BILLING_MERCHANTS_DOUJINS_API_HOST":                                          "merchants.doujins.api_host",
		"BILLING_MERCHANTS_DOUJINS_PROFILE_FROM_EMAIL":                                "merchants.doujins.profile.from_email",
		"BILLING_MERCHANTS_DOUJINS_PSPS_MOBIUS_NMI_SECRETS_SECURITY_KEY":              "merchants.doujins.psps.mobius.nmi.secrets.security_key",
		"BILLING_MERCHANTS_DOUJINS_PSPS_MOBIUS_SANDBOX_NMI_SETTINGS_TOKENIZATION_URL": "merchants.doujins.psps.mobius-sandbox.nmi.settings.tokenization_url",
		"BILLING_MERCHANTS_DOUJINS_PSPS_MOBIUS_SANDBOX_NMI_SETTINGS_TOKENIZATION_KEY": "merchants.doujins.psps.mobius-sandbox.nmi.settings.tokenization_key",
		"BILLING_MERCHANTS_DOUJINS_PSPS_CCBILL_CCBILL_SECRETS_DATALINK_USERNAME":      "merchants.doujins.psps.ccbill.ccbill.secrets.datalink_username",
		"BILLING_MERCHANTS_DOUJINS_PSPS_SOLANA_SOLANA_SIGNER_MODE":                    "merchants.doujins.psps.solana.solana.signer.mode",
		// #710: real manifest field, previously listed as a section but unrouted.
		"BILLING_MERCHANTS_DOUJINS_DELEGATED_INVOKER_WASTED_SPEND_WINDOWS": "merchants.doujins.delegated_invoker_wasted_spend_windows",
	}
	for envName, want := range tests {
		t.Run(envName, func(t *testing.T) {
			require.Equal(t, want, MerchantBillingEnvKey(envName))
		})
	}
	require.Empty(t, MerchantBillingEnvKey("DB_URL"))
	// #710: the ISSUER section routed to a manifest field that does not exist;
	// it is gone (host-app trust lives under remote_application, not env).
	require.Empty(t, MerchantBillingEnvKey("BILLING_MERCHANTS_DOUJINS_ISSUER_URL"))
}

// #698: env vars still using a retired accounts anchor must fail loudly. The
// new single-token PSPS anchor would otherwise mis-split the old name into
// a wrong merchant key and silently overlay config nobody declared.
func TestLoadMerchantConfigManifestBytesRejectsRenamedEnvAnchor(t *testing.T) {
	manifest := []byte(`
version: 1
merchants:
  doujins:
    display_name: Doujins
`)
	t.Run("rail_merchant_accounts", func(t *testing.T) {
		t.Setenv("BILLING_MERCHANTS_DOUJINS_RAIL_MERCHANT_ACCOUNTS_MOBIUS_NMI_SECRETS_SECURITY_KEY", "old-form")
		_, err := LoadMerchantConfigManifestBytes(manifest)
		require.Error(t, err)
		require.Contains(t, err.Error(), "RAIL_MERCHANT_ACCOUNTS was renamed to PSPS")
		require.Contains(t, err.Error(), "BILLING_MERCHANTS_DOUJINS_RAIL_MERCHANT_ACCOUNTS_MOBIUS_NMI_SECRETS_SECURITY_KEY")
	})
	t.Run("provider_accounts", func(t *testing.T) {
		t.Setenv("BILLING_MERCHANTS_DOUJINS_PROVIDER_ACCOUNTS_MOBIUS_NMI_SECRETS_SECURITY_KEY", "pre-683-form")
		_, err := LoadMerchantConfigManifestBytes(manifest)
		require.Error(t, err)
		require.Contains(t, err.Error(), "PROVIDER_ACCOUNTS was renamed to PSPS")
	})
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
    psps:
      mobius:
        nmi:
          account_id: "579145"
          secrets:
            security_key: public-placeholder
`), 0o600))
	require.NoError(t, os.WriteFile(overlay, []byte(`
merchants:
  cozy-art:
    psps:
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
	account := manifest.Merchants["cozy-art"].PSPs["mobius"]["nmi"]
	require.Equal(t, "private-overlay-value", account.Secrets["security_key"])
	require.Equal(t, "https://secure.networkmerchants.com/token/Collect.js", account.Settings["tokenization_url"])
	require.Equal(t, "public-tokenization-key", account.Settings["tokenization_key"])
}

func TestLoadMerchantConfigManifestBytesMergesEnvOverlay(t *testing.T) {
	t.Setenv("BILLING_MERCHANTS_COHOST_PSPS_MOBIUS_SANDBOX_NMI_SECRETS_SECURITY_KEY", "private-env-value")

	manifest, err := LoadMerchantConfigManifestBytes([]byte(`
version: 1
merchants:
  cohost:
    display_name: Cohost
    psps:
      mobius-sandbox:
        nmi:
          account_id: "681902"
          secrets:
            security_key: public-placeholder
`))
	require.NoError(t, err)
	account := manifest.Merchants["cohost"].PSPs["mobius-sandbox"]["nmi"]
	require.Equal(t, "private-env-value", account.Secrets["security_key"])
}

// #710: delegated_invoker_wasted_spend_windows is overlayable from env as one
// JSON list value (it is a list field — no per-index env shape).
func TestLoadMerchantConfigManifestBytesMergesWastedWindowsEnvOverlay(t *testing.T) {
	t.Setenv("BILLING_MERCHANTS_COHOST_DELEGATED_INVOKER_WASTED_SPEND_WINDOWS",
		`[{"key":"burst","window":"15m","limit":5000000},{"key":"sustained","window":"5h","limit":20000000,"currency":"USD"}]`)

	manifest, err := LoadMerchantConfigManifestBytes([]byte(`
version: 1
merchants:
  cohost:
    display_name: Cohost
`))
	require.NoError(t, err)
	windows := manifest.Merchants["cohost"].DelegatedInvokerWastedSpendWindows
	require.Len(t, windows, 2)
	require.Equal(t, BudgetWindowConfig{Key: "burst", Window: "15m", Limit: 5_000_000}, windows[0])
	require.Equal(t, BudgetWindowConfig{Key: "sustained", Window: "5h", Limit: 20_000_000, Currency: "USD"}, windows[1])
}

// #710: a BILLING_MERCHANTS_* var that routes to no manifest field fails
// loudly — the namespace is ours, so a typo or a retired section (ISSUER) must
// never be silently dropped.
func TestLoadMerchantConfigManifestBytesRejectsUnroutableEnvVars(t *testing.T) {
	manifest := []byte(`
version: 1
merchants:
  doujins:
    display_name: Doujins
`)
	t.Run("retired ISSUER section", func(t *testing.T) {
		t.Setenv("BILLING_MERCHANTS_DOUJINS_ISSUER_URL", "https://doujins.example")
		_, err := LoadMerchantConfigManifestBytes(manifest)
		require.Error(t, err)
		require.Contains(t, err.Error(), "BILLING_MERCHANTS_DOUJINS_ISSUER_URL")
		require.Contains(t, err.Error(), "does not route")
	})
	t.Run("typo'd section", func(t *testing.T) {
		t.Setenv("BILLING_MERCHANTS_DOUJINS_ACOUNTS_MOBIUS_NMI_SECRETS_SECURITY_KEY", "x")
		_, err := LoadMerchantConfigManifestBytes(manifest)
		require.Error(t, err)
		require.Contains(t, err.Error(), "does not route")
	})
}

// #710: the env overlay unmarshal is strict, matching the file path's
// DisallowUnknownField — a var that routes to a section but names an unknown
// field errors instead of silently dropping.
func TestLoadMerchantConfigManifestBytesEnvOverlayIsStrict(t *testing.T) {
	t.Setenv("BILLING_MERCHANTS_DOUJINS_PROFILE_LOGO_URI", "https://doujins.example/logo.png") // logo_url, not logo_uri

	_, err := LoadMerchantConfigManifestBytes([]byte(`
version: 1
merchants:
  doujins:
    display_name: Doujins
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unmarshal merchant config env overlay")
}

// Operator-mounted secret files (filename = env-var name) are a first-class
// overlay source: the default non-SaaS pattern where Vault renders files into
// a mounted dir. Env still wins over files; retired anchors in file names fail
// loudly the same as env.
func TestLoadMerchantConfigManifestBytesMergesSecretFileOverlay(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VAULT_SECRETS_PATH", dir)
	writeSecret := func(name, value string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600))
	}
	writeSecret("BILLING_MERCHANTS_COHOST_PSPS_MOBIUS_SANDBOX_NMI_SECRETS_SECURITY_KEY", "file-value")
	writeSecret("jwt-secret", "host-app-secret-ignored") // host app's dash-cased secret sharing the dir

	manifest := []byte(`
version: 1
merchants:
  cohost:
    display_name: Cohost
    psps:
      mobius-sandbox:
        nmi:
          account_id: "681902"
          secrets:
            security_key: public-placeholder
`)

	out, err := LoadMerchantConfigManifestBytes(manifest)
	require.NoError(t, err)
	account := out.Merchants["cohost"].PSPs["mobius-sandbox"]["nmi"]
	require.Equal(t, "file-value", account.Secrets["security_key"])

	// Env overrides the file.
	t.Setenv("BILLING_MERCHANTS_COHOST_PSPS_MOBIUS_SANDBOX_NMI_SECRETS_SECURITY_KEY", "env-wins")
	out, err = LoadMerchantConfigManifestBytes(manifest)
	require.NoError(t, err)
	account = out.Merchants["cohost"].PSPs["mobius-sandbox"]["nmi"]
	require.Equal(t, "env-wins", account.Secrets["security_key"])
}

func TestLoadMerchantConfigManifestBytesRejectsRenamedSecretFileAnchor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VAULT_SECRETS_PATH", dir)
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "BILLING_MERCHANTS_COHOST_RAIL_MERCHANT_ACCOUNTS_MOBIUS_NMI_SECRETS_SECURITY_KEY"),
		[]byte("x"), 0o600))

	_, err := LoadMerchantConfigManifestBytes([]byte("version: 1\nmerchants:\n  cohost:\n    display_name: Cohost\n"))
	require.ErrorContains(t, err, "renamed to PSPS")
	require.ErrorContains(t, err, "secret file")
}
