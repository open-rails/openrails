package openrails

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

func TestReadMerchantFile(t *testing.T) {
	_, err := ReadMerchantFile(filepath.Join(t.TempDir(), "missing.yaml"))
	require.ErrorIs(t, err, os.ErrNotExist)

	typo := filepath.Join(t.TempDir(), "merchant.yaml")
	require.NoError(t, os.WriteFile(typo, []byte("psp:\n  mobius: {rail: nmi}\n"), 0o600))
	_, err = ReadMerchantFile(typo)
	require.ErrorContains(t, err, typo, "a refused file names itself")
}

// A declaration built in Go, written as a merchant file, reads back unchanged.
func TestMerchantFileRoundTrip(t *testing.T) {
	want := MerchantDeclaration{
		Slug: "onlydemo", DisplayName: "OnlyDemo", APIHost: "api.onlydemo.example",
		PSPs: map[string]PSPConfig{
			"mobius": NMIPSP{AccountID: "000000", TokenizationKey: "tk", SecurityKey: "sk", WebhookSigningSecret: "whs", CardEntry: "server"}.PSPConfig(),
			"wallet": SolanaPSP{TransitKey: "transit", RPCProvider: "public", Tokens: map[string]SolanaToken{"SOL": {}, "XYZ": {Mint: "m", Name: "Xyz"}}}.PSPConfig(),
		},
		Settings: billing.MerchantSettings{InvoiceBillingBoundary: "calendar_month"},
	}
	raw, err := yaml.Marshal(struct {
		Slug                string `yaml:"slug"`
		MerchantDeclaration `yaml:",inline"`
	}{want.Slug, want})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "merchant.yaml")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	got, err := ReadMerchantFile(path)
	require.NoError(t, err)
	require.Equal(t, want, got)
}
