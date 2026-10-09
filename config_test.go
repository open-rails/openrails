package openrails

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

func TestReadMerchantFile(t *testing.T) {
	m, err := ReadMerchantFile(filepath.Join("examples", "embedded", "merchant.example.yaml"))
	require.NoError(t, err)
	require.Empty(t, m.Slug, "the caller names the merchant")
	mobius := m.PSPs["mobius"]
	require.Equal(t, billing.RailNMI, mobius.Rail)
	require.Equal(t, "000000", mobius.AccountID)
	require.Equal(t, "your-public-tokenization-key", mobius.Settings["tokenization_key"])
	require.Equal(t, map[string]string{"security_key": "your-private-security-key", "webhook_signing_secret": "your-webhook-signing-key"}, mobius.Secrets)

	_, err = ReadMerchantFile(filepath.Join(t.TempDir(), "missing.yaml"))
	require.ErrorIs(t, err, os.ErrNotExist)

	typo := filepath.Join(t.TempDir(), "merchant.yaml")
	require.NoError(t, os.WriteFile(typo, []byte("psp:\n  mobius: {rail: nmi}\n"), 0o600))
	_, err = ReadMerchantFile(typo)
	require.ErrorContains(t, err, typo, "a refused file names itself")
}
