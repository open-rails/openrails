package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The one-off tools (catalog apply, nmi decline-report, reconcile) take the
// manifest run-server boots with, the server's own keys included.
func TestToolsReadTheServerManifest(t *testing.T) {
	path := writeTemp(t, "merchant.yaml", `version: 1
merchants:
  onlydemo:
    display_name: OnlyDemo
    remote_application:
      issuer: https://app.example
      jwks_uri: https://app.example/.well-known/jwks.json
    psps:
      mobius:
        rail: nmi
        environment: sandbox
        gateway_id: "000000"
        tokenization_key: public-key
        secrets:
          security_key: private-key
`)
	manifest, err := cliMerchantManifest(context.Background(), path)
	require.NoError(t, err)
	require.Equal(t, "OnlyDemo", manifest.Merchants["onlydemo"].DisplayName)
	require.Equal(t, "000000", manifest.Merchants["onlydemo"].PSPs["mobius"].AccountID)

	_, err = cliMerchantManifest(context.Background(), writeTemp(t, "typo.yaml", "version: 1\nmerchants:\n  onlydemo:\n    display_nam: OnlyDemo\n"))
	require.Error(t, err, "an unknown key is still refused")
}
