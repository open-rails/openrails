package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joho/godotenv"
	billing "github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

// Documented in .env.example but interpolated by docker-compose.yaml, never
// read by Load.
var composeOnlyEnvVars = map[string]bool{
	"OPENRAILS_HOST_PORT": true, "POSTGRES_HOST_PORT": true, "GARNET_HOST_PORT": true,
	"AUTHKIT_KEYS_HOST_DIR": true, "DB_ADMIN_PASSWORD": true,
}

// or#915: .env.example must be bootable as-is. Every var routes to a real
// config key or is a compose interpolation var that compose actually uses, and
// the file passes the real Load pipeline (dotenv pickup included) in an
// otherwise empty environment.
func TestEnvExampleRoundTrip(t *testing.T) {
	examplePath, err := filepath.Abs(filepath.Join("..", "..", ".env.example"))
	require.NoError(t, err)
	raw, err := os.ReadFile(examplePath)
	require.NoError(t, err)
	vars, err := godotenv.Unmarshal(string(raw))
	require.NoError(t, err)
	require.Equal(t, "5434", vars["DB_PORT"])
	require.Contains(t, vars["DB_URL"], ":5434/openrails_db")

	// One compose host-port dial moves every standalone connection setting.
	moved, err := godotenv.Unmarshal(strings.Replace(string(raw), "POSTGRES_HOST_PORT=5434", "POSTGRES_HOST_PORT=5544", 1))
	require.NoError(t, err)
	require.Equal(t, "5544", moved["DB_PORT"])
	require.Contains(t, moved["DB_URL"], ":5544/openrails_db")

	compose, err := os.ReadFile(filepath.Join("..", "..", "docker-compose.yaml"))
	require.NoError(t, err)
	for name := range composeOnlyEnvVars {
		require.Containsf(t, string(compose), "${"+name, "%s is declared compose-only but compose never interpolates it", name)
	}
	for name := range vars {
		if !composeOnlyEnvVars[name] {
			require.NotEmptyf(t, envKeyToConfigKey(name), "%s routes to no config key; .env.example must not document a dropped var", name)
		}
	}

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
	require.NoError(t, os.Setenv("VAULT_SECRETS_PATH", t.TempDir()))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), raw, 0o600))
	t.Chdir(dir)

	cfg, err := Load("")
	require.NoError(t, err, ".env.example must boot Load as-is")
	require.Equal(t, billing.CredentialPostureSandbox, cfg.TestMode, "the example pins the sandbox posture explicitly")
	require.Equal(t, billing.ProviderWriteModeFull, cfg.GetProviderWriteMode())
	require.Contains(t, cfg.DB.URL, ":5434/openrails_db")
}
