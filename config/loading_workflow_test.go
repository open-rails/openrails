package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func configInputFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	return path
}

// The loading boundary owns environment interpretation. Validate's direct-struct
// safety cases remain separate from this file/env/flag precedence workflow.
func TestConfigurationLoadingWorkflow(t *testing.T) {
	t.Run("defaults and canonical inputs", func(t *testing.T) {
		t.Chdir(t.TempDir())
		cfg, err := Load("")
		require.NoError(t, err)
		require.True(t, cfg.RequiresSecretEncryption())
		require.Equal(t, CredentialPostureSandbox, cfg.TestMode)
		require.True(t, cfg.IsTestMode())
		require.Equal(t, "billing", cfg.DB.SchemaName())
		require.NotNil(t, cfg.Auth)
		require.Empty(t, cfg.Auth.Issuer)
		for key, value := range map[string]string{
			"DB_HOST": "  example.com  ", "DB_USERNAME": "  user  ", "DB_PASSWORD": "  pass  ", "DB_SQL_TRACE": "true", "DB_SCHEMA": "  Custom_Billing  ",
			"VAULT_ENABLED": "true", "VAULT_ADDR": "http://127.0.0.1:8200", "VAULT_TOKEN": "root", "VAULT_AUTH_METHOD": "token",
			"SECRET_BACKEND": "db", "ENCRYPTION_MASTER_KEY": "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", "SENDGRID_API_KEY": "SG.test-key", "TEST_MODE": "sandbox", "PROVIDER_WRITE_MODE": "limited",
			"CATALOG_RECONCILIATION_INTERVAL": "30m", "PROVIDER_BILLING_QUIESCENCE_INTERVAL": "36h",
			"AUTHKIT_ACTIVE_KEY_ID": "kid-1", "AUTHKIT_ACTIVE_PRIVATE_KEY_PEM": "-----BEGIN PRIVATE KEY-----", "AUTHKIT_PUBLIC_KEYS": `{"kid-0":"-----BEGIN PUBLIC KEY-----"}`,
		} {
			t.Setenv(key, value)
		}
		cfg, err = Load("")
		require.NoError(t, err)
		require.Equal(t, "example.com", cfg.DB.Host)
		require.Equal(t, "user", cfg.DB.Username)
		require.Equal(t, "pass", cfg.DB.Password)
		require.True(t, cfg.DB.SQLTrace)
		require.Equal(t, "custom_billing", cfg.DB.Schema)
		require.Equal(t, "custom_billing", cfg.DB.SchemaName())
		require.Equal(t, SecretBackendDB, cfg.SecretBackend)
		require.Equal(t, SecretBackendDB, cfg.SecretStoreBackend())
		require.Equal(t, "SG.test-key", cfg.SendGrid.APIKey)
		require.True(t, cfg.Vault.Enabled)
		require.Equal(t, "http://127.0.0.1:8200", cfg.Vault.Address)
		require.Equal(t, "root", cfg.Vault.Token)
		require.Equal(t, "token", cfg.Vault.AuthMethod)
		require.Equal(t, ProviderWriteModeLimited, cfg.GetProviderWriteMode())
		require.True(t, cfg.IsLimitedMode())
		interval, enabled, err := cfg.CatalogReconciliationSchedule()
		require.NoError(t, err)
		require.True(t, enabled)
		require.Equal(t, 30*time.Minute, interval)
		quiet, err := cfg.ProviderBillingQuiescence()
		require.NoError(t, err)
		require.Equal(t, 36*time.Hour, quiet)
		require.Equal(t, "kid-1", cfg.Auth.ActiveKeyID)
		require.Equal(t, "-----BEGIN PRIVATE KEY-----", cfg.Auth.ActivePrivateKeyPEM)
		require.Equal(t, `{"kid-0":"-----BEGIN PUBLIC KEY-----"}`, cfg.Auth.PublicKeysJSON)
		t.Setenv("DB_URL", "postgres://u:p@localhost:5432/db?sslmode=disable")
		cfg, err = Load("")
		require.NoError(t, err)
		require.Equal(t, "postgres://u:p@localhost:5432/db?sslmode=disable", cfg.DB.URL)
	})
	t.Run("explicit posture and flag precedence", func(t *testing.T) {
		t.Chdir(t.TempDir())
		t.Setenv("TEST_MODE", "live")
		cfg, err := Load("")
		require.NoError(t, err)
		require.True(t, cfg.RequiresSecretEncryption())
		require.False(t, cfg.IsTestMode())
		require.Equal(t, CredentialPostureLive, cfg.TestMode)

		t.Setenv("PROVIDER_WRITE_MODE", "readonly")
		require.NoError(t, os.Unsetenv("TEST_MODE"))
		_, err = Load("")
		require.ErrorContains(t, err, "test_mode is required")
		t.Setenv("TEST_MODE", "live")
		cfg, err = Load("")
		require.NoError(t, err)
		require.Equal(t, CredentialPostureLive, cfg.TestMode)
		t.Setenv("TEST_MODE", "sandbox")
		cfg, err = Load("", WithOverride("test_mode", "live"))
		require.NoError(t, err)
		require.Equal(t, CredentialPostureLive, cfg.TestMode)
		require.NoError(t, os.Unsetenv("TEST_MODE"))
		cfg, err = Load("", WithOverride("test_mode", "live"))
		require.NoError(t, err)
		require.Equal(t, CredentialPostureLive, cfg.TestMode, "flag alone is explicit outside development")
	})
	t.Run("issuer and partial file overrides", func(t *testing.T) {
		cfg, err := Load(configInputFile(t, "public_billing_base_url: http://openrails:3053/\nrate_limits:\n  checkout:\n    requests_per_minute: 99\n"))
		require.NoError(t, err)
		require.Empty(t, cfg.Auth.Issuer)
		require.Equal(t, 99, (*cfg.RateLimits)["checkout"].RequestsPerMinute)
		require.Equal(t, 20, (*cfg.RateLimits)["subscribe"].RequestsPerMinute)
		require.NotNil(t, (*cfg.RateLimits)["default"])
		t.Setenv("AUTH_ISSUER", "https://billing.example.com/")
		cfg, err = Load("")
		require.NoError(t, err)
		require.Equal(t, "https://billing.example.com/", cfg.Auth.Issuer)
	})
}

func TestConfigurationInputRefusals(t *testing.T) {
	for _, row := range []struct {
		key, value string
		message    []string
	}{
		{"ENV", "", []string{"ENV is retired"}}, {"ENV", "   ", []string{"ENV is retired"}},
		{"OPENRAILS_BILLING_HOT_PATH_FAIL_POLICY", "fail_open", []string{"billing_hot_path was removed"}},
		{"TEST_ENV", "true", []string{"TEST_ENV was never consumed and is removed (or#915)"}},
		{"DB_TYPO_FIELD", "x", []string{"unknown keys refuse boot", "typo_field"}},
		{"CLICKHOUSE_HTTP_ADDR", "http://ch.example:8123", []string{"clickhouse config was removed (#735)"}},
		{"CATALOG_RECONCILIATION_INTERVAL", "30minutes", []string{"catalog_reconciliation_interval"}},
		{"MODE", "full", []string{"mode was removed", "provider_write_mode"}},
		{"BILLING_MODE", "full", []string{"mode was removed", "provider_write_mode"}},
		{"OPENRAILS_CATALOG_RECONCILIATION_INTERVAL", "30m", []string{"CATALOG_RECONCILIATION_INTERVAL", "renamed"}},
		{"OPENRAILS_SQL_TRACE", "1", []string{"db.sql_trace"}},
		{"ACTIVE_KEY_ID", "x", []string{"ACTIVE_KEY_ID was renamed", "AUTHKIT_ACTIVE_KEY_ID"}},
		{"ACTIVE_PRIVATE_KEY_PEM", "x", []string{"ACTIVE_PRIVATE_KEY_PEM was renamed", "AUTHKIT_ACTIVE_PRIVATE_KEY_PEM"}},
		{"PUBLIC_KEYS", "x", []string{"PUBLIC_KEYS was renamed", "AUTHKIT_PUBLIC_KEYS"}},
		{"DB_SCHEMA", "bad schema", []string{"schema"}},
	} {
		t.Run(row.key+"/"+row.value, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv(row.key, row.value)
			_, err := Load("")
			for _, message := range row.message {
				require.ErrorContains(t, err, message)
			}
		})
	}
	for _, row := range []struct {
		yaml    string
		message []string
	}{
		{"not_a_real_key: 1\n", []string{"unknown keys refuse boot", "not_a_real_key"}},
		{"mode: readonly\n", []string{"mode was removed"}},
	} {
		_, err := Load(configInputFile(t, row.yaml))
		for _, message := range row.message {
			require.ErrorContains(t, err, message)
		}
	}
}
