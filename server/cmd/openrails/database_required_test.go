package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The binary refuses to start without a database instead of dialing a local
// default.
func TestRunServerRefusesWithoutDatabase(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, key := range []string{"DB_URL", "DB_HOST", "DB_PORT", "DB_DATABASE", "DB_USERNAME", "DB_PASSWORD", "OPENRAILS_CONFIG", "BILLING_CONFIG"} {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
	t.Setenv("VAULT_SECRETS_PATH", t.TempDir())
	config := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(config, []byte("test_mode: sandbox\nprovider_write_mode: readonly\n"), 0o600))
	for _, args := range [][]string{{"run-server"}, {"run-worker"}, {"migrate", "up"}} {
		_, err := execute(newRootCmd(), append(args, "--config", config)...)
		require.ErrorContains(t, err, "no database configured", args)
	}
}
