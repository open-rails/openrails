//go:build integration

package embedded_test

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/embedded"
)

// or#893 phase 9. In API merchant-source mode the per-merchant secret store IS
// the credential plane — there is no second one. The build failure used to warn
// ("pulls arm from boot-config rails only") and continue, but the plane it
// named was an empty config.PSPSet constructed two lines above: the pull then
// armed NOTHING and returned success-shaped output over a snapshot it never
// fetched. Silently reporting "no drift" because you read no providers is the
// worst possible answer for a reconciliation tool.
//
// Failing before this change: PullProvider returned nil.
func TestPullProviderRefusesWhenNoCredentialPlaneCanBeBuilt(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	// API mode + Vault declared but unreachable: merchantsecrets.Build fails,
	// so no rail can be armed for anyone.
	cfg := &config.Config{
		Env:            "development",
		DB:             &config.DBConfig{URL: dsn},
		MerchantSource: config.MerchantSourceAPI,
		SecretBackend:  config.SecretBackendVault,
		Vault: &config.VaultConfig{
			Enabled:    true,
			Address:    "http://127.0.0.1:1", // nothing listens here
			AuthMethod: "token",
			Token:      "not-a-real-token",
		},
	}

	err := embedded.PullProvider(context.Background(), embedded.PullProviderOptions{
		Config:       cfg,
		MerchantSlug: "does-not-matter",
		Out:          io.Discard,
	})
	require.Error(t, err, "a pull with no credential plane must fail, not report an empty-but-clean run")
	require.Contains(t, err.Error(), "no rail can be armed")
}
