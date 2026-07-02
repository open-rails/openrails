package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The CLI file path must be as strict as the embedded bytes path (#653): koanf's
// Unmarshal silently drops unknown fields, so a typo'd manifest key would apply
// wrong config with no error. LoadMerchantConfigManifestFiles now routes the
// merged tree through the DisallowUnknownField parser.
func TestLoadMerchantConfigManifestFilesRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()

	valid := "version: 1\nmerchants:\n  acme:\n    display_name: Acme\n"
	validPath := filepath.Join(dir, "valid.yaml")
	require.NoError(t, os.WriteFile(validPath, []byte(valid), 0o600))
	m, err := LoadMerchantConfigManifest(validPath)
	require.NoError(t, err)
	require.Equal(t, "Acme", m.Merchants["acme"].DisplayName)

	// dispaly_name is a typo of display_name; it must be rejected, not ignored.
	typo := "version: 1\nmerchants:\n  acme:\n    display_name: Acme\n    dispaly_name: Whoops\n"
	typoPath := filepath.Join(dir, "typo.yaml")
	require.NoError(t, os.WriteFile(typoPath, []byte(typo), 0o600))
	_, err = LoadMerchantConfigManifest(typoPath)
	require.Error(t, err, "unknown/typo'd field must be rejected on the file path")
}

// #697: the manifest's ccbill rail block rejects a slash-form account_id at
// load time — the composite identity is dash-joined (clientAccnum-clientSubacc).
func TestParseMerchantConfigManifestRejectsCCBillSlashAccountID(t *testing.T) {
	slashed := "version: 1\nmerchants:\n  acme:\n    display_name: Acme\n    accounts:\n      ccbill:\n        ccbill:\n          account_id: \"945280/0000\"\n"
	_, err := ParseMerchantConfigManifest([]byte(slashed))
	require.Error(t, err)
	require.Contains(t, err.Error(), "CCBill account_id uses a dash: clientAccnum-clientSubacc, e.g. 945280-0000")

	dashed := "version: 1\nmerchants:\n  acme:\n    display_name: Acme\n    accounts:\n      ccbill:\n        ccbill:\n          account_id: \"945280-0000\"\n"
	m, err := ParseMerchantConfigManifest([]byte(dashed))
	require.NoError(t, err)
	require.Equal(t, "945280-0000", m.Merchants["acme"].RailMerchantAccounts["ccbill"]["ccbill"].AccountID)
}
