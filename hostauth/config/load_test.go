package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-rails/authkit"
	billing "github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

// bootEnv isolates Load from the working directory (.env, config.yaml), the
// host's mounted secrets and ambient database env, and declares the two
// required dials.
func bootEnv(t *testing.T) (secretsDir string) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, key := range []string{"DB_URL", "DB_PASSWORD", "OPENRAILS_CONFIG", "BILLING_CONFIG"} {
		unsetenv(t, key)
	}
	secretsDir = t.TempDir()
	t.Setenv("VAULT_SECRETS_PATH", secretsDir)
	t.Setenv("TEST_MODE", "sandbox")
	t.Setenv("PROVIDER_WRITE_MODE", "full")
	return secretsDir
}

func writeFile(t *testing.T, path, text string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	return path
}

func unsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	require.NoError(t, os.Unsetenv(key))
}

func TestLoadDefaultsAndEnvironmentMapping(t *testing.T) {
	bootEnv(t)
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, billing.CredentialPostureSandbox, cfg.TestMode)
	require.Equal(t, billing.DefaultSchema, cfg.DB.Schema)
	require.NotEmpty(t, cfg.DB.URL, "the DSN is assembled from the atomic parts")
	require.NotNil(t, cfg.Auth)
	require.Empty(t, cfg.Auth.Issuer, "no URL setting supplies an issuer fallback")
	require.False(t, cfg.AllowCatalogUpdates)

	for key, value := range map[string]string{
		"DB_HOST": "  example.com  ", "DB_USERNAME": "  user  ", "DB_PASSWORD": "  pass  ", "DB_SQL_TRACE": "true", "DB_SCHEMA": "  Custom_Billing  ",
		"VAULT_ENABLED": "true", "VAULT_ADDR": "http://127.0.0.1:8200", "VAULT_TOKEN": "root",
		"SECRET_BACKEND": "db", "ENCRYPTION_MASTER_KEY": "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", "SENDGRID_API_KEY": "SG.test-key",
		"PROVIDER_WRITE_MODE": "limited", "CATALOG_RECONCILIATION_INTERVAL": "30m", "PROVIDER_BILLING_QUIESCENCE_INTERVAL": "36h",
		"ALLOW_CATALOG_UPDATES": "true", "MERCHANT_CONFIG_HTTP": "true", "TRUSTED_PROXIES": `["10.0.0.0/8"]`,
		"AUTHKIT_ACTIVE_KEY_ID": "kid-1", "AUTHKIT_ACTIVE_PRIVATE_KEY_PEM": "-----BEGIN PRIVATE KEY-----", "AUTHKIT_PUBLIC_KEYS": `{"kid-0":"pem"}`,
		"AUTH_ISSUER": "https://billing.example.com/", "AUTH_DIRECT_PEER_IP": "true",
	} {
		t.Setenv(key, value)
	}
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, [3]string{"example.com", "user", "pass"}, [3]string{cfg.DB.Host, cfg.DB.Username, cfg.DB.Password})
	require.True(t, cfg.DB.SQLTrace)
	require.Equal(t, "custom_billing", cfg.DB.Schema)
	require.Contains(t, cfg.DB.URL, "@example.com:")
	require.True(t, cfg.Vault.Enabled)
	require.Equal(t, "http://127.0.0.1:8200", cfg.Vault.Address)
	require.Equal(t, billing.SecretBackendDB, cfg.SecretStoreBackend())
	require.Equal(t, "SG.test-key", cfg.SendGrid.APIKey)
	require.True(t, cfg.IsLimitedMode())
	require.False(t, cfg.IsProviderReadOnly())
	interval, enabled, err := cfg.CatalogReconciliationSchedule()
	require.NoError(t, err)
	require.True(t, enabled)
	require.Equal(t, 30*time.Minute, interval)
	quiet, err := cfg.ProviderBillingQuiescence()
	require.NoError(t, err)
	require.Equal(t, 36*time.Hour, quiet)
	require.True(t, cfg.AllowCatalogUpdates)
	require.True(t, cfg.MerchantConfigHTTP)
	require.Equal(t, []string{"10.0.0.0/8"}, cfg.TrustedProxies)
	require.Equal(t, "kid-1", cfg.Auth.ActiveKeyID)
	require.Equal(t, "-----BEGIN PRIVATE KEY-----", cfg.Auth.ActivePrivateKeyPEM)
	require.Equal(t, `{"kid-0":"pem"}`, cfg.Auth.PublicKeysJSON, "public keys stay a JSON string, never decoded")
	require.Equal(t, "https://billing.example.com/", cfg.Auth.Issuer)
	require.True(t, cfg.Auth.DirectPeerIP)

	t.Setenv("DB_URL", "postgres://u:p@localhost:5432/db?sslmode=disable")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, "postgres://u:p@localhost:5432/db?sslmode=disable", cfg.DB.URL, "an explicit URL beats the parts")

	for key, value := range map[string]string{"ALLOW_CATALOG_UPDATES": "maybe", "TRUSTED_PROXIES": "0.0.0.0/0", "DB_SCHEMA": "bad schema"} {
		t.Run("refuses "+key, func(t *testing.T) {
			t.Setenv(key, value)
			_, err := Load("")
			require.Error(t, err)
		})
	}
}

func TestEnvKeyRouting(t *testing.T) {
	for env, key := range map[string]string{
		"SECRET_BACKEND":                    "secret_backend", // #710: multi-word top-level scalar
		"ALLOW_CATALOG_UPDATES":             "allow_catalog_updates",
		"DB_URL":                            "db.url",
		"db_url":                            "db.url",
		"VAULT_ADDR":                        "vault.address",
		"AUTHKIT_ACTIVE_KEY_ID":             "auth.active_key_id",
		"AUTHKIT_KEYS_PATH":                 "auth.keys_path",
		"AUTH_NAMING_FORMER_NAMES_DURATION": "auth.naming.former_names.duration",
		"PROVIDER_SANDBOX_NMI_GATEWAY_URL":  "provider_sandbox.nmi_gateway_url",
		"CATALOG_SOURCE":                    "",
		"MERCHANT_SOURCE":                   "",
		"MERCHANT_CONFIG_SOURCE":            "",
		"PATH":                              "",
		"DB_":                               "",
	} {
		require.Equal(t, key, envKeyToConfigKey(env), env)
	}
}

// yaml < mounted secret file < env < flag; flags never travel through the
// process environment.
func TestLoadSourcePrecedence(t *testing.T) {
	secrets := bootEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "config.yaml"),
		"db:\n  password: from-yaml\nrate_limits:\n  checkout:\n    requests_per_minute: 99\n")
	password := func(opts ...LoadOption) string {
		t.Helper()
		cfg, err := Load(path, opts...)
		require.NoError(t, err)
		return cfg.DB.Password
	}
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "from-yaml", cfg.DB.Password)
	require.Equal(t, 99, (*cfg.RateLimits)["checkout"].RequestsPerMinute)
	require.Equal(t, 20, (*cfg.RateLimits)["subscribe"].RequestsPerMinute, "a partial map keeps the other defaults")

	writeFile(t, filepath.Join(secrets, "DB_PASSWORD"), "from-file\n")
	require.Equal(t, "from-file", password())
	t.Setenv("DB_PASSWORD", "from-env")
	require.Equal(t, "from-env", password())
	require.Equal(t, "from-flag", password(WithOverride("db.password", "from-flag")))
	require.Equal(t, "from-env", os.Getenv("DB_PASSWORD"), "flags never write the process env")

	unsetenv(t, "TEST_MODE")
	_, err = Load(path)
	require.ErrorContains(t, err, "test_mode is required", "posture is never inherited by omission")
	cfg, err = Load(path, WithOverride("test_mode", "live"))
	require.NoError(t, err)
	require.Equal(t, billing.CredentialPostureLive, cfg.TestMode)
	t.Setenv("TEST_MODE", "sandbox")
	cfg, err = Load(path, WithOverride("test_mode", "live"))
	require.NoError(t, err)
	require.Equal(t, billing.CredentialPostureLive, cfg.TestMode)
	t.Setenv("TEST_MODE", "true")
	_, err = Load(path)
	require.ErrorContains(t, err, "invalid test_mode")
}

func TestLoadDatabaseIgnoresServerConfiguration(t *testing.T) {
	bootEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "config.yaml"),
		"db:\n  url: postgres://file.invalid/db\n  schema: Archive_Test\nauth:\n  invalid_server_field: true\nprovider_write_mode: invalid\n")
	t.Setenv("DB_URL", "postgres://env.invalid/db")
	unsetenv(t, "TEST_MODE")
	cfg, err := LoadDatabase(path)
	require.NoError(t, err)
	require.Equal(t, "postgres://env.invalid/db", cfg.DB.URL)
	require.Equal(t, "archive_test", cfg.DB.Schema)
	require.Nil(t, cfg.Auth, "never a server configuration")
	require.Nil(t, cfg.Redis)
	cfg, err = LoadDatabase(path, WithOverride("db.url", "postgres://flag.invalid/db"))
	require.NoError(t, err)
	require.Equal(t, "postgres://flag.invalid/db", cfg.DB.URL)
	_, err = Load(path)
	require.Error(t, err, "the server loader still refuses the same file")

	for _, fields := range []string{"  unknown: true\n", "  schema: bad-schema\n", "  require_rls: false\n"} {
		_, err := LoadDatabase(writeFile(t, filepath.Join(t.TempDir(), "config.yaml"), "db:\n"+fields))
		require.Error(t, err, fields)
	}
}

func TestAuthNamingDefaultsAndExplicitZero(t *testing.T) {
	bootEnv(t)
	for name, row := range map[string]struct {
		values   map[string]any
		enabled  bool
		interval time.Duration
		mode     authkit.FormerNameRetentionMode
		duration time.Duration
	}{
		"defaults":              {nil, true, 72 * time.Hour, authkit.FormerNamesFinite, 90 * 24 * time.Hour},
		"disabled zero forever": {map[string]any{"auth.naming.enabled": false, "auth.naming.rename_interval": "0s", "auth.naming.former_names.mode": "forever"}, false, 0, authkit.FormerNamesForever, 0},
		"finite duration":       {map[string]any{"auth.naming.former_names.duration": "240h"}, true, 72 * time.Hour, authkit.FormerNamesFinite, 240 * time.Hour},
		"immediate":             {map[string]any{"auth.naming.former_names.mode": "immediate"}, true, 72 * time.Hour, authkit.FormerNamesImmediate, 0},
	} {
		var opts []LoadOption
		for key, value := range row.values {
			opts = append(opts, WithOverride(key, value))
		}
		cfg, err := Load("", opts...)
		require.NoError(t, err, name)
		policy, err := cfg.Auth.Naming.Normalize()
		require.NoError(t, err, name)
		require.Equal(t, row.enabled, policy.Enabled, name)
		require.Equal(t, row.interval, policy.RenameInterval, name)
		require.Equal(t, row.mode, policy.FormerNameRetentionMode, name)
		require.Equal(t, row.duration, policy.FormerNameRetention, name)
	}
	_, err := Load("", WithOverride("auth.naming.rename_interval", "999999999999999999999h"))
	require.Error(t, err, "duration overflow must not become an immediate rename")

	// Explicit false/0 from env must survive as set values, not read as unset.
	t.Setenv("AUTH_NAMING_ENABLED", "false")
	t.Setenv("AUTH_NAMING_RENAME_INTERVAL", "0s")
	t.Setenv("AUTH_NAMING_FORMER_NAMES_MODE", "finite")
	t.Setenv("AUTH_NAMING_FORMER_NAMES_DURATION", "0s")
	cfg, err := Load("")
	require.NoError(t, err)
	require.NotNil(t, cfg.Auth.Naming.Enabled)
	require.False(t, *cfg.Auth.Naming.Enabled)
	require.NotNil(t, cfg.Auth.Naming.RenameInterval)
	require.Zero(t, *cfg.Auth.Naming.RenameInterval)
	policy, err := cfg.Auth.Naming.Normalize()
	require.NoError(t, err)
	require.Equal(t, authkit.FormerNamesImmediate, policy.FormerNameRetentionMode)
}

// Sandbox posture never relaxes auth transport; only the explicit loopback
// exception admits HTTP, and only to a loopback host.
func TestAuthTransportIsExplicit(t *testing.T) {
	cfg := &Config{Config: billing.GetDefaultBillingConfig(), Auth: &AuthConfig{}}
	cfg.TestMode, cfg.ProviderWriteMode = billing.CredentialPostureSandbox, billing.ProviderWriteModeFull
	cfg.DB.URL = cfg.DB.GetConnectionString()
	require.NoError(t, Validate(cfg))

	for _, row := range []struct {
		issuer, origin string
		loopback       bool
		want           string
	}{
		{"https://auth.example.com", "", false, ""},
		{"http://auth.internal:8080", "", false, "must use HTTPS"},
		{"http://127.0.0.1:3053", "", false, "must use HTTPS"},
		{"http://127.0.0.1:3053", "", true, ""},
		{"http://localhost:3053", "http://localhost:3053", true, ""},
		{"http://10.0.0.2", "", true, "must use HTTPS"},
		{"https://user:pw@auth.example.com", "", false, "without credentials"},
		{"https://auth.example.com?x=1", "", false, "without credentials, query or fragment"},
		{"https://issuer.example", "https://billing.example/billing", false, "without a path"},
		{"https://issuer.example", "https://billing.example/", false, ""},
	} {
		cfg.Auth.Issuer, cfg.Auth.RequestOrigin, cfg.Auth.AllowLoopbackHTTP = row.issuer, row.origin, row.loopback
		err := Validate(cfg)
		if row.want == "" {
			require.NoError(t, err, row.issuer)
		} else {
			require.ErrorContains(t, err, row.want, row.issuer)
		}
	}
	require.False(t, cfg.Auth.AllowMemory || cfg.Auth.AllowPrivateNetworkJWKS || cfg.Auth.AllowMissingSenders || cfg.Auth.AllowEphemeralSigningKey)
	require.ErrorContains(t, Validate(&Config{}), "standalone config is required")
}
