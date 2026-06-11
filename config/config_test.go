package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

	t.Run("loads JSON arrays for slices (auth.issuers)", func(t *testing.T) {
		t.Setenv("AUTH_ISSUERS", `["http://a.test","http://b.test"]`)

		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.Equal(t, []string{"http://a.test", "http://b.test"}, cfg.Auth.Issuers)
	})

	t.Run("maps canonical Vault env vars to vault.* (#251)", func(t *testing.T) {
		t.Setenv("VAULT_ENABLED", "true")
		t.Setenv("VAULT_ADDR", "http://127.0.0.1:8200")
		t.Setenv("VAULT_TOKEN", "root")
		t.Setenv("VAULT_AUTH_METHOD", "token")
		t.Setenv("VAULT_KV_MOUNT", "secret")

		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		if assert.NotNil(t, cfg.Vault) {
			assert.True(t, cfg.Vault.Enabled)
			assert.Equal(t, "http://127.0.0.1:8200", cfg.Vault.Address)
			assert.Equal(t, "root", cfg.Vault.Token)
			assert.Equal(t, "token", cfg.Vault.AuthMethod)
			assert.Equal(t, "secret", cfg.Vault.KVMount)
		}
	})

	t.Run("maps canonical OpenRails hot-path env vars", func(t *testing.T) {
		t.Setenv("OPENRAILS_BILLING_HOT_PATH_FAIL_POLICY", "fail_open")

		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.Equal(t, "fail_open", cfg.BillingHotPath.FailPolicy)
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

func TestLoad_RequiresExplicitTypeForCustomProcessors(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(cfgPath, []byte(`
processors:
  acme:
    security_key: test-key
`), 0o600)
	assert.NoError(t, err)

	_, err = Load(cfgPath)
	assert.Error(t, err)
	assert.ErrorContains(t, err, "processor 'acme' must declare a type")
}

func TestLoad_MobiusProcessorRequiresExplicitType(t *testing.T) {
	t.Setenv("PROCESSORS_MOBIUS_TYPE", "")
	t.Setenv("PROCESSORS_MOBIUS_TOKENIZATION_KEY", "")
	t.Setenv("PROCESSORS_MOBIUS_TOKENIZATION_URL", "")
	t.Setenv("PROCESSORS_MOBIUS_WEBHOOK_SECRET", "")
	t.Setenv("PROCESSORS_MOBIUS_SECURITY_KEY", "test-key")

	_, err := Load("nonexistent-config.yaml")
	assert.Error(t, err)
	assert.ErrorContains(t, err, "processor 'mobius' must declare a type")
}

func TestValidateCaptchaRequiresKeysWhenEnabled(t *testing.T) {
	cfg := GetDefaultBillingConfig()
	cfg.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
	cfg.Captcha.Enabled = true
	cfg.Captcha.Provider = CaptchaProviderTurnstile
	cfg.Captcha.SiteKey = ""
	cfg.Captcha.SecretKey = "secret-key"

	err := Validate(cfg)
	assert.Error(t, err)
	assert.ErrorContains(t, err, "site_key is required")

	cfg.Captcha.SiteKey = "site-key"
	err = Validate(cfg)
	assert.NoError(t, err)
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
		{
			name:      "invalid verify url",
			mutate:    func(c *CaptchaConfig) { c.VerifyURL = "://bad" },
			wantError: "invalid verify_url",
		},
		{
			name:      "unsupported verify url scheme",
			mutate:    func(c *CaptchaConfig) { c.VerifyURL = "ftp://captcha.example/siteverify" },
			wantError: "verify_url must use https",
		},
		{
			name:      "insecure verify url",
			mutate:    func(c *CaptchaConfig) { c.VerifyURL = "http://captcha.example/siteverify" },
			wantError: "verify_url must use https",
		},
		{
			name:      "invalid challenge ttl",
			mutate:    func(c *CaptchaConfig) { c.ChallengeTTL = "soon" },
			wantError: "invalid challenge_ttl",
		},
		{
			name:      "insecure script url",
			mutate:    func(c *CaptchaConfig) { c.ScriptURL = "http://captcha.example/api.js" },
			wantError: "script_url must use https",
		},
		{
			name:      "invalid score",
			mutate:    func(c *CaptchaConfig) { c.MinScore = 1.5 },
			wantError: "min_score must be between 0 and 1",
		},
		{
			name:      "empty buckets",
			mutate:    func(c *CaptchaConfig) { c.ChallengeBuckets = []string{" ", ""} },
			wantError: "challenge_buckets must include at least one non-empty bucket",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := GetDefaultBillingConfig()
			cfg.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
			cfg.Captcha.Enabled = true
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

func TestIsTestMode(t *testing.T) {
	t.Run("defaults to true when nil", func(t *testing.T) {
		cfg := &Config{TestMode: nil}
		assert.True(t, cfg.IsTestMode(), "should default to test mode when not explicitly set")
	})

	t.Run("returns true when explicitly true", func(t *testing.T) {
		trueBool := true
		cfg := &Config{TestMode: &trueBool}
		assert.True(t, cfg.IsTestMode())
	})

	t.Run("returns false when explicitly false", func(t *testing.T) {
		falseBool := false
		cfg := &Config{TestMode: &falseBool}
		assert.False(t, cfg.IsTestMode())
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

func TestProductionTestModeValidation(t *testing.T) {
	t.Run("prod env rejects test_mode true", func(t *testing.T) {
		trueBool := true
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.TestMode = &trueBool
		assembleDBURL(cfg)
		assert.False(t, cfg.IsDev(), "env=prod should not be dev")
		assert.ErrorContains(t, Validate(cfg), "test_mode=true is not allowed outside development")
	})

	t.Run("prod env treats unset test_mode as live", func(t *testing.T) {
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.TestMode = nil
		cfg.DB.Username = "billing_app"
		cfg.DB.Password = "production-db-password"
		cfg.Auth.Issuers = []string{"https://issuer.example.com"}
		cfg.CorsOrigins = []string{"https://app.example.com"}
		cfg.ClickHouse.Username = "prod_analytics"
		cfg.ClickHouse.Password = "production-clickhouse-password"
		assembleDBURL(cfg)
		assert.False(t, cfg.IsTestMode(), "unset test_mode in prod should be live")
		assert.NoError(t, Validate(cfg))
	})

	t.Run("dev env treats unset test_mode as sandbox", func(t *testing.T) {
		cfg := GetDefaultBillingConfig()
		cfg.Env = "dev"
		cfg.TestMode = nil
		assembleDBURL(cfg)
		assert.True(t, cfg.IsTestMode(), "unset test_mode in dev should be sandbox")
		assert.NoError(t, Validate(cfg))
	})

	t.Run("dev env can have test_mode false", func(t *testing.T) {
		falseBool := false
		cfg := GetDefaultBillingConfig()
		cfg.Env = "dev"
		cfg.TestMode = &falseBool
		assembleDBURL(cfg)
		assert.True(t, cfg.IsDev(), "env=dev should be dev")
		assert.False(t, cfg.IsTestMode(), "test_mode=false should not be in test mode")
		assert.NoError(t, Validate(cfg))
	})

	t.Run("prod env accepts explicit test_mode false", func(t *testing.T) {
		falseBool := false
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.TestMode = &falseBool
		cfg.DB.Username = "billing_app"
		cfg.DB.Password = "production-db-password"
		cfg.Auth.Issuers = []string{"https://issuer.example.com"}
		cfg.CorsOrigins = []string{"https://app.example.com"}
		cfg.ClickHouse.Username = "prod_analytics"
		cfg.ClickHouse.Password = "production-clickhouse-password"
		assembleDBURL(cfg)
		assert.NoError(t, Validate(cfg))
	})

	t.Run("prod env rejects missing auth audience", func(t *testing.T) {
		falseBool := false
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.TestMode = &falseBool
		cfg.DB.Username = "billing_app"
		cfg.DB.Password = "production-db-password"
		cfg.Auth.Issuers = []string{"https://issuer.example.com"}
		cfg.Auth.ExpectedAudience = ""
		cfg.CorsOrigins = []string{"https://app.example.com"}
		cfg.ClickHouse.Username = "prod_analytics"
		cfg.ClickHouse.Password = "production-clickhouse-password"
		assembleDBURL(cfg)
		assert.ErrorContains(t, Validate(cfg), "auth expected_audience must be configured outside development")
	})

	t.Run("prod env rejects missing cors origins", func(t *testing.T) {
		falseBool := false
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.TestMode = &falseBool
		cfg.DB.Username = "billing_app"
		cfg.DB.Password = "production-db-password"
		cfg.Auth.Issuers = []string{"https://issuer.example.com"}
		cfg.CorsOrigins = nil
		cfg.ClickHouse.Username = "prod_analytics"
		cfg.ClickHouse.Password = "production-clickhouse-password"
		assembleDBURL(cfg)
		assert.ErrorContains(t, Validate(cfg), "cors_origins must be configured outside development")
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

// stripeModeTestConfig builds a minimal Config with a single Stripe processor
// carrying the given secret key and the given test_mode setting.
func stripeModeTestConfig(secretKey string, testMode bool) *Config {
	tm := testMode
	return &Config{
		TestMode: &tm,
		Processors: map[string]*ProcessorConfig{
			"stripe": {
				Type:      string(ProcessorTypeStripe),
				SecretKey: secretKey,
			},
		},
	}
}

func TestValidateStripeKeyForTestMode(t *testing.T) {
	// Standard secret keys (sk_*)
	t.Run("sk_live_ + test_mode=true disables Stripe", func(t *testing.T) {
		cfg := stripeModeTestConfig("sk_live_abc123", true)
		validateStripeKeyForTestMode(cfg)
		assert.Empty(t, cfg.Processors["stripe"].SecretKey, "live key in test mode should be disabled")
	})

	t.Run("sk_test_ + test_mode=true allowed", func(t *testing.T) {
		cfg := stripeModeTestConfig("sk_test_abc123", true)
		validateStripeKeyForTestMode(cfg)
		assert.Equal(t, "sk_test_abc123", cfg.Processors["stripe"].SecretKey, "test key in test mode should be kept")
	})

	t.Run("sk_test_ + test_mode=false disables Stripe", func(t *testing.T) {
		cfg := stripeModeTestConfig("sk_test_abc123", false)
		validateStripeKeyForTestMode(cfg)
		assert.Empty(t, cfg.Processors["stripe"].SecretKey, "test key in live mode should be disabled")
	})

	t.Run("sk_live_ + test_mode=false allowed", func(t *testing.T) {
		cfg := stripeModeTestConfig("sk_live_abc123", false)
		validateStripeKeyForTestMode(cfg)
		assert.Equal(t, "sk_live_abc123", cfg.Processors["stripe"].SecretKey, "live key in live mode should be kept")
	})

	// Restricted keys (rk_*) — these must be classified the same as sk_* keys.
	t.Run("rk_live_ + test_mode=true disables Stripe", func(t *testing.T) {
		cfg := stripeModeTestConfig("rk_live_abc123", true)
		validateStripeKeyForTestMode(cfg)
		assert.Empty(t, cfg.Processors["stripe"].SecretKey, "restricted live key in test mode should be disabled")
	})

	t.Run("rk_test_ + test_mode=true allowed", func(t *testing.T) {
		cfg := stripeModeTestConfig("rk_test_abc123", true)
		validateStripeKeyForTestMode(cfg)
		assert.Equal(t, "rk_test_abc123", cfg.Processors["stripe"].SecretKey, "restricted test key in test mode should be kept")
	})

	t.Run("rk_test_ + test_mode=false disables Stripe", func(t *testing.T) {
		cfg := stripeModeTestConfig("rk_test_abc123", false)
		validateStripeKeyForTestMode(cfg)
		assert.Empty(t, cfg.Processors["stripe"].SecretKey, "restricted test key in live mode should be disabled")
	})

	t.Run("rk_live_ + test_mode=false allowed", func(t *testing.T) {
		cfg := stripeModeTestConfig("rk_live_abc123", false)
		validateStripeKeyForTestMode(cfg)
		assert.Equal(t, "rk_live_abc123", cfg.Processors["stripe"].SecretKey, "restricted live key in live mode should be kept")
	})
}

// =============================================================================
// Feature Flags Tests
// =============================================================================

func TestFeatureFlags_DunningMode(t *testing.T) {
	t.Run("defaults to 'on' when nil", func(t *testing.T) {
		var flags *FeatureFlags
		assert.Equal(t, DunningModeOn, flags.GetDunningMode())
	})

	t.Run("defaults to 'on' when empty", func(t *testing.T) {
		flags := &FeatureFlags{DunningMode: ""}
		assert.Equal(t, DunningModeOn, flags.GetDunningMode())
	})

	t.Run("returns 'on' for explicit 'on'", func(t *testing.T) {
		flags := &FeatureFlags{DunningMode: "on"}
		assert.Equal(t, DunningModeOn, flags.GetDunningMode())
		assert.True(t, flags.IsDunningEnabled())
		assert.False(t, flags.IsDunningDryRun())
		assert.False(t, flags.IsDunningOff())
	})

	t.Run("returns 'dry_run_only' for explicit 'dry_run_only'", func(t *testing.T) {
		flags := &FeatureFlags{DunningMode: "dry_run_only"}
		assert.Equal(t, DunningModeDryRunOnly, flags.GetDunningMode())
		assert.False(t, flags.IsDunningEnabled())
		assert.True(t, flags.IsDunningDryRun())
		assert.False(t, flags.IsDunningOff())
	})

	t.Run("returns 'off' for explicit 'off'", func(t *testing.T) {
		flags := &FeatureFlags{DunningMode: "off"}
		assert.Equal(t, DunningModeOff, flags.GetDunningMode())
		assert.False(t, flags.IsDunningEnabled())
		assert.False(t, flags.IsDunningDryRun())
		assert.True(t, flags.IsDunningOff())
	})

	t.Run("invalid mode defaults to 'on'", func(t *testing.T) {
		flags := &FeatureFlags{DunningMode: "invalid_mode"}
		assert.Equal(t, DunningModeOn, flags.GetDunningMode())
	})

	t.Run("handles case insensitivity", func(t *testing.T) {
		flags := &FeatureFlags{DunningMode: "DRY_RUN_ONLY"}
		assert.Equal(t, DunningModeDryRunOnly, flags.GetDunningMode())
	})

	t.Run("trims whitespace", func(t *testing.T) {
		flags := &FeatureFlags{DunningMode: "  off  "}
		assert.Equal(t, DunningModeOff, flags.GetDunningMode())
	})
}

func TestFeatureFlags_DisableEntitlementExpiration(t *testing.T) {
	t.Run("defaults to false when nil", func(t *testing.T) {
		cfg := &Config{FeatureFlags: nil}
		assert.False(t, cfg.IsEntitlementExpirationDisabled())
	})

	t.Run("returns false when explicitly false", func(t *testing.T) {
		cfg := &Config{
			FeatureFlags: &FeatureFlags{DisableEntitlementExpiration: false},
		}
		assert.False(t, cfg.IsEntitlementExpirationDisabled())
	})

	t.Run("returns true when explicitly true", func(t *testing.T) {
		cfg := &Config{
			FeatureFlags: &FeatureFlags{DisableEntitlementExpiration: true},
		}
		assert.True(t, cfg.IsEntitlementExpirationDisabled())
	})
}

func TestConfig_DunningMode(t *testing.T) {
	t.Run("GetDunningMode defaults to 'on' when no feature flags", func(t *testing.T) {
		cfg := &Config{FeatureFlags: nil}
		assert.Equal(t, DunningModeOn, cfg.GetDunningMode())
	})

	t.Run("IsDunningEnabled returns true by default", func(t *testing.T) {
		cfg := &Config{FeatureFlags: nil}
		assert.True(t, cfg.IsDunningEnabled())
	})

	t.Run("IsDunningOff returns true when mode is off", func(t *testing.T) {
		cfg := &Config{
			FeatureFlags: &FeatureFlags{DunningMode: "off"},
		}
		assert.True(t, cfg.IsDunningOff())
		assert.False(t, cfg.IsDunningEnabled())
	})

	t.Run("IsDunningDryRun returns true when mode is dry_run_only", func(t *testing.T) {
		cfg := &Config{
			FeatureFlags: &FeatureFlags{DunningMode: "dry_run_only"},
		}
		assert.True(t, cfg.IsDunningDryRun())
		assert.False(t, cfg.IsDunningEnabled())
	})
}

func TestGetDefaultBillingConfig_FeatureFlags(t *testing.T) {
	cfg := GetDefaultBillingConfig()

	t.Run("has feature flags initialized", func(t *testing.T) {
		assert.NotNil(t, cfg.FeatureFlags)
	})

	t.Run("dunning_mode defaults to 'on'", func(t *testing.T) {
		assert.Equal(t, DunningModeOn, cfg.FeatureFlags.DunningMode)
		assert.True(t, cfg.IsDunningEnabled())
	})

	t.Run("disable_entitlement_expiration defaults to false", func(t *testing.T) {
		assert.False(t, cfg.FeatureFlags.DisableEntitlementExpiration)
		assert.False(t, cfg.IsEntitlementExpirationDisabled())
	})
}

func TestAllowedCORSOrigins_UnionsGlobalAndTenantOrigins(t *testing.T) {
	cfg := &Config{
		CorsOrigins: []string{"https://admin.example.com", "https://admin.example.com"}, // dup
		TenantCORS: map[string]*TenantCORSConfig{
			"tenant-a": {AllowedOrigins: []string{"https://media.example.com", "https://admin.example.com"}}, // dup w/ global
			"tenant-b": {AllowedOrigins: []string{"https://app.example.com", "https://portal.example.com"}},
			"empty":    nil,
		},
	}

	got := cfg.AllowedCORSOrigins()

	// Global origin first, de-duplicated; tenant origins follow in slug order.
	assert.Equal(t, []string{
		"https://admin.example.com",
		"https://media.example.com",
		"https://app.example.com",
		"https://portal.example.com",
	}, got)
}

func TestAllowedCORSOrigins_NoWildcardWhenConfigured(t *testing.T) {
	cfg := &Config{TenantCORS: map[string]*TenantCORSConfig{
		"t": {AllowedOrigins: []string{"https://t.example.com"}},
	}}
	got := cfg.AllowedCORSOrigins()
	assert.Equal(t, []string{"https://t.example.com"}, got)
	assert.NotContains(t, got, "*")
}

func TestAllowedCORSOrigins_NilSafe(t *testing.T) {
	var cfg *Config
	assert.Nil(t, cfg.AllowedCORSOrigins())
	assert.Empty(t, (&Config{}).AllowedCORSOrigins())
}

// TestDBConfig_SchemaName covers the issue #165 schema accessor: default `billing`,
// normalization (trim + lower-case), and nil-safety.
func TestDBConfig_SchemaName(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty defaults to billing", "", "billing"},
		{"explicit value preserved", "host_billing", "host_billing"},
		{"trimmed", "  custom  ", "custom"},
		{"lower-cased", "Billing", "billing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, (&DBConfig{Schema: tc.raw}).SchemaName())
		})
	}

	t.Run("nil receiver defaults to billing", func(t *testing.T) {
		var c *DBConfig
		assert.Equal(t, "billing", c.SchemaName())
	})

	// The exported default constant is the historical schema.
	assert.Equal(t, "billing", DefaultSchema)
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
// load, is normalized, and (default) resolves to billing. A custom value is honored.
func TestLoad_DBSchemaEnv(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.Equal(t, "billing", cfg.DB.SchemaName())
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

func TestFeatureFlagsDunningWindow(t *testing.T) {
	// nil flags and zero value fall back to the default window
	var nilFlags *FeatureFlags
	require.Equal(t, DefaultDunningWindowDays*24*time.Hour, nilFlags.GetDunningWindow())
	require.Equal(t, DefaultDunningWindowDays*24*time.Hour, (&FeatureFlags{}).GetDunningWindow())
	require.Equal(t, DefaultDunningWindowDays*24*time.Hour, (&FeatureFlags{DunningWindowDays: -3}).GetDunningWindow())

	require.Equal(t, 30*24*time.Hour, (&FeatureFlags{DunningWindowDays: 30}).GetDunningWindow())

	// Config wrapper is nil-safe via GetFeatureFlags
	cfg := &Config{}
	require.Equal(t, DefaultDunningWindowDays*24*time.Hour, cfg.GetDunningWindow())
}

func TestFeatureFlagsProcessorSubscriptionDeletionKillSwitch(t *testing.T) {
	var nilFlags *FeatureFlags
	require.False(t, nilFlags.IsProcessorSubscriptionDeletionDisabled())
	require.False(t, (&FeatureFlags{}).IsProcessorSubscriptionDeletionDisabled())
	require.True(t, (&FeatureFlags{DisableProcessorSubscriptionDeletions: true}).IsProcessorSubscriptionDeletionDisabled())

	cfg := &Config{FeatureFlags: &FeatureFlags{DisableProcessorSubscriptionDeletions: true}}
	require.True(t, cfg.IsProcessorSubscriptionDeletionDisabled())
}
