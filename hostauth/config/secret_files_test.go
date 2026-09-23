package config

import (
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

// Load precedence: mounted secret files sit below env — a file supplies the
// value, an explicit env var overrides it.
func TestLoadSecretFilesBelowEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VAULT_SECRETS_PATH", dir)
	t.Chdir(t.TempDir()) // no stray config.yaml/.env pickup
	require.NoError(t, os.WriteFile(filepath.Join(dir, "DB_PASSWORD"), []byte("from-file"), 0o600))

	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "from-file", cfg.DB.Password)

	t.Setenv("DB_PASSWORD", "from-env")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, "from-env", cfg.DB.Password)
}
