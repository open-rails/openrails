package hostconfig

import (
	"bytes"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestCommandLoadKeepsConfigurationWithoutStartupMessages(t *testing.T) {
	bootEnv(t)
	unsetenv(t, "PUBLIC_BILLING_BASE_URL")
	writeFile(t, ".env", "PUBLIC_BILLING_BASE_URL=https://billing.example\n")
	var logs bytes.Buffer
	previousOutput, previousLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&logs)
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() { log.SetOutput(previousOutput); log.SetLevel(previousLevel) })

	normal, err := Load("")
	require.NoError(t, err)
	require.Contains(t, logs.String(), "loaded environment overrides")
	logs.Reset()
	command, err := LoadForCommand("")
	require.NoError(t, err)
	require.Equal(t, normal, command)
	require.Equal(t, "https://billing.example", command.PublicBillingBaseURL)
	require.Empty(t, logs.String())

	database, err := LoadDatabaseForCommand("")
	require.NoError(t, err)
	require.Equal(t, testDatabaseURL, database.DB.URL)
	require.Empty(t, logs.String())
	_, err = LoadForCommand("", WithOverride("provider_write_mode", "invalid"))
	require.Error(t, err, "quiet commands retain validation")
	_, err = LoadDatabaseForCommand("", WithOverride("database.schema", "invalid schema"))
	require.Error(t, err, "quiet database commands retain validation")
}

func TestDirectoryCommandNeedsOnlyDatabaseAndOptionalVault(t *testing.T) {
	bootEnv(t)
	unsetenv(t, "PROVIDER_WRITE_MODE")
	path := writeFile(t, "directory.yaml", "vault:\n  address: https://vault.example\n  kv_mount: merchants\n  token: fixture-token\n")
	cfg, err := LoadDirectoryForCommand(path)
	require.NoError(t, err)
	require.Equal(t, testDatabaseURL, cfg.DB.URL)
	require.Equal(t, "https://vault.example", cfg.Vault.Address)
	require.Equal(t, "merchants", cfg.Vault.KVMount)
	require.Empty(t, cfg.Auth.Token.Issuer)
	require.Empty(t, cfg.ProviderWriteMode)
}
