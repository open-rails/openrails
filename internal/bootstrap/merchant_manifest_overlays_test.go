package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const overlayBaseManifest = `
version: 1
merchants:
  doujins:
    display_name: Doujins
    psps:
      mobius:
        nmi:
          account_id: "1331929"
          settings:
            tokenization_url: https://secure.networkmerchants.com/token/Collect.js
            tokenization_key: public-key
          secrets:
            security_key: replace-with-live-nmi-security-key
      ccbill:
        ccbill:
          account_id: "945280-0000"
`

func TestLoadMerchantConfigManifestWithOverlaysReplacesPlaceholders(t *testing.T) {
	t.Setenv("BILLING_MERCHANTS_DOUJINS_PSPS_MOBIUS_NMI_SECRETS_SECURITY_KEY", "env-must-not-win")
	overlay := []byte(`
merchants:
  doujins:
    psps:
      mobius:
        nmi:
          secrets:
            security_key: live-key
            webhook_signing_secret: live-webhook
      ccbill:
        ccbill:
          secrets:
            datalink_username: dl-user
            datalink_password: dl-pass
            salt: dl-salt
`)
	m, err := LoadMerchantConfigManifestWithOverlays([]byte(overlayBaseManifest), overlay)
	require.NoError(t, err)
	nmi := m.Merchants["doujins"].PSPs["mobius"]["nmi"]
	require.Equal(t, "live-key", nmi.Secrets["security_key"])
	require.Equal(t, "live-webhook", nmi.Secrets["webhook_signing_secret"])
	require.Equal(t, "1331929", nmi.AccountID)
	require.Equal(t, "public-key", nmi.Settings["tokenization_key"])
	cc := m.Merchants["doujins"].PSPs["ccbill"]["ccbill"]
	require.Equal(t, "dl-user", cc.Secrets["datalink_username"])
	require.Equal(t, "dl-salt", cc.Secrets["salt"])
}

func TestLoadMerchantConfigManifestWithOverlaysLaterWins(t *testing.T) {
	a := []byte("merchants: {doujins: {psps: {mobius: {nmi: {secrets: {security_key: first}}}}}}")
	b := []byte("merchants: {doujins: {psps: {mobius: {nmi: {secrets: {security_key: second}}}}}}")
	m, err := LoadMerchantConfigManifestWithOverlays([]byte(overlayBaseManifest), a, []byte("  \n"), b)
	require.NoError(t, err)
	require.Equal(t, "second", m.Merchants["doujins"].PSPs["mobius"]["nmi"].Secrets["security_key"])
}

func TestLoadMerchantConfigManifestWithOverlaysRejectsUndeclaredPSPAndUnknownField(t *testing.T) {
	_, err := LoadMerchantConfigManifestWithOverlays([]byte(overlayBaseManifest),
		[]byte("merchants: {doujins: {psps: {paykings: {nmi: {secrets: {security_key: k}}}}}}"))
	require.Error(t, err, "secrets for a PSP the manifest never declared must not be silently accepted")

	_, err = LoadMerchantConfigManifestWithOverlays([]byte(overlayBaseManifest),
		[]byte("merchants: {doujins: {psps: {mobius: {nmi: {secrtes: {security_key: k}}}}}}"))
	require.Error(t, err)

	_, err = LoadMerchantConfigManifestWithOverlays([]byte(overlayBaseManifest),
		[]byte("catalogs: {x: 1}"))
	require.ErrorContains(t, err, "does not accept")
}

func TestReadMerchantManifestOverlays(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.yaml")
	require.NoError(t, os.WriteFile(a, []byte("merchants: {doujins: {psps: {mobius: {nmi: {secrets: {security_key: from-file}}}}}}"), 0o600))
	overlays, err := ReadMerchantManifestOverlays([]string{a, " "})
	require.NoError(t, err)
	require.Len(t, overlays, 1)
	m, err := LoadMerchantConfigManifestWithOverlays([]byte(overlayBaseManifest), overlays...)
	require.NoError(t, err)
	require.Equal(t, "from-file", m.Merchants["doujins"].PSPs["mobius"]["nmi"].Secrets["security_key"])

	_, err = ReadMerchantManifestOverlays([]string{filepath.Join(dir, "missing.yaml")})
	require.Error(t, err)
}
