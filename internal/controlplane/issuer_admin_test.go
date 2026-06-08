package controlplane

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

// cpWithEnv builds a minimal ControlPlane shell carrying only the config env, so
// the SSRF allowlist (which keys off production vs not) can be unit-tested without
// a database or AuthKit service.
func cpWithEnv(env string) *ControlPlane {
	return &ControlPlane{cfg: &config.Config{Env: env}}
}

func TestValidateJWKSURI_SSRFAllowlist(t *testing.T) {
	prod := cpWithEnv("prod")
	dev := cpWithEnv("development")

	// Public https is always fine.
	require.NoError(t, prod.validateJWKSURI("https://issuer.example/.well-known/jwks.json"))
	require.NoError(t, dev.validateJWKSURI("https://issuer.example/jwks"))

	// http is rejected in production, allowed outside it (httptest/local).
	require.Error(t, prod.validateJWKSURI("http://issuer.example/jwks"))
	require.NoError(t, dev.validateJWKSURI("http://127.0.0.1:8080/jwks"))

	// Private/loopback/link-local hosts are rejected in production (SSRF).
	for _, u := range []string{
		"https://127.0.0.1/jwks",
		"https://localhost/jwks",
		"https://10.0.0.5/jwks",
		"https://192.168.1.10/jwks",
		"https://169.254.169.254/latest/meta-data", // cloud metadata endpoint
		"https://[::1]/jwks",
	} {
		require.Error(t, prod.validateJWKSURI(u), "prod must reject private/loopback host: %s", u)
	}

	// Garbage / unsupported schemes / empty are always rejected.
	for _, u := range []string{"", "   ", "ftp://x/jwks", "file:///etc/passwd", "://nope"} {
		require.Error(t, prod.validateJWKSURI(u))
		require.Error(t, dev.validateJWKSURI(u))
	}
}
