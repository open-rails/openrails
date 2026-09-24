package merchantbootstrap

import (
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestValidateMerchantDeclaration(t *testing.T) {
	stripeKey := func(key string) MerchantConfig {
		return MerchantConfig{PSPs: map[string]PSPConfig{"main": {"Stripe": {AccountID: "acct_1", Settings: map[string]any{"publishable_key": key}}}}}
	}
	sandbox := &config.Config{TestMode: config.CredentialPostureSandbox}
	live := &config.Config{TestMode: config.CredentialPostureLive}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		mt   MerchantConfig
		ok   bool
	}{
		{"sandbox test key", sandbox, stripeKey("pk_test_abc"), true},
		{"live live key", live, stripeKey("pk_live_abc"), true},
		{"sandbox live key", sandbox, stripeKey("pk_live_abc"), false},
		{"live test key", live, stripeKey("pk_test_abc"), false},
		{"bare prefix", sandbox, stripeKey("pk_test_"), false},
		{"secret key as publishable", sandbox, stripeKey("sk_test_abc"), false},
		{"non-string key", sandbox, MerchantConfig{PSPs: map[string]PSPConfig{"main": {"stripe": {Settings: map[string]any{"publishable_key": 1}}}}}, false},
		{"invalid api host", live, MerchantConfig{APIHost: "bad_host.example"}, false},
		{"managed backend refuses startup PSPs", &config.Config{TestMode: config.CredentialPostureSandbox, SecretBackend: config.SecretBackendDB}, stripeKey("pk_test_abc"), false},
		{"managed backend without PSPs", &config.Config{SecretBackend: config.SecretBackendDB}, MerchantConfig{}, true},
	} {
		err := ValidateMerchantDeclaration(tc.cfg, tc.mt)
		if tc.ok {
			require.NoError(t, err, tc.name)
		} else {
			require.Error(t, err, tc.name)
		}
	}
}
