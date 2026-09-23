package embed

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestProviderCredentialSnapshotEnforcesStripePostureBeforeSeeding(t *testing.T) {
	for _, posture := range []config.CredentialPosture{config.CredentialPostureSandbox, config.CredentialPostureLive} {
		for _, key := range []string{"sk_test_fixture", "rk_test_fixture", "sk_live_fixture", "rk_live_fixture", "invalid_secret_fixture", "sk_other_fixture", "sk_test_", "rk_live_"} {
			t.Run(string(posture)+"/"+key, func(t *testing.T) {
				store, err := merchants.NewManifestSecretStoreWithIdentity("11111111-1111-4111-8111-111111111111")
				require.NoError(t, err)
				cfg := &config.Config{TestMode: posture, SecretBackend: config.SecretBackendSnapshot}
				rt := &app.Runtime{Config: cfg, ManifestSecrets: store}
				id := merchant.ID(uuid.New())
				matching := "sk_live_first"
				if posture == config.CredentialPostureSandbox {
					matching = "sk_test_first"
				}
				// A later invalid input must not leave earlier credentials seeded. Neither
				// staged entry includes a webhook: partial snapshots remain supported.
				err = loadProviderCredentialSnapshot(context.Background(), rt, []ProviderCredentialSnapshot{
					{MerchantID: id, Rail: "stripe", AccountID: "acct_first", Credentials: map[string]string{"secret_key": matching}},
					{MerchantID: id, Rail: " STRIPE ", AccountID: "acct_second", Credentials: map[string]string{"secret_key": key}},
				})
				valid := (posture == config.CredentialPostureSandbox && (key == "sk_test_fixture" || key == "rk_test_fixture")) || (posture == config.CredentialPostureLive && (key == "sk_live_fixture" || key == "rk_live_fixture"))
				name, nameErr := merchants.PSPSecretName("stripe", config.ExpectedProviderEnvironment(cfg.IsTestMode()), "acct_first", "secret_key")
				require.NoError(t, nameErr)
				first, readErr := store.Get(context.Background(), id, name)
				if valid {
					require.NoError(t, err)
					require.NoError(t, readErr)
					require.Equal(t, matching, first.Value)
				} else {
					require.Error(t, err)
					require.ErrorIs(t, readErr, merchants.ErrSecretNotFound)
					if len(key) > len("sk_test_") {
						require.NotContains(t, err.Error(), key, "credential values must not appear in errors")
					}
				}
			})
		}
	}
}
