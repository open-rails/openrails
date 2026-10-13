package merchantbootstrap

import (
	"testing"

	"github.com/open-rails/openrails/internal/config"
	"github.com/stretchr/testify/require"
)

func TestValidateMerchantDeclaration(t *testing.T) {
	stripe := func(environment, key string) config.MerchantDeclaration {
		return config.MerchantDeclaration{PSPs: map[string]config.PSPConfig{"main": {Rail: "Stripe", Environment: environment, AccountID: "acct_1", Settings: map[string]any{"publishable_key": key}}}}
	}
	cfg := &config.Config{}
	ccbill := func(secrets map[string]string) config.MerchantDeclaration {
		return config.MerchantDeclaration{PSPs: map[string]config.PSPConfig{"main": {Rail: "ccbill", Environment: "sandbox", AccountID: "999999-0000", Secrets: secrets}}}
	}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		mt   config.MerchantDeclaration
		ok   bool
	}{
		{"test test key", cfg, stripe("test", "pk_test_abc"), true},
		{"live live key", cfg, stripe("live", "pk_live_abc"), true},
		{"test live key", cfg, stripe("test", "pk_live_abc"), false},
		{"live test key", cfg, stripe("live", "pk_test_abc"), false},
		{"bare prefix", cfg, stripe("test", "pk_test_"), false},
		{"secret key as publishable", cfg, stripe("test", "sk_test_abc"), false},
		{"non-string key", cfg, config.MerchantDeclaration{PSPs: map[string]config.PSPConfig{"main": {Rail: "stripe", Environment: "test", Settings: map[string]any{"publishable_key": 1}}}}, false},
		{"ccbill with salt", cfg, ccbill(map[string]string{"salt": "s", "datalink_username": "u", "datalink_password": "p"}), true},
		{"ccbill credentials without salt", cfg, ccbill(map[string]string{"datalink_username": "u", "datalink_password": "p"}), false},
		{"ccbill blank salt", cfg, ccbill(map[string]string{"salt": " "}), false},
		{"ccbill identity only", cfg, ccbill(nil), true},
		{"vault-held configuration seeds from the declaration", &config.Config{Vault: &config.VaultConfig{KVMount: "kv"}}, stripe("test", "pk_test_abc"), true},
	} {
		err := ValidateMerchantDeclaration(tc.cfg, tc.mt)
		if tc.ok {
			require.NoError(t, err, tc.name)
		} else {
			require.Error(t, err, tc.name)
		}
	}
}
