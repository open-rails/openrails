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
  doujins:
    display_name: Doujins
    psps:
      mobius:
        nmi:
          account_id: "1331929"
          settings: {tokenization_url: "https://secure.networkmerchants.com/token/Collect.js", tokenization_key: public-key}
          secrets: {security_key: replace-with-live-nmi-security-key}
      ccbill:
        ccbill:
          account_id: "945280-0000"
`

func nmiSecret(value string) string {
	return "merchants: {doujins: {psps: {mobius: {nmi: {secrets: {security_key: " + value + "}}}}}}"
}

// Overlays carry only secrets for declared PSPs, merge later-wins, and never
// read the environment.
func TestManifestSecretOverlays(t *testing.T) {
	t.Setenv("BILLING_MERCHANTS_DOUJINS_PSPS_MOBIUS_NMI_SECRETS_SECURITY_KEY", "env-must-not-win")
	m, err := LoadMerchantConfigManifestWithOverlays([]byte(overlayBase), []byte(`
merchants:
  doujins:
    psps:
      mobius: {nmi: {secrets: {security_key: first, webhook_signing_secret: live-webhook}}}
      ccbill: {ccbill: {secrets: {datalink_username: dl-user, datalink_password: dl-pass, salt: dl-salt}}}
`), []byte("  \n"), []byte(nmiSecret("second")))
	require.NoError(t, err)
	nmi := m.Merchants["doujins"].PSPs["mobius"]["nmi"]
	require.Equal(t, "second", nmi.Secrets["security_key"])
	require.Equal(t, "live-webhook", nmi.Secrets["webhook_signing_secret"], "a later overlay only replaces the keys it names")
	require.Equal(t, "1331929", nmi.AccountID)
	require.Equal(t, "public-key", nmi.Settings["tokenization_key"])
	require.Equal(t, "dl-salt", m.Merchants["doujins"].PSPs["ccbill"]["ccbill"].Secrets["salt"])

	for name, overlay := range map[string]string{
		"undeclared psp":     "merchants: {doujins: {psps: {paykings: {nmi: {secrets: {security_key: k}}}}}}",
		"misspelled secrets": "merchants: {doujins: {psps: {mobius: {nmi: {secrtes: {security_key: k}}}}}}",
		"top-level catalogs": "catalogs: {x: 1}",
		"account identity":   `merchants: {doujins: {psps: {mobius: {nmi: {account_id: "attacker"}}}}}`,
		"processor settings": "merchants: {doujins: {psps: {mobius: {nmi: {settings: {tokenization_key: attacker}}}}}}",
		"merchant profile":   "merchants: {doujins: {profile: {display_name: Attacker}}}",
		"billing policy":     "merchants: {doujins: {billing_policies: {free: {kind: outstanding_cap}}}}",
		"secrets not a map":  "merchants: {doujins: {psps: {mobius: {nmi: {secrets: plain}}}}}",
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
	require.Equal(t, "from-file", m.Merchants["doujins"].PSPs["mobius"]["nmi"].Secrets["security_key"])

	_, err = ReadMerchantManifestOverlays([]string{filepath.Join(dir, "missing.yaml")})
	require.Error(t, err, "a listed overlay must exist")
	_, err = LoadMerchantConfigManifestFiles(base, filepath.Join(dir, "missing.yaml"))
	require.Error(t, err)
}
