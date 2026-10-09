package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const overlayBase = `
version: 1
merchants:
  host-one:
    display_name: Host One
    psps:
      mobius:
        rail: nmi
        account_id: "999999"
        settings: {tokenization_url: "https://secure.networkmerchants.com/token/Collect.js", tokenization_key: public-key}
        secrets: {security_key: replace-with-live-nmi-security-key}
      ccbill:
        rail: ccbill
        account_id: "999999-0000"
`

func nmiSecret(value string) string {
	return "merchants: {host-one: {psps: {mobius: {secrets: {security_key: " + value + "}}}}}"
}

// Overlays carry only secrets for declared PSPs, merge later-wins, and never
// read the environment.
func TestManifestSecretOverlays(t *testing.T) {
	t.Setenv("BILLING_MERCHANTS_HOST_ONE_PSPS_MOBIUS_SECRETS_SECURITY_KEY", "env-must-not-win")
	m, err := LoadMerchantConfigManifestWithOverlays([]byte(overlayBase), []byte(`
merchants:
  host-one:
    psps:
      mobius: {secrets: {security_key: first, webhook_signing_secret: live-webhook}}
      ccbill: {secrets: {datalink_username: dl-user, datalink_password: dl-pass, salt: dl-salt}}
`), []byte("  \n"), []byte(nmiSecret("second")))
	require.NoError(t, err)
	nmi := m.Merchants["host-one"].PSPs["mobius"]
	require.Equal(t, "second", nmi.Secrets["security_key"])
	require.Equal(t, "live-webhook", nmi.Secrets["webhook_signing_secret"], "a later overlay only replaces the keys it names")
	require.Equal(t, "999999", nmi.AccountID)
	require.Equal(t, "public-key", nmi.Settings["tokenization_key"])
	require.Equal(t, "dl-salt", m.Merchants["host-one"].PSPs["ccbill"].Secrets["salt"])

	for name, overlay := range map[string]string{
		"undeclared psp":     "merchants: {host-one: {psps: {paykings: {secrets: {security_key: k}}}}}",
		"misspelled secrets": "merchants: {host-one: {psps: {mobius: {secrtes: {security_key: k}}}}}",
		"top-level catalogs": "catalogs: {x: 1}",
		"account identity":   `merchants: {host-one: {psps: {mobius: {account_id: "attacker"}}}}`,
		"rail":               "merchants: {host-one: {psps: {mobius: {rail: stripe}}}}",
		"processor settings": "merchants: {host-one: {psps: {mobius: {settings: {tokenization_key: attacker}}}}}",
		"merchant profile":   "merchants: {host-one: {profile: {display_name: Attacker}}}",
		"billing policy":     "merchants: {host-one: {billing_policies: {free: {kind: outstanding_cap}}}}",
		"secrets not a map":  "merchants: {host-one: {psps: {mobius: {secrets: plain}}}}",
	} {
		_, err := LoadMerchantConfigManifestWithOverlays([]byte(overlayBase), []byte(overlay))
		require.Error(t, err, name)
	}
	_, err = LoadMerchantConfigManifestWithOverlays([]byte("users: []\n" + overlayBase))
	require.ErrorContains(t, err, "use the matching push command", "authority never rides the merchant manifest")
}

func TestManifestOverlayFiles(t *testing.T) {
	dir := t.TempDir()
	base, overlay := filepath.Join(dir, "base.yaml"), filepath.Join(dir, "secrets.yaml")
	require.NoError(t, os.WriteFile(base, []byte(overlayBase), 0o600))
	require.NoError(t, os.WriteFile(overlay, []byte(nmiSecret("from-file")), 0o600))

	overlays, err := ReadMerchantManifestOverlays([]string{overlay, " "})
	require.NoError(t, err)
	require.Len(t, overlays, 1)
	m, err := LoadMerchantConfigManifestFiles(base, overlay)
	require.NoError(t, err)
	require.Equal(t, "from-file", m.Merchants["host-one"].PSPs["mobius"].Secrets["security_key"])

	_, err = ReadMerchantManifestOverlays([]string{filepath.Join(dir, "missing.yaml")})
	require.Error(t, err, "a listed overlay must exist")
	_, err = LoadMerchantConfigManifestFiles(base, filepath.Join(dir, "missing.yaml"))
	require.Error(t, err)
}
