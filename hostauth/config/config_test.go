package config

import (
	billing "github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestMain(m *testing.M) { _ = os.Setenv("ENV", "development"); os.Exit(m.Run()) }
func TestStandaloneIssuerValidation(t *testing.T) {
	for _, issuer := range []string{"http://auth.internal:8080", "https://auth.example.com"} {
		cfg := &Config{Config: billing.GetDefaultBillingConfig(), Auth: &AuthConfig{Issuer: issuer}}
		cfg.Env = "production"
		cfg.ProviderWriteMode = billing.ProviderWriteModeReadOnly
		cfg.DB.URL = cfg.DB.GetConnectionString()
		err := Validate(cfg)
		if issuer == "http://auth.internal:8080" {
			require.ErrorContains(t, err, "must use https outside development")
		} else {
			require.NoError(t, err)
		}
	}
}
