package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/catalog"
)

// The example's files read as the README describes them.
func TestExampleFilesRead(t *testing.T) {
	m, err := openrails.ReadMerchantFile("merchant.example.yaml")
	require.NoError(t, err)
	require.Equal(t, "onlydemo", m.Slug)
	require.Equal(t, openrails.NMIPSP{
		AccountID:            "000000",
		TokenizationKey:      "your-public-tokenization-key",
		SecurityKey:          "your-private-security-key",
		WebhookSigningSecret: "your-webhook-signing-key",
	}.PSPConfig(), m.PSPs["mobius"], "the README's Go form is the same declaration")

	_, err = catalog.ReadFile("catalog.yaml")
	require.NoError(t, err)
}
