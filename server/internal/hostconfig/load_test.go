package hostconfig

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	billing "github.com/open-rails/openrails/internal/config"
	"github.com/stretchr/testify/require"
)

// testDatabaseURL is the database bootEnv names.
const testDatabaseURL = "postgres://openrails@db.test:5432/openrails?sslmode=require"

// bootEnv isolates Load from the working directory (.env, config.yaml), the
// host's mounted secrets and ambient database env, and declares the database
// and the two required dials.
func bootEnv(t *testing.T) (secretsDir string) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, key := range []string{"DB_URL", "DB_HOST", "DB_PORT", "DB_DATABASE", "DB_USERNAME", "DB_PASSWORD", "DB_SSLMODE", "REDIS_ADDR", "REDIS_URL", "OPENRAILS_CONFIG", "BILLING_CONFIG"} {
		unsetenv(t, key)
	}
	secretsDir = t.TempDir()
	t.Setenv("VAULT_SECRETS_PATH", secretsDir)
	t.Setenv("DB_URL", testDatabaseURL)
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
	require.Equal(t, billing.DefaultSchema, cfg.Database.Schema)
	require.Equal(t, testDatabaseURL, cfg.DB.URL)
	require.NotNil(t, cfg.Auth)
	require.Empty(t, cfg.Auth.Issuer, "no URL setting supplies an issuer fallback")

	unsetenv(t, "DB_URL")
	for key, value := range map[string]string{
		"DB_HOST": "  example.com  ", "DB_PORT": "5432", "DB_DATABASE": "openrails", "DB_USERNAME": "  user  ", "DB_PASSWORD": "  pass  ", "DB_SQL_TRACE": "true", "DATABASE_SCHEMA": "  Custom_Billing  ", "DATABASE_RIVER_SCHEMA": "jobs",
		"VAULT_ADDR": "http://127.0.0.1:8200", "VAULT_TOKEN": "root", "VAULT_KV_MOUNT": "kv",
		"EMAIL_SMTP_HOST": "smtp.sendgrid.net", "EMAIL_SMTP_PORT": "587", "EMAIL_SMTP_USERNAME": "apikey", "EMAIL_SMTP_PASSWORD": "SG.test-key", "EMAIL_SMTP_FROM": "Billing <noreply@billing.example>",
		"PROVIDER_WRITE_MODE": "limited", "CATALOG_RECONCILIATION_INTERVAL": "30m", "PROVIDER_BILLING_QUIESCENCE_INTERVAL": "36h",
		"TRUSTED_PROXIES":       `["10.0.0.0/8"]`,
		"AUTHKIT_ACTIVE_KEY_ID": "kid-1", "AUTHKIT_ACTIVE_PRIVATE_KEY_PEM": "-----BEGIN PRIVATE KEY-----", "AUTHKIT_PUBLIC_KEYS": `{"kid-0":"pem"}`,
		"AUTH_ISSUER": "https://billing.example.com/", "AUTH_DIRECT_PEER_IP": "true",
	} {
		t.Setenv(key, value)
	}
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, [3]string{"example.com", "user", "pass"}, [3]string{cfg.DB.Host, cfg.DB.Username, cfg.DB.Password})
	require.True(t, cfg.DB.SQLTrace)
	require.Equal(t, "custom_billing", cfg.Database.Schema)
	require.Equal(t, "jobs", cfg.Database.RiverSchema)
	require.Equal(t, "postgresql://user:pass@example.com:5432/openrails?sslmode=require", cfg.DB.URL, "parts without db.sslmode require TLS")
	require.NotNil(t, cfg.Vault, "a VAULT_* setting declares the connection")
	require.Equal(t, "http://127.0.0.1:8200", cfg.Vault.Address)
	require.Equal(t, "kv", billing.MerchantConfigKVMount(cfg.Config))
	require.Equal(t, billing.SMTPConfig{Host: "smtp.sendgrid.net", Port: 587, Username: "apikey", Password: "SG.test-key",
		From: billing.EmailAddress{Name: "Billing", Address: "noreply@billing.example"}}, *cfg.SMTP)
	require.True(t, billing.IsLimitedMode(cfg.Config))
	require.False(t, billing.IsProviderReadOnly(cfg.Config))
	interval, enabled, err := billing.CatalogReconciliationSchedule(cfg.Config)
	require.NoError(t, err)
	require.True(t, enabled)
	require.Equal(t, 30*time.Minute, interval)
	quiet, err := billing.ProviderBillingQuiescence(cfg.Config)
	require.NoError(t, err)
	require.Equal(t, 36*time.Hour, quiet)
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

	for key, value := range map[string]string{"CATALOG_EDITS": "maybe", "TRUSTED_PROXIES": "0.0.0.0/0", "DATABASE_SCHEMA": "bad schema", "DB_SCHEMA": "billing"} {
		t.Run("refuses "+key, func(t *testing.T) {
			t.Setenv(key, value)
			_, err := Load("")
			require.Error(t, err)
		})
	}
}

func TestEnvKeyRouting(t *testing.T) {
	for env, key := range map[string]string{
		"PUBLIC_BILLING_BASE_URL":           "public_billing_base_url", // #710: multi-word top-level scalar
		"SECRET_BACKEND":                    "",
		"CATALOG_EDITS":                     "",
		"DB_URL":                            "db.url",
		"db_url":                            "db.url",
		"VAULT_ADDR":                        "vault.address",
		"VAULT_AUTH_MOUNT":                  "vault.auth_mount",
		"AUTHKIT_ACTIVE_KEY_ID":             "auth.active_key_id",
		"AUTHKIT_KEYS_PATH":                 "auth.keys_path",
		"AUTH_NAMING_FORMER_NAMES_DURATION": "auth.naming.former_names.duration",
		"PROVIDER_SANDBOX_NMI_GATEWAY_URL":  "provider_sandbox.nmi_gateway_url",
		"EMAIL_SMTP_PASSWORD":               "email_smtp.password",
		"SENDGRID_API_KEY":                  "",
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

// rate_limits_disabled turns the defaults off, from the file or the environment.
func TestRateLimitsDisabled(t *testing.T) {
	bootEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "config.yaml"), "db:\n  password: x\n")
	cfg, err := Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.RateLimits, "on by default")
	t.Setenv("RATE_LIMITS_DISABLED", "true")
	cfg, err = Load(path)
	require.NoError(t, err)
	require.True(t, cfg.RateLimitsDisabled)
	require.Nil(t, cfg.RateLimits)
	require.Nil(t, cfg.Captcha)
}

func TestLoadDatabaseIgnoresServerConfiguration(t *testing.T) {
	bootEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "config.yaml"),
		"database:\n  schema: Archive_Test\ndb:\n  url: postgres://file.invalid/db\nauth:\n  invalid_server_field: true\nprovider_write_mode: invalid\n")
	t.Setenv("DB_URL", "postgres://env.invalid/db")
	unsetenv(t, "TEST_MODE")
	cfg, err := LoadDatabase(path)
	require.NoError(t, err)
	require.Equal(t, "postgres://env.invalid/db", cfg.DB.URL)
	require.Equal(t, "archive_test", cfg.Database.Schema)
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
		mode     billing.FormerNamesMode
		duration time.Duration
	}{
		"defaults":              {nil, true, 72 * time.Hour, billing.FormerNamesFinite, 90 * 24 * time.Hour},
		"disabled zero forever": {map[string]any{"auth.naming.enabled": false, "auth.naming.rename_interval": "0s", "auth.naming.former_names.mode": "forever"}, false, 0, billing.FormerNamesForever, 0},
		"finite duration":       {map[string]any{"auth.naming.former_names.duration": "240h"}, true, 72 * time.Hour, billing.FormerNamesFinite, 240 * time.Hour},
		"immediate":             {map[string]any{"auth.naming.former_names.mode": "immediate"}, true, 72 * time.Hour, billing.FormerNamesImmediate, 0},
	} {
		var opts []LoadOption
		for key, value := range row.values {
			opts = append(opts, WithOverride(key, value))
		}
		cfg, err := Load("", opts...)
		require.NoError(t, err, name)
		policy, err := billing.NormalizeNaming(cfg.Auth.Naming)
		require.NoError(t, err, name)
		require.Equal(t, row.enabled, policy.Enabled, name)
		require.Equal(t, row.interval, policy.RenameInterval, name)
		require.Equal(t, row.mode, policy.FormerNames, name)
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
	policy, err := billing.NormalizeNaming(cfg.Auth.Naming)
	require.NoError(t, err)
	require.Equal(t, billing.FormerNamesImmediate, policy.FormerNames)
}

// Sandbox posture never relaxes auth transport; only the explicit loopback
// exception admits HTTP, and only to a loopback host.
func TestAuthTransportIsExplicit(t *testing.T) {
	f := defaults()
	f.TestMode, f.ProviderWriteMode = "sandbox", billing.ProviderWriteModeFull
	f.DB.URL = testDatabaseURL
	cfg, err := f.config()
	require.NoError(t, err)
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
	require.False(t, cfg.Auth.AllowPrivateNetworkJWKS || cfg.Auth.AllowMissingSenders || cfg.Auth.AllowEphemeralSigningKey)
	require.ErrorContains(t, Validate(&Config{}), "standalone config is required")
}

// No database is assumed: without one every loader refuses and names the
// settings; parts never borrow a local default.
func TestLoadRequiresDatabase(t *testing.T) {
	bootEnv(t)
	unsetenv(t, "DB_URL")
	const none = "no database configured: set db.url (DB_URL), or db.host, db.port, db.database and db.username (DB_HOST, DB_PORT, DB_DATABASE, DB_USERNAME)"
	_, err := Load("")
	require.EqualError(t, err, none)
	_, err = LoadDatabase("")
	require.EqualError(t, err, none)

	t.Setenv("DB_HOST", "db.internal")
	t.Setenv("DB_PASSWORD", "secret")
	_, err = Load("")
	require.EqualError(t, err, "database: db.url (DB_URL) is unset and its parts are incomplete: set db.port (DB_PORT), db.database (DB_DATABASE), db.username (DB_USERNAME)")

	t.Setenv("DB_PORT", "5432")
	t.Setenv("DB_DATABASE", "openrails")
	t.Setenv("DB_USERNAME", "openrails")
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "postgresql://openrails:secret@db.internal:5432/openrails?sslmode=require", cfg.DB.URL)
}

// The binary drains by default; both settings come from env too.
func TestShutdownSettings(t *testing.T) {
	bootEnv(t)
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, [2]time.Duration{5 * time.Second, 20 * time.Second}, [2]time.Duration{cfg.DrainDelay, cfg.ShutdownTimeout})

	t.Setenv("DRAIN_DELAY", "0s")
	t.Setenv("SHUTDOWN_TIMEOUT", "45s")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, [2]time.Duration{0, 45 * time.Second}, [2]time.Duration{cfg.DrainDelay, cfg.ShutdownTimeout})

	t.Setenv("DRAIN_DELAY", "-1s")
	_, err = Load("")
	require.ErrorContains(t, err, "must not be negative")
}

// No Redis unless one is named; each connection setting has its variable.
func TestRedisSettings(t *testing.T) {
	bootEnv(t)
	cfg, err := Load("")
	require.NoError(t, err)
	require.Nil(t, cfg.Redis, "no default Redis")
	t.Setenv("REDIS_PASSWORD", "")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Nil(t, cfg.Redis, "a blank variable declares none")

	t.Setenv("REDIS_URL", "rediss://cache.example:6380/2")
	t.Setenv("REDIS_USERNAME", "openrails")
	t.Setenv("REDIS_PASSWORD", "s3cret")
	t.Setenv("REDIS_CA_CERT", "not a certificate")
	_, err = Load("")
	require.ErrorContains(t, err, "redis.ca_cert holds no PEM certificate")
	unsetenv(t, "REDIS_CA_CERT")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, &billing.RedisConfig{URL: "rediss://cache.example:6380/2", Username: "openrails", Password: "s3cret"}, cfg.Redis)

	t.Setenv("REDIS_ADDR", "cache:6379")
	_, err = Load("")
	require.ErrorContains(t, err, "redis.addr and redis.url are exclusive")
	unsetenv(t, "REDIS_URL")
	t.Setenv("REDIS_TLS", "true")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, &billing.RedisConfig{Addr: "cache:6379", Username: "openrails", Password: "s3cret", TLS: true}, cfg.Redis)

	unsetenv(t, "REDIS_ADDR")
	_, err = Load("")
	require.ErrorContains(t, err, "redis: set redis.addr (REDIS_ADDR) or redis.url (REDIS_URL)")
}
