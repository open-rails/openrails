package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSandboxDoesNotRelaxSecurity(t *testing.T) {
	cfg := validationConfig("")
	cfg.TestMode = CredentialPostureSandbox
	cfg.Auth.Issuer = "http://127.0.0.1:3053"
	require.ErrorContains(t, Validate(cfg), "must use HTTPS")
	cfg.Auth.AllowLoopbackHTTP = true
	require.NoError(t, Validate(cfg))
	cfg.Auth.Issuer = "http://10.0.0.2"
	require.ErrorContains(t, Validate(cfg), "must use HTTPS")
	cfg.Auth.Issuer = "https://issuer.example"
	cfg.Auth.RequestOrigin = "https://billing.example/billing"
	require.ErrorContains(t, Validate(cfg), "without a path")
	cfg.Auth.RequestOrigin = "https://billing.example"
	require.NoError(t, Validate(cfg))
	require.True(t, cfg.RequiresSecretEncryption())
	require.False(t, cfg.Auth.AllowMemory)
	require.False(t, cfg.Auth.AllowPrivateNetworkJWKS)
	require.False(t, cfg.Auth.AllowMissingSenders)
	require.False(t, cfg.Auth.AllowEphemeralSigningKey)
}

func TestRetiredSecurityAndCollectionInputs(t *testing.T) {
	for _, key := range []string{"ENV", "API_URL", "NEW_SUBSCRIPTION_COLLECTION_POLICY"} {
		t.Run(key, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv(key, "unused")
			_, err := Load("")
			require.ErrorContains(t, err, "retired")
		})
	}
	for _, key := range []string{"env", "api_url", "new_subscription_collection_policy"} {
		t.Run(key, func(t *testing.T) {
			t.Chdir(t.TempDir())
			_, err := Load(configInputFile(t, key+": unused\n"))
			require.ErrorContains(t, err, "retired")
		})
	}
}
