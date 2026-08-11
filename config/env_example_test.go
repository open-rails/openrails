package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joho/godotenv"
	"github.com/stretchr/testify/require"
)

// composeOnlyEnvVars are docker-compose interpolation vars documented in
// .env.example but consumed by docker-compose.yaml, never by config.Load.
// Every entry must actually appear in docker-compose.yaml — an entry that
// disappears from compose has no reason to stay in .env.example.
var composeOnlyEnvVars = map[string]bool{
	"OPENRAILS_HOST_PORT":   true,
	"POSTGRES_HOST_PORT":    true,
	"GARNET_HOST_PORT":      true,
	"AUTHKIT_KEYS_HOST_DIR": true,
	"DB_ADMIN_PASSWORD":     true,
}

// or#915: .env.example must be BOOTABLE. The env audit found it documenting
// MODE=full (hard-refused since #710) and TEST_ENV=true (never consumed) —
// following the docs could land an operator on live credentials while
// believing they were in test. This test round-trips the file two ways:
// every var must route to a real config key (or be a declared compose
// interpolation var), and the file as a whole must pass config.Load.
func TestEnvExampleRoundTrip(t *testing.T) {
	examplePath, err := filepath.Abs(filepath.Join("..", ".env.example"))
	require.NoError(t, err)
	vars, err := godotenv.Read(examplePath)
	require.NoError(t, err)
	require.NotEmpty(t, vars)

	compose, err := os.ReadFile(filepath.Join("..", "docker-compose.yaml"))
	require.NoError(t, err)
	for name := range composeOnlyEnvVars {
		require.Containsf(t, string(compose), "${"+name, "%s is declared compose-only but docker-compose.yaml never interpolates it", name)
	}

	for name := range vars {
		if composeOnlyEnvVars[name] {
			continue
		}
		key := envKeyToConfigKey(strings.ToLower(name))
		require.NotEmptyf(t, key, "%s routes to no config key — .env.example must never document a dropped var (or#915)", name)
	}

	// Full boot: the example values themselves must Load + Validate through
	// the REAL pipeline (godotenv .env pickup included). Run in a scrubbed
	// environment and an empty working directory so nothing but the example
	// file participates.
	saved := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, kv := range saved {
			k, v, _ := strings.Cut(kv, "=")
			_ = os.Setenv(k, v)
		}
	})
	os.Clearenv()
	dir := t.TempDir()
	raw, err := os.ReadFile(examplePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), raw, 0o600))
	t.Chdir(dir)

	cfg, err := Load("")
	require.NoError(t, err, ".env.example must boot config.Load as-is")
	require.True(t, cfg.IsDev())
	require.Equal(t, CredentialPostureSandbox, cfg.TestMode, "the example must pin the sandbox posture explicitly")
	require.Equal(t, ProviderWriteModeFull, cfg.GetProviderWriteMode())
	require.NotEmpty(t, cfg.DB.URL)
}
