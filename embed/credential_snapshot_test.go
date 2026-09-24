package embed

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

func snapshotRuntime(t *testing.T, posture config.CredentialPosture) (*app.Runtime, *merchants.ManifestSecretStore) {
	t.Helper()
	store, err := merchants.NewManifestSecretStoreWithIdentity("11111111-1111-4111-8111-111111111111")
	require.NoError(t, err)
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, TestMode: posture, SecretBackend: config.SecretBackendSnapshot}
	return &app.Runtime{Config: cfg, ManifestSecrets: store}, store
}

// A snapshot is all-or-nothing: a later invalid entry leaves earlier entries
// unseeded, and a Stripe key must match the runtime's posture.
func TestCredentialSnapshotStripePostureIsAtomic(t *testing.T) {
	for _, posture := range []config.CredentialPosture{config.CredentialPostureSandbox, config.CredentialPostureLive} {
		prefix := map[config.CredentialPosture]string{config.CredentialPostureSandbox: "_test_", config.CredentialPostureLive: "_live_"}[posture]
		for _, key := range []string{"sk_test_fixture", "rk_test_fixture", "sk_live_fixture", "rk_live_fixture", "invalid_secret_fixture", "sk_other_fixture", "sk_test_", "rk_live_"} {
			t.Run(string(posture)+"/"+key, func(t *testing.T) {
				rt, store := snapshotRuntime(t, posture)
				id := merchant.ID(uuid.New())
				first := "sk" + prefix + "first"
				err := loadProviderCredentialSnapshot(context.Background(), rt, []ProviderCredentialSnapshot{
					{MerchantID: id, Rail: "stripe", AccountID: "acct_first", Credentials: map[string]string{"secret_key": first}},
					{MerchantID: id, Rail: " STRIPE ", AccountID: "acct_second", Credentials: map[string]string{"secret_key": key}},
				})
				name, nameErr := merchants.PSPSecretName("stripe", config.ExpectedProviderEnvironment(rt.Config.IsTestMode()), "acct_first", "secret_key")
				require.NoError(t, nameErr)
				seeded, readErr := store.Get(context.Background(), id, name)
				valid := len(key) > len("sk_test_") && (key[:2] == "sk" || key[:2] == "rk") && key[2:8] == prefix
				if valid {
					require.NoError(t, err)
					require.NoError(t, readErr)
					require.Equal(t, first, seeded.Value)
					return
				}
				require.Error(t, err)
				require.ErrorIs(t, readErr, merchants.ErrSecretNotFound)
				if len(key) > len("sk_test_") {
					require.NotContains(t, err.Error(), key, "credential values never appear in errors")
				}
			})
		}
	}
}

func TestCredentialSnapshotRefusals(t *testing.T) {
	id := merchant.ID(uuid.New())
	entry := func(mid merchant.ID, credentials map[string]string) ProviderCredentialSnapshot {
		return ProviderCredentialSnapshot{MerchantID: mid, Rail: "nmi", AccountID: "100001", Credentials: credentials}
	}
	for name, values := range map[string][]ProviderCredentialSnapshot{
		"zero merchant":  {entry(merchant.ID{}, map[string]string{"security_key": "k"})},
		"no credentials": {entry(id, nil)},
		"blank value":    {entry(id, map[string]string{"security_key": "  "})},
		"duplicate":      {entry(id, map[string]string{"security_key": "a"}), entry(id, map[string]string{"security_key": "b"})},
	} {
		t.Run(name, func(t *testing.T) {
			rt, _ := snapshotRuntime(t, config.CredentialPostureSandbox)
			require.Error(t, loadProviderCredentialSnapshot(context.Background(), rt, values))
		})
	}
	rt, _ := snapshotRuntime(t, config.CredentialPostureSandbox)
	rt.Config.SecretBackend = config.SecretBackendDB
	require.ErrorContains(t, loadProviderCredentialSnapshot(context.Background(), rt, []ProviderCredentialSnapshot{entry(id, map[string]string{"security_key": "k"})}), "snapshot credential custody")
	require.NoError(t, loadProviderCredentialSnapshot(context.Background(), rt, nil), "no snapshot needs no custody")
}
