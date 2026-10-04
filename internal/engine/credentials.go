package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchants"
)

func loadProviderCredentialSnapshot(ctx context.Context, rt *app.Runtime, values []config.ProviderCredentialSnapshot) error {
	if len(values) == 0 {
		return nil
	}
	if rt.Config.SecretStoreBackend() != config.SecretBackendSnapshot || rt.ManifestSecrets == nil {
		return fmt.Errorf("openrails: Deps.ProviderCredentials requires snapshot credential custody")
	}
	type entry struct {
		merchant billing.MerchantID
		name     string
		value    string
	}
	var entries []entry
	seen := map[string]bool{}
	for _, provider := range values {
		if provider.MerchantID.IsZero() || len(provider.Credentials) == 0 {
			return fmt.Errorf("provider snapshot requires merchant and credentials")
		}
		for key, value := range provider.Credentials {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("provider snapshot contains an empty credential")
			}
			name, err := merchants.PSPSecretName(provider.Rail, config.ExpectedProviderEnvironment(rt.Config.IsTestMode()), provider.AccountID, key)
			if err != nil {
				return err
			}
			rail, _, _, credentialKey, _, err := merchants.ParsePSPSecretName(name)
			if err != nil {
				return err
			}
			if rail == "stripe" && credentialKey == "secret_key" {
				if err := config.ValidateStripeCredentialPosture(rt.Config, value); err != nil {
					return fmt.Errorf("provider snapshot stripe credential: %w", err)
				}
			}
			identity := provider.MerchantID.String() + "/" + name
			if seen[identity] {
				return fmt.Errorf("provider snapshot contains a duplicate credential")
			}
			seen[identity] = true
			entries = append(entries, entry{provider.MerchantID, name, value})
		}
	}
	for _, e := range entries {
		if _, err := rt.ManifestSecrets.Seeder().Put(ctx, e.merchant, e.name, e.value); err != nil {
			return err
		}
	}
	return nil
}
