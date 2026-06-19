package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_EnvMapping(t *testing.T) {
	// Issue #222 removed the SERVICE_MTLS_* env surface entirely (no private/mTLS
	// service listener). These cases assert the remaining env-mapping behaviour.
	t.Run("loads nested keys via single underscore (db.url)", func(t *testing.T) {
		t.Setenv("DB_URL", "postgres://u:p@localhost:5432/db?sslmode=disable")

		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.Equal(t, "postgres://u:p@localhost:5432/db?sslmode=disable", cfg.DB.URL)
	})

	t.Run("ignores auth issuers from env (#521)", func(t *testing.T) {
		t.Setenv("AUTH_ISSUERS", `["http://a.test","http://b.test"]`)
		t.Setenv("AUTH_EXPECTED_AUDIENCE", "legacy-audience")

		cfg, err := Load("nonexistent-config.yaml")
		require.NoError(t, err)
		require.NotNil(t, cfg.Auth)
	})

	t.Run("ignores merchant from env (#520/#521)", func(t *testing.T) {
		t.Setenv("MERCHANT", "doujins")

		_, err := Load("nonexistent-config.yaml")
		require.NoError(t, err)
	})

	t.Run("ignores cors origins from env (#519)", func(t *testing.T) {
		t.Setenv("CORS_ORIGINS", "https://app.example.com")

		_, err := Load("nonexistent-config.yaml")
		require.NoError(t, err)
	})

	t.Run("maps canonical Vault env vars to vault.* (#251)", func(t *testing.T) {
		t.Setenv("VAULT_ENABLED", "true")
		t.Setenv("VAULT_ADDR", "http://127.0.0.1:8200")
		t.Setenv("VAULT_TOKEN", "root")
		t.Setenv("VAULT_AUTH_METHOD", "token")

		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		if assert.NotNil(t, cfg.Vault) {
			assert.True(t, cfg.Vault.Enabled)
			assert.Equal(t, "http://127.0.0.1:8200", cfg.Vault.Address)
			assert.Equal(t, "root", cfg.Vault.Token)
			assert.Equal(t, "token", cfg.Vault.AuthMethod)
		}
	})

	t.Run("maps TEST_ENV to the top-level test_env bool (#355)", func(t *testing.T) {
		t.Setenv("TEST_ENV", "true")

		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.True(t, cfg.TestEnv)
		assert.True(t, cfg.IsTestEnv())
	})

	t.Run("defaults to sandbox in development when test_env is unset (#355)", func(t *testing.T) {
		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.True(t, cfg.IsDev(), "default env is development")
		assert.True(t, cfg.TestEnv, "unset test_env defaults to sandbox in dev — a local boot can never silently use live credentials")
		assert.True(t, cfg.IsTestEnv())
	})

	t.Run("explicit test_env=false runs live in development (#355)", func(t *testing.T) {
		t.Setenv("TEST_ENV", "false")
		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.True(t, cfg.IsDev())
		assert.False(t, cfg.TestEnv, "explicit false is honored over the dev sandbox default")
		assert.False(t, cfg.IsTestEnv())
	})

	t.Run("rejects removed hot-path fail policy env vars", func(t *testing.T) {
		t.Setenv("OPENRAILS_BILLING_HOT_PATH_FAIL_POLICY", "fail_open")

		_, err := Load("nonexistent-config.yaml")
		assert.ErrorContains(t, err, "billing_hot_path was removed")
	})
}

func TestLoad_ConfigFileAndEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(cfgPath, []byte(`
db:
  host: filehost
  port: "5432"
  database: filedb
  username: fileuser
  password: filepass
  sslmode: disable

clickhouse:
  client_addr: clickhouse:9000
  http_addr: http://clickhouse:8123
  db: analytics
  user: analytics_user
  password: analytics_password
`), 0o600)
	assert.NoError(t, err)

	// Env overrides config file values.
	t.Setenv("DB_USERNAME", "envuser")
	t.Setenv("DB_PASSWORD", "envpass")
	t.Setenv("CLICKHOUSE_CLIENT_ADDR", "envclickhouse:9000")

	cfg, err := Load(cfgPath)
	assert.NoError(t, err)

	// From file (not overridden)
	assert.Equal(t, "filehost", cfg.DB.Host)
	assert.Equal(t, "filedb", cfg.DB.Database)

	// Overridden by env
	assert.Equal(t, "envuser", cfg.DB.Username)
	assert.Equal(t, "envpass", cfg.DB.Password)
	assert.Equal(t, "envclickhouse:9000", cfg.ClickHouse.ClientAddr)
}

func TestConfigExampleShowsCodeDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "config.example.yaml"))
	require.NoError(t, err)
	defaults := GetDefaultBillingConfig()

	require.NotNil(t, cfg.DB)
	require.NotNil(t, cfg.Redis)
	require.NotNil(t, cfg.ClickHouse)
	require.NotNil(t, cfg.Auth)

	assert.Equal(t, defaults.Env, cfg.Env)
	assert.Equal(t, defaults.Host, cfg.Host)
	assert.Equal(t, defaults.Port, cfg.Port)
	assert.Equal(t, defaults.APIURL, cfg.APIURL)
	assert.Equal(t, defaults.DB.Host, cfg.DB.Host)
	assert.Equal(t, defaults.DB.Port, cfg.DB.Port)
	assert.Equal(t, defaults.DB.Database, cfg.DB.Database)
	assert.Equal(t, defaults.DB.Username, cfg.DB.Username)
	assert.Equal(t, defaults.DB.Password, cfg.DB.Password)
	assert.Equal(t, defaults.DB.SSLMode, cfg.DB.SSLMode)
	assert.Equal(t, defaults.DB.SchemaName(), cfg.DB.SchemaName())
	assert.Equal(t, defaults.Redis.Addr, cfg.Redis.Addr)
	assert.Equal(t, defaults.Redis.Password, cfg.Redis.Password)
	assert.Equal(t, defaults.Redis.DB, cfg.Redis.DB)
	assert.Equal(t, defaults.ClickHouse.HTTPAddr, cfg.ClickHouse.HTTPAddr)
	assert.Equal(t, defaults.ClickHouse.ClientAddr, cfg.ClickHouse.ClientAddr)
	assert.Equal(t, defaults.ClickHouse.Database, cfg.ClickHouse.Database)
	assert.Equal(t, defaults.ClickHouse.Username, cfg.ClickHouse.Username)
	assert.Equal(t, defaults.ClickHouse.Password, cfg.ClickHouse.Password)
	assert.Equal(t, defaults.ClickHouse.Cluster, cfg.ClickHouse.Cluster)
	assert.Equal(t, "http://localhost:2053", cfg.Auth.Issuer)
}

func TestLoad_RejectsRemovedControlPlaneEnabledKnob(t *testing.T) {
	// HARD CUT (#469): the control plane is always on; auth.control_plane.enabled
	// is a load error, not an ignored key — whichever way it was set.
	for _, val := range []string{"true", "false"} {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		require.NoError(t, os.WriteFile(cfgPath, []byte(`
auth:
  control_plane:
    enabled: `+val+`
    issuer: "https://openrails.example.com"
`), 0o600))

		_, err := Load(cfgPath)
		require.Error(t, err)
		require.ErrorContains(t, err, "auth.control_plane.enabled was removed")
	}
}

func TestLoad_RejectsRemovedBillingHotPathConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
billing_hot_path:
  fail_policy: fail_open
`), 0o600))

	_, err := Load(cfgPath)
	require.Error(t, err)
	require.ErrorContains(t, err, "billing_hot_path was removed")
}

func TestLoad_RejectsRemovedMerchantCORSConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
merchant_cors:
  doujins:
    allowed_origins:
      - https://doujins.com
`), 0o600))

	_, err := Load(cfgPath)
	require.Error(t, err)
	require.ErrorContains(t, err, "merchant_cors was removed")
}

func TestLoad_IgnoresRemovedStoreConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
store:
  name: Doujins
  from_email: billing@example.com
`), 0o600))

	_, err := Load(cfgPath)
	require.NoError(t, err)

	t.Setenv("STORE_FROM_EMAIL", "billing@example.com")
	_, err = Load("nonexistent-config.yaml")
	require.NoError(t, err)
}

func TestLoad_IgnoresRemovedMerchantConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
merchant: doujins
`), 0o600))

	_, err := Load(cfgPath)
	require.NoError(t, err)
}

func TestLoad_ControlPlaneMaterializedWithDevIssuerDefault(t *testing.T) {
	// The control plane is mandatory (#469); in development auth.issuer defaults
	// to the deployment's own base URL.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("env: development\n"), 0o600))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.NotNil(t, cfg.Auth)
	require.Equal(t, "http://localhost:2053", cfg.Auth.Issuer)
}

func TestLoad_ControlPlaneIssuerFromAPIURLInDev(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("env: development\napi_url: \"http://openrails:2053/\"\n"), 0o600))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.Equal(t, "http://openrails:2053", cfg.Auth.Issuer)
}

func TestLoad_AuthIssuerFromEnv(t *testing.T) {
	t.Setenv("AUTH_ISSUER", "https://billing.example.com/")

	cfg, err := Load("nonexistent-config.yaml")
	require.NoError(t, err)
	require.Equal(t, "https://billing.example.com/", cfg.Auth.Issuer)
}

func TestLoad_IgnoresProcessorConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(cfgPath, []byte(`
processors:
  acme:
    security_key: test-key
`), 0o600)
	assert.NoError(t, err)

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.NotNil(t, cfg)
}

func TestLoad_IgnoresProcessorConfigEnv(t *testing.T) {
	t.Setenv("PROCESSORS_MOBIUS_TYPE", "")
	t.Setenv("PROCESSORS_MOBIUS_TOKENIZATION_KEY", "")
	t.Setenv("PROCESSORS_MOBIUS_WEBHOOK_SECRET", "")
	t.Setenv("PROCESSORS_MOBIUS_SECURITY_KEY", "test-key")

	cfg, err := Load("nonexistent-config.yaml")
	require.NoError(t, err)
	require.NotNil(t, cfg)
}

func TestValidateCaptchaRejectsHalfConfiguredCredentials(t *testing.T) {
	cfg := GetDefaultBillingConfig()
	cfg.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
	cfg.Captcha.Provider = CaptchaProviderTurnstile
	cfg.Captcha.SiteKey = ""
	cfg.Captcha.SecretKey = "secret-key"

	// There is no enabled knob (#353): credentials ARE the signal, so the only
	// invalid state is half a pair.
	err := Validate(cfg)
	assert.Error(t, err)
	assert.ErrorContains(t, err, "BOTH site_key and secret_key")

	cfg.Captcha.SiteKey = "site-key"
	assert.NoError(t, Validate(cfg))
	assert.True(t, cfg.Captcha.IsEnabled())

	cfg.Captcha.SiteKey = ""
	cfg.Captcha.SecretKey = ""
	assert.NoError(t, Validate(cfg))
	assert.False(t, cfg.Captcha.IsEnabled())
}

func TestValidateCaptchaRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*CaptchaConfig)
		wantError string
	}{
		{
			name:      "legacy recaptcha provider unsupported",
			mutate:    func(c *CaptchaConfig) { c.Provider = "recaptcha" },
			wantError: "unsupported provider",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := GetDefaultBillingConfig()
			cfg.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
			cfg.Captcha.Provider = CaptchaProviderTurnstile
			cfg.Captcha.SiteKey = "site-key"
			cfg.Captcha.SecretKey = "secret-key"
			tt.mutate(cfg.Captcha)

			err := Validate(cfg)
			assert.Error(t, err)
			assert.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestCaptchaConfigDefaults(t *testing.T) {
	cfg := &CaptchaConfig{}
	assert.Equal(t, CaptchaProviderTurnstile, cfg.EffectiveProvider())
	assert.Equal(t, "https://challenges.cloudflare.com/turnstile/v0/siteverify", cfg.EffectiveVerifyURL())
	assert.Equal(t, "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit", cfg.EffectiveScriptURL())
	assert.Equal(t, "billing_challenge", cfg.EffectiveAction())
	assert.Equal(t, 3, cfg.EffectiveExtremeMultiplier())
	assert.Equal(t, "checkout", cfg.EffectiveChallengeBuckets()[0])
}

func TestCaptchaConfigRecaptchaV3Defaults(t *testing.T) {
	cfg := &CaptchaConfig{Provider: CaptchaProviderRecaptchaV3, SiteKey: "site key"}
	assert.Equal(t, "https://www.google.com/recaptcha/api/siteverify", cfg.EffectiveVerifyURL())
	assert.Equal(t, "https://www.google.com/recaptcha/api.js?render=site+key", cfg.EffectiveScriptURL())
}

func TestLoad_EnvTrimming(t *testing.T) {
	t.Setenv("DB_HOST", "  example.com  ")
	t.Setenv("DB_USERNAME", "  user  ")
	t.Setenv("DB_PASSWORD", "  pass  ")

	cfg, err := Load("nonexistent-config.yaml")
	assert.NoError(t, err)

	assert.Equal(t, "example.com", cfg.DB.Host)
	assert.Equal(t, "user", cfg.DB.Username)
	assert.Equal(t, "pass", cfg.DB.Password)
}

func TestIsTestEnv(t *testing.T) {
	t.Run("defaults to live credentials when unset", func(t *testing.T) {
		assert.False(t, (&Config{}).IsTestEnv())
	})

	t.Run("test_env=true is sandbox", func(t *testing.T) {
		assert.True(t, (&Config{TestEnv: true}).IsTestEnv())
	})

	t.Run("orthogonal to mode", func(t *testing.T) {
		assert.False(t, (&Config{Mode: ModeFull}).IsTestEnv())
		assert.False(t, (&Config{Mode: ModeLimited}).IsTestEnv())
		assert.False(t, (&Config{Mode: ModeReadOnly}).IsTestEnv())
		assert.True(t, (&Config{Mode: ModeFull, TestEnv: true}).IsTestEnv())
		assert.True(t, (&Config{Mode: ModeReadOnly, TestEnv: true}).IsTestEnv())
	})
}

func TestIsDev(t *testing.T) {
	t.Run("returns true for empty env", func(t *testing.T) {
		cfg := &Config{Env: ""}
		assert.True(t, cfg.IsDev())
	})

	t.Run("returns true for dev", func(t *testing.T) {
		cfg := &Config{Env: "dev"}
		assert.True(t, cfg.IsDev())
	})

	t.Run("returns true for development", func(t *testing.T) {
		cfg := &Config{Env: "development"}
		assert.True(t, cfg.IsDev())
	})

	t.Run("returns false for prod", func(t *testing.T) {
		cfg := &Config{Env: "prod"}
		assert.False(t, cfg.IsDev())
	})

	t.Run("returns false for production", func(t *testing.T) {
		cfg := &Config{Env: "production"}
		assert.False(t, cfg.IsDev())
	})

	t.Run("is case sensitive (expects lowercase)", func(t *testing.T) {
		// IsDev expects lowercase env values
		cfg := &Config{Env: "DEV"}
		assert.False(t, cfg.IsDev(), "uppercase DEV is not recognized as dev")
	})
}

func TestRequiresRLS(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want bool
	}{
		{"empty env is development", "", false},
		{"dev allows local privileged DB role", "dev", false},
		{"development allows local privileged DB role", "development", false},
		{"prod requires RLS-enforcing DB role", "prod", true},
		{"production requires RLS-enforcing DB role", "production", true},
		{"unknown non-dev env requires RLS", "staging", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, (&Config{Env: tc.env}).RequiresRLS())
		})
	}
}

func TestProductionTestEnvValidation(t *testing.T) {
	t.Run("prod env rejects test_env=true", func(t *testing.T) {
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.Mode = ModeFull
		cfg.TestEnv = true
		assembleDBURL(cfg)
		assert.False(t, cfg.IsDev(), "env=prod should not be dev")
		assert.ErrorContains(t, Validate(cfg), "test_env=true is not allowed outside development")
	})

	t.Run("prod env requires an explicit mode", func(t *testing.T) {
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.DB.Username = "billing_app"
		cfg.DB.Password = "production-db-password"
		cfg.ClickHouse.Username = "prod_analytics"
		cfg.ClickHouse.Password = "production-clickhouse-password"
		assembleDBURL(cfg)
		assert.ErrorContains(t, Validate(cfg), "mode is required outside development")
	})

	t.Run("a zero-value struct is live + full behavior (Load applies the dev sandbox default separately)", func(t *testing.T) {
		// This builds the struct directly, bypassing Load(), so the zero-value
		// TestEnv is live. The dev sandbox-by-default behavior is applied by
		// Load() and covered in TestLoad_EnvMapping (#355).
		cfg := GetDefaultBillingConfig()
		cfg.Env = "dev"
		assembleDBURL(cfg)
		assert.False(t, cfg.IsTestEnv(), "zero-value test_env is live; the dev sandbox default is a Load() concern")
		assert.False(t, cfg.IsLimitedMode(), "unset mode in dev defaults to full behavior")
		assert.NoError(t, Validate(cfg))
	})

	t.Run("dev env accepts test_env=true (sandbox)", func(t *testing.T) {
		cfg := GetDefaultBillingConfig()
		cfg.Env = "dev"
		cfg.TestEnv = true
		assembleDBURL(cfg)
		assert.True(t, cfg.IsDev(), "env=dev should be dev")
		assert.True(t, cfg.IsTestEnv())
		assert.NoError(t, Validate(cfg))
	})

	t.Run("prod env accepts mode=full", func(t *testing.T) {
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.Mode = ModeFull
		cfg.DB.Username = "billing_app"
		cfg.DB.Password = "production-db-password"
		cfg.ClickHouse.Username = "prod_analytics"
		cfg.ClickHouse.Password = "production-clickhouse-password"
		assembleDBURL(cfg)
		assert.NoError(t, Validate(cfg))
	})
}

func TestStripeKeyModeCompatibility(t *testing.T) {
	// These tests verify the documented Stripe key validation behavior
	// Note: Actual validation happens in Load(), these test the rules

	t.Run("test key sk_test_ prefix identified correctly", func(t *testing.T) {
		key := "sk_test_abc123"
		isTestKey := len(key) >= 8 && key[:8] == "sk_test_"
		assert.True(t, isTestKey)
	})

	t.Run("live key sk_live_ prefix identified correctly", func(t *testing.T) {
		key := "sk_live_abc123"
		isLiveKey := len(key) >= 8 && key[:8] == "sk_live_"
		assert.True(t, isLiveKey)
	})
}

// stripeTestEnvConfig builds a minimal Config plus in-memory ProcessorSet with a
// single Stripe processor carrying the given secret key and test_env setting.
func stripeTestEnvConfig(secretKey string, testEnv bool) (*Config, ProcessorSet) {
	return &Config{
			Mode:    ModeFull,
			TestEnv: testEnv,
		}, ProcessorSet{
			"stripe": {
				Type:      string(ProcessorTypeStripe),
				SecretKey: secretKey,
			},
		}
}

func TestValidateStripeKeyForTestEnv(t *testing.T) {
	// Standard secret keys (sk_*)
	t.Run("sk_live_ + test_env=true is a hard error (#347)", func(t *testing.T) {
		cfg, processors := stripeTestEnvConfig("sk_live_abc123", true)
		assert.Error(t, validateStripeKeyForTestEnv(cfg, processors), "live key in test env must refuse to boot")
	})

	t.Run("sk_test_ + test_env=true allowed", func(t *testing.T) {
		cfg, processors := stripeTestEnvConfig("sk_test_abc123", true)
		assert.NoError(t, validateStripeKeyForTestEnv(cfg, processors))
		assert.Equal(t, "sk_test_abc123", processors["stripe"].SecretKey, "test key in test env should be kept")
	})

	t.Run("sk_test_ + test_env=false disables Stripe", func(t *testing.T) {
		cfg, processors := stripeTestEnvConfig("sk_test_abc123", false)
		assert.NoError(t, validateStripeKeyForTestEnv(cfg, processors))
		assert.Empty(t, processors["stripe"].SecretKey, "test key in live env should be disabled")
	})

	t.Run("sk_live_ + test_env=false allowed", func(t *testing.T) {
		cfg, processors := stripeTestEnvConfig("sk_live_abc123", false)
		assert.NoError(t, validateStripeKeyForTestEnv(cfg, processors))
		assert.Equal(t, "sk_live_abc123", processors["stripe"].SecretKey, "live key in live env should be kept")
	})

	// Restricted keys (rk_*) — these must be classified the same as sk_* keys.
	t.Run("rk_live_ + test_env=true is a hard error (#347)", func(t *testing.T) {
		cfg, processors := stripeTestEnvConfig("rk_live_abc123", true)
		assert.Error(t, validateStripeKeyForTestEnv(cfg, processors), "restricted live key in test env must refuse to boot")
	})

	t.Run("rk_test_ + test_env=true allowed", func(t *testing.T) {
		cfg, processors := stripeTestEnvConfig("rk_test_abc123", true)
		assert.NoError(t, validateStripeKeyForTestEnv(cfg, processors))
		assert.Equal(t, "rk_test_abc123", processors["stripe"].SecretKey, "restricted test key in test env should be kept")
	})

	t.Run("rk_test_ + test_env=false disables Stripe", func(t *testing.T) {
		cfg, processors := stripeTestEnvConfig("rk_test_abc123", false)
		assert.NoError(t, validateStripeKeyForTestEnv(cfg, processors))
		assert.Empty(t, processors["stripe"].SecretKey, "restricted test key in live env should be disabled")
	})

	t.Run("rk_live_ + test_env=false allowed", func(t *testing.T) {
		cfg, processors := stripeTestEnvConfig("rk_live_abc123", false)
		assert.NoError(t, validateStripeKeyForTestEnv(cfg, processors))
		assert.Equal(t, "rk_live_abc123", processors["stripe"].SecretKey, "restricted live key in live env should be kept")
	})

	t.Run("validates every stripe processor", func(t *testing.T) {
		cfg := &Config{Mode: ModeFull, TestEnv: false}
		processors := ProcessorSet{
			"stripe_primary": {Type: ProcessorTypeStripe, SecretKey: "sk_live_primary"},
			"stripe_legacy":  {Type: ProcessorTypeStripe, Role: ProcessorRoleLegacy, SecretKey: "sk_test_legacy"},
		}
		require.NoError(t, validateStripeKeyForTestEnv(cfg, processors))
		require.Equal(t, "sk_live_primary", processors["stripe_primary"].SecretKey)
		require.Empty(t, processors["stripe_legacy"].SecretKey)
	})
}

func TestPrimaryProcessorByType(t *testing.T) {
	processors := ProcessorSet{
		"stripe_old": {Type: ProcessorTypeStripe, Role: ProcessorRoleLegacy, SecretKey: "sk_live_old"},
		"stripe_new": {Type: ProcessorTypeStripe, SecretKey: "sk_live_new"},
		"mobius":     {Type: ProcessorTypeNMI, SecurityKey: "sec"},
	}
	key, proc, err := processors.PrimaryProcessorByType(ProcessorTypeStripe)
	require.NoError(t, err)
	require.Equal(t, "stripe_new", key)
	require.Equal(t, "sk_live_new", proc.SecretKey)
	require.Equal(t, proc, processors.GetStripeProcessor())

	processors["stripe_other"] = &ProcessorConfig{Type: ProcessorTypeStripe, SecretKey: "sk_live_other"}
	_, _, err = processors.PrimaryProcessorByType(ProcessorTypeStripe)
	require.ErrorContains(t, err, "multiple primary processors")
	require.ErrorContains(t, ValidateProcessorSet(&Config{Mode: ModeFull}, ProcessorSet{
		"stripe_a": {Type: ProcessorTypeStripe, SecretKey: "sk_live_a"},
		"stripe_b": {Type: ProcessorTypeStripe, SecretKey: "sk_live_b"},
	}), "multiple primary processors")
}

func TestStripeLiveKeyRejectedInTestEnv(t *testing.T) {
	cfg := GetDefaultBillingConfig()
	cfg.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
	cfg.TestEnv = true
	processors := ProcessorSet{
		"stripe": {Type: "stripe", SecretKey: "sk_live_abc123"},
	}
	require.Error(t, ValidateProcessorSet(cfg, processors))

	// test key in the test env is fine
	processors["stripe"].SecretKey = "sk_test_abc123"
	require.NoError(t, ValidateProcessorSet(cfg, processors))

	// test key with live credentials expected: disabled with a warning, not fatal
	cfg2 := GetDefaultBillingConfig()
	cfg2.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
	cfg2.Mode = ModeLimited
	processors2 := ProcessorSet{
		"stripe": {
			Type:      "stripe",
			SecretKey: "sk_test_abc123",
		},
	}
	require.NoError(t, ValidateProcessorSet(cfg2, processors2))
	require.Equal(t, "", processors2["stripe"].SecretKey)
}

// TestDBConfig_SchemaName covers the issue #165 schema accessor: default `billing`,
// normalization (trim + lower-case), and nil-safety.
func TestDBConfig_SchemaName(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty defaults to openrails", "", "openrails"},
		{"explicit value preserved", "host_billing", "host_billing"},
		{"trimmed", "  custom  ", "custom"},
		{"lower-cased", "Openrails", "openrails"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, (&DBConfig{Schema: tc.raw}).SchemaName())
		})
	}

	t.Run("nil receiver defaults to openrails", func(t *testing.T) {
		var c *DBConfig
		assert.Equal(t, "openrails", c.SchemaName())
	})

	// The exported default constant is the OpenRails schema (#471).
	assert.Equal(t, "openrails", DefaultSchema)
}

// TestValidateSchema enforces that only safe SQL identifiers are accepted (#165).
func TestValidateSchema(t *testing.T) {
	valid := []string{"", "billing", "host_billing", "_x", "Schema1"}
	for _, s := range valid {
		assert.NoError(t, validateSchema(s), "expected %q valid", s)
	}
	invalid := []string{"1schema", "bad schema", "bad-schema", "bill;drop", `"quoted"`, "a.b"}
	for _, s := range invalid {
		assert.Error(t, validateSchema(s), "expected %q invalid", s)
	}
}

// TestLoad_DBSchemaEnv is the #165 regression test: DB_SCHEMA flows through config
// load, is normalized, and (default) resolves to openrails. A custom value is honored.
func TestLoad_DBSchemaEnv(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.Equal(t, "openrails", cfg.DB.SchemaName())
	})

	t.Run("custom honored and normalized", func(t *testing.T) {
		t.Setenv("DB_SCHEMA", "  Custom_Billing  ")
		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.Equal(t, "custom_billing", cfg.DB.Schema)
		assert.Equal(t, "custom_billing", cfg.DB.SchemaName())
	})

	t.Run("invalid rejected by Validate", func(t *testing.T) {
		t.Setenv("DB_SCHEMA", "bad schema")
		_, err := Load("nonexistent-config.yaml")
		assert.Error(t, err)
	})
}

func TestOperatingModes(t *testing.T) {
	// unset: nothing limited (dev default = full); test_env defaults off
	require.False(t, (&Config{}).IsLimitedMode())
	require.False(t, (&Config{}).IsProviderReadOnly())
	require.False(t, (&Config{}).IsTestEnv())

	require.False(t, (&Config{Mode: ModeFull}).IsLimitedMode())
	require.False(t, (&Config{Mode: ModeFull}).IsProviderReadOnly())

	limited := &Config{Mode: ModeLimited}
	require.True(t, limited.IsLimitedMode())
	require.False(t, limited.IsProviderReadOnly())

	ro := &Config{Mode: ModeReadOnly}
	require.True(t, ro.IsLimitedMode())
	require.True(t, ro.IsProviderReadOnly())

	// mode is pure behavior: test_env does not change the behavior gates
	roSandbox := &Config{Mode: ModeReadOnly, TestEnv: true}
	require.True(t, roSandbox.IsLimitedMode())
	require.True(t, roSandbox.IsProviderReadOnly())

	// case/space tolerant; unknown normalizes to "" pre-validation
	require.True(t, (&Config{Mode: " Limited "}).IsLimitedMode())
	require.False(t, (&Config{Mode: "redaonly"}).IsLimitedMode())
	require.Error(t, Validate(&Config{Mode: "redaonly"}))

	// hard cut (#355): the old mode values are gone, not aliased
	require.ErrorContains(t, Validate(&Config{Mode: "test"}), "must be one of full, limited, readonly")
	require.ErrorContains(t, Validate(&Config{Mode: "production"}), "must be one of full, limited, readonly")
}

func TestFlexiblePortRange(t *testing.T) {
	// #349: int16 wrapped every kernel-ephemeral port (>=32768) negative.
	var p FlexiblePort
	require.NoError(t, p.UnmarshalText([]byte("44553")))
	require.Equal(t, FlexiblePort(44553), p)
	require.NoError(t, p.UnmarshalText([]byte("65535")))
	require.Equal(t, FlexiblePort(65535), p)
	require.NoError(t, p.UnmarshalText([]byte(" 2053 ")))
	require.Equal(t, FlexiblePort(2053), p)
	require.NoError(t, p.UnmarshalText([]byte("")))
	require.Equal(t, FlexiblePort(0), p)

	require.Error(t, p.UnmarshalText([]byte("65536")))
	require.Error(t, p.UnmarshalText([]byte("0")))
	require.Error(t, p.UnmarshalText([]byte("-1")))
	require.Error(t, p.UnmarshalText([]byte("not-a-port")))

	// integer-typed values bypass UnmarshalText; Validate catches them
	cfg := GetDefaultBillingConfig()
	cfg.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
	cfg.Port = 44553
	require.NoError(t, Validate(cfg))
	cfg.Port = 70000
	require.ErrorContains(t, Validate(cfg), "invalid port")
	cfg.Port = -20983
	require.ErrorContains(t, Validate(cfg), "invalid port")
}

// TestRateLimitsPartialOverrideKeepsDefaults pins the #353 contract: rate
// limits are configurable, but overriding ONE endpoint must not wipe the
// built-in defaults for the others.
func TestRateLimitsPartialOverrideKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("rate_limits:\n  checkout:\n    requests_per_minute: 99\n"), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.RateLimits)
	rl := *cfg.RateLimits
	require.Equal(t, 99, rl["checkout"].RequestsPerMinute, "explicit override applies")
	require.NotNil(t, rl["subscribe"], "untouched endpoints keep their defaults")
	require.Equal(t, 20, rl["subscribe"].RequestsPerMinute)
	require.NotNil(t, rl["default"])
}
