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

func TestLoad_ControlPlaneMaterializedWithDevIssuerDefault(t *testing.T) {
	// The control plane section is materialized even when omitted (#469); in
	// development the issuer defaults to the deployment's own base URL.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("env: development\n"), 0o600))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.NotNil(t, cfg.Auth)
	require.NotNil(t, cfg.Auth.ControlPlane)
	require.Equal(t, "http://localhost:2053", cfg.Auth.ControlPlane.Issuer)
	// Registration axes default closed.
	require.False(t, cfg.Auth.ControlPlane.UserRegistrationOpen())
	require.False(t, cfg.Auth.ControlPlane.TenantRegistrationOpen())
}

func TestLoad_ControlPlaneIssuerFromAPIURLInDev(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("env: development\napi_url: \"http://openrails:2053/\"\n"), 0o600))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.Equal(t, "http://openrails:2053", cfg.Auth.ControlPlane.Issuer)
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
		cfg.Auth.Issuers = []string{"https://issuer.example.com"}
		cfg.CorsOrigins = []string{"https://app.example.com"}
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
		cfg.Auth.Issuers = []string{"https://issuer.example.com"}
		cfg.CorsOrigins = []string{"https://app.example.com"}
		cfg.ClickHouse.Username = "prod_analytics"
		cfg.ClickHouse.Password = "production-clickhouse-password"
		assembleDBURL(cfg)
		assert.NoError(t, Validate(cfg))
	})

	t.Run("prod env rejects missing auth audience", func(t *testing.T) {
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.Mode = ModeFull
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
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.Mode = ModeFull
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

// stripeTestEnvConfig builds a minimal Config with a single Stripe processor
// carrying the given secret key and the given test_env setting (mode stays
// full — the behavior dial is orthogonal to the credential axis).
func stripeTestEnvConfig(secretKey string, testEnv bool) *Config {
	return &Config{
		Mode:    ModeFull,
		TestEnv: testEnv,
		Processors: map[string]*ProcessorConfig{
			"stripe": {
				Type:      string(ProcessorTypeStripe),
				SecretKey: secretKey,
			},
		},
	}
}

func TestValidateStripeKeyForTestEnv(t *testing.T) {
	// Standard secret keys (sk_*)
	t.Run("sk_live_ + test_env=true is a hard error (#347)", func(t *testing.T) {
		cfg := stripeTestEnvConfig("sk_live_abc123", true)
		assert.Error(t, validateStripeKeyForTestEnv(cfg), "live key in test env must refuse to boot")
	})

	t.Run("sk_test_ + test_env=true allowed", func(t *testing.T) {
		cfg := stripeTestEnvConfig("sk_test_abc123", true)
		assert.NoError(t, validateStripeKeyForTestEnv(cfg))
		assert.Equal(t, "sk_test_abc123", cfg.Processors["stripe"].SecretKey, "test key in test env should be kept")
	})

	t.Run("sk_test_ + test_env=false disables Stripe", func(t *testing.T) {
		cfg := stripeTestEnvConfig("sk_test_abc123", false)
		assert.NoError(t, validateStripeKeyForTestEnv(cfg))
		assert.Empty(t, cfg.Processors["stripe"].SecretKey, "test key in live env should be disabled")
	})

	t.Run("sk_live_ + test_env=false allowed", func(t *testing.T) {
		cfg := stripeTestEnvConfig("sk_live_abc123", false)
		assert.NoError(t, validateStripeKeyForTestEnv(cfg))
		assert.Equal(t, "sk_live_abc123", cfg.Processors["stripe"].SecretKey, "live key in live env should be kept")
	})

	// Restricted keys (rk_*) — these must be classified the same as sk_* keys.
	t.Run("rk_live_ + test_env=true is a hard error (#347)", func(t *testing.T) {
		cfg := stripeTestEnvConfig("rk_live_abc123", true)
		assert.Error(t, validateStripeKeyForTestEnv(cfg), "restricted live key in test env must refuse to boot")
	})

	t.Run("rk_test_ + test_env=true allowed", func(t *testing.T) {
		cfg := stripeTestEnvConfig("rk_test_abc123", true)
		assert.NoError(t, validateStripeKeyForTestEnv(cfg))
		assert.Equal(t, "rk_test_abc123", cfg.Processors["stripe"].SecretKey, "restricted test key in test env should be kept")
	})

	t.Run("rk_test_ + test_env=false disables Stripe", func(t *testing.T) {
		cfg := stripeTestEnvConfig("rk_test_abc123", false)
		assert.NoError(t, validateStripeKeyForTestEnv(cfg))
		assert.Empty(t, cfg.Processors["stripe"].SecretKey, "restricted test key in live env should be disabled")
	})

	t.Run("rk_live_ + test_env=false allowed", func(t *testing.T) {
		cfg := stripeTestEnvConfig("rk_live_abc123", false)
		assert.NoError(t, validateStripeKeyForTestEnv(cfg))
		assert.Equal(t, "rk_live_abc123", cfg.Processors["stripe"].SecretKey, "restricted live key in live env should be kept")
	})

	t.Run("validates every stripe processor", func(t *testing.T) {
		cfg := &Config{
			Mode:    ModeFull,
			TestEnv: false,
			Processors: map[string]*ProcessorConfig{
				"stripe_primary": {Type: ProcessorTypeStripe, SecretKey: "sk_live_primary"},
				"stripe_legacy":  {Type: ProcessorTypeStripe, Role: ProcessorRoleLegacy, SecretKey: "sk_test_legacy"},
			},
		}
		require.NoError(t, validateStripeKeyForTestEnv(cfg))
		require.Equal(t, "sk_live_primary", cfg.Processors["stripe_primary"].SecretKey)
		require.Empty(t, cfg.Processors["stripe_legacy"].SecretKey)
	})
}

func TestPrimaryProcessorByType(t *testing.T) {
	cfg := &Config{Processors: map[string]*ProcessorConfig{
		"stripe_old": {Type: ProcessorTypeStripe, Role: ProcessorRoleLegacy, SecretKey: "sk_live_old"},
		"stripe_new": {Type: ProcessorTypeStripe, SecretKey: "sk_live_new"},
		"mobius":     {Type: ProcessorTypeNMI, SecurityKey: "sec"},
	}}
	key, proc, err := cfg.PrimaryProcessorByType(ProcessorTypeStripe)
	require.NoError(t, err)
	require.Equal(t, "stripe_new", key)
	require.Equal(t, "sk_live_new", proc.SecretKey)
	require.Equal(t, proc, cfg.GetStripeProcessor())

	cfg.Processors["stripe_other"] = &ProcessorConfig{Type: ProcessorTypeStripe, SecretKey: "sk_live_other"}
	_, _, err = cfg.PrimaryProcessorByType(ProcessorTypeStripe)
	require.ErrorContains(t, err, "multiple primary processors")
	require.ErrorContains(t, Validate(&Config{
		Mode: ModeFull,
		DB:   &DBConfig{URL: "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"},
		Processors: map[string]*ProcessorConfig{
			"stripe_a": {Type: ProcessorTypeStripe, SecretKey: "sk_live_a"},
			"stripe_b": {Type: ProcessorTypeStripe, SecretKey: "sk_live_b"},
		},
	}), "multiple primary processors")
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

func TestAllowedCORSOrigins_UnionsGlobalAndMerchantOrigins(t *testing.T) {
	cfg := &Config{
		CorsOrigins: []string{"https://admin.example.com", "https://admin.example.com"}, // dup
		MerchantCORS: map[string]*MerchantCORSConfig{
			"merchant-a": {AllowedOrigins: []string{"https://media.example.com", "https://admin.example.com"}}, // dup w/ global
			"merchant-b": {AllowedOrigins: []string{"https://app.example.com", "https://portal.example.com"}},
			"empty":      nil,
		},
	}

	got := cfg.AllowedCORSOrigins()

	// Global origin first, de-duplicated; merchant origins follow in slug order.
	assert.Equal(t, []string{
		"https://admin.example.com",
		"https://media.example.com",
		"https://app.example.com",
		"https://portal.example.com",
	}, got)
}

func TestAllowedCORSOrigins_NoWildcardWhenConfigured(t *testing.T) {
	cfg := &Config{MerchantCORS: map[string]*MerchantCORSConfig{
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

func TestFeatureFlagsProcessorSubscriptionDeletionKillSwitch(t *testing.T) {
	var nilFlags *FeatureFlags
	require.False(t, nilFlags.IsProcessorSubscriptionDeletionDisabled())
	require.False(t, (&FeatureFlags{}).IsProcessorSubscriptionDeletionDisabled())
	require.True(t, (&FeatureFlags{DisableProcessorSubscriptionDeletions: true}).IsProcessorSubscriptionDeletionDisabled())

	cfg := &Config{FeatureFlags: &FeatureFlags{DisableProcessorSubscriptionDeletions: true}}
	require.True(t, cfg.IsProcessorSubscriptionDeletionDisabled())
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
	require.False(t, limited.IsProcessorSubscriptionDeletionDisabled())

	ro := &Config{Mode: ModeReadOnly}
	require.True(t, ro.IsLimitedMode())
	require.True(t, ro.IsProviderReadOnly())
	require.True(t, ro.IsProcessorSubscriptionDeletionDisabled()) // readonly implies the kill switch

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

func TestStripeLiveKeyRejectedInTestEnv(t *testing.T) {
	cfg := GetDefaultBillingConfig()
	cfg.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
	cfg.TestEnv = true
	cfg.Processors = map[string]*ProcessorConfig{
		"stripe": {Type: "stripe", SecretKey: "sk_live_abc123"},
	}
	require.Error(t, Validate(cfg))

	// test key in the test env is fine
	cfg.Processors["stripe"].SecretKey = "sk_test_abc123"
	require.NoError(t, Validate(cfg))

	// test key with live credentials expected: disabled with a warning, not fatal
	cfg2 := GetDefaultBillingConfig()
	cfg2.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
	cfg2.Mode = ModeLimited
	cfg2.Processors = map[string]*ProcessorConfig{
		"stripe": {Type: "stripe", SecretKey: "sk_test_abc123"},
	}
	require.NoError(t, Validate(cfg2))
	require.Equal(t, "", cfg2.Processors["stripe"].SecretKey)
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
