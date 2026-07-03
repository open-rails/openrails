package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSecretFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VAULT_SECRETS_PATH", dir)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "DB_PASSWORD"), []byte("s3cret\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "jwt-secret"), []byte("dash-cased-skipped"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "EMPTY_VALUE"), []byte(" \n"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "..data"), 0o700))

	got, err := SecretFiles()
	require.NoError(t, err)
	require.Equal(t, map[string]string{"DB_PASSWORD": "s3cret"}, got)
}

func TestSecretFilesAbsentDir(t *testing.T) {
	t.Setenv("VAULT_SECRETS_PATH", filepath.Join(t.TempDir(), "nope"))
	got, err := SecretFiles()
	require.NoError(t, err)
	require.Nil(t, got)
}

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
