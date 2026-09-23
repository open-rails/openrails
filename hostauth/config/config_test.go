package config

import (
	billing "github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("TEST_MODE", "sandbox")
	_ = os.Setenv("PROVIDER_WRITE_MODE", "full")
	os.Exit(m.Run())
}
func hostValidationConfig() *Config {
	cfg := &Config{Config: billing.GetDefaultBillingConfig(), Auth: &AuthConfig{}}
	cfg.TestMode = billing.CredentialPostureSandbox
	cfg.ProviderWriteMode = billing.ProviderWriteModeFull
	cfg.DB.Username, cfg.DB.Password = "billing_app", "database-password"
	cfg.DB.URL = cfg.DB.GetConnectionString()
	return cfg
}
func TestStandaloneIssuerValidation(t *testing.T) {
	for _, issuer := range []string{"http://auth.internal:8080", "https://auth.example.com"} {
		cfg := hostValidationConfig()
		cfg.Auth.Issuer = issuer
		err := Validate(cfg)
		if issuer == "http://auth.internal:8080" {
			require.ErrorContains(t, err, "must use HTTPS")
		} else {
			require.NoError(t, err)
		}
	}
}
