package config

import (
	"encoding/base64"
	"github.com/open-rails/openrails/internal/db/models"
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

	t.Run("maps TEST_MODE to the top-level test_mode bool (#355)", func(t *testing.T) {
		t.Setenv("TEST_MODE", "true")

		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.True(t, cfg.TestMode)
		assert.True(t, cfg.IsTestMode())
	})

	t.Run("maps PROVIDER_WRITE_MODE to provider_write_mode", func(t *testing.T) {
		t.Setenv("PROVIDER_WRITE_MODE", "limited")

		cfg, err := Load("nonexistent-config.yaml")
		require.NoError(t, err)
		require.Equal(t, ProviderWriteModeLimited, cfg.GetProviderWriteMode())
		require.True(t, cfg.IsLimitedMode())
	})

	t.Run("defaults to sandbox in development when test_mode is unset (#355)", func(t *testing.T) {
		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.True(t, cfg.IsDev(), "default env is development")
		assert.True(t, cfg.TestMode, "unset test_mode defaults to sandbox in dev — a local boot can never silently use live credentials")
		assert.True(t, cfg.IsTestMode())
	})

	t.Run("explicit test_mode=false runs live in development (#355)", func(t *testing.T) {
		t.Setenv("TEST_MODE", "false")
		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.True(t, cfg.IsDev())
		assert.False(t, cfg.TestMode, "explicit false is honored over the dev sandbox default")
		assert.False(t, cfg.IsTestMode())
	})

	t.Run("rejects removed hot-path fail policy env vars", func(t *testing.T) {
		t.Setenv("OPENRAILS_BILLING_HOT_PATH_FAIL_POLICY", "fail_open")

		_, err := Load("nonexistent-config.yaml")
		assert.ErrorContains(t, err, "billing_hot_path was removed")
	})
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
	require.Equal(t, "http://localhost:3053", cfg.Auth.Issuer)
}

func TestLoad_ControlPlaneIssuerFromAPIURLInDev(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("env: development\napi_url: \"http://openrails:3053/\"\n"), 0o600))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.Equal(t, "http://openrails:3053", cfg.Auth.Issuer)
}

func TestLoad_AuthIssuerFromEnv(t *testing.T) {
	t.Setenv("AUTH_ISSUER", "https://billing.example.com/")

	cfg, err := Load("nonexistent-config.yaml")
	require.NoError(t, err)
	require.Equal(t, "https://billing.example.com/", cfg.Auth.Issuer)
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
	t.Run("defaults to live credentials when unset", func(t *testing.T) {
		assert.False(t, (&Config{}).IsTestMode())
	})

	t.Run("test_mode=true is sandbox", func(t *testing.T) {
		assert.True(t, (&Config{TestMode: true}).IsTestMode())
	})

	t.Run("orthogonal to mode", func(t *testing.T) {
		assert.False(t, (&Config{Mode: ModeFull}).IsTestMode())
		assert.False(t, (&Config{Mode: ModeLimited}).IsTestMode())
		assert.False(t, (&Config{Mode: ModeReadOnly}).IsTestMode())
		assert.True(t, (&Config{Mode: ModeFull, TestMode: true}).IsTestMode())
		assert.True(t, (&Config{Mode: ModeReadOnly, TestMode: true}).IsTestMode())
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

func TestProductionTestModeValidation(t *testing.T) {
	t.Run("prod env rejects test_mode=true", func(t *testing.T) {
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.ProviderWriteMode = ProviderWriteModeFull
		cfg.TestMode = true
		assembleDBURL(cfg)
		assert.False(t, cfg.IsDev(), "env=prod should not be dev")
		assert.ErrorContains(t, Validate(cfg), "test_mode=true is not allowed outside development")
	})

	t.Run("prod env requires an explicit provider_write_mode", func(t *testing.T) {
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.DB.Username = "billing_app"
		cfg.DB.Password = "production-db-password"
		cfg.ClickHouse.Username = "prod_analytics"
		cfg.ClickHouse.Password = "production-clickhouse-password"
		assembleDBURL(cfg)
		assert.ErrorContains(t, Validate(cfg), "provider_write_mode is required outside development")
	})

	t.Run("a zero-value struct is live + full behavior (Load applies the dev sandbox default separately)", func(t *testing.T) {
		// This builds the struct directly, bypassing Load(), so the zero-value
		// TestMode is live. The dev sandbox-by-default behavior is applied by
		// Load() and covered in TestLoad_EnvMapping (#355).
		cfg := GetDefaultBillingConfig()
		cfg.Env = "dev"
		assembleDBURL(cfg)
		assert.False(t, cfg.IsTestMode(), "zero-value test_mode is live; the dev sandbox default is a Load() concern")
		assert.False(t, cfg.IsLimitedMode(), "unset provider_write_mode in dev defaults to full behavior")
		assert.NoError(t, Validate(cfg))
	})

	t.Run("dev env accepts test_mode=true (sandbox)", func(t *testing.T) {
		cfg := GetDefaultBillingConfig()
		cfg.Env = "dev"
		cfg.TestMode = true
		assembleDBURL(cfg)
		assert.True(t, cfg.IsDev(), "env=dev should be dev")
		assert.True(t, cfg.IsTestMode())
		assert.NoError(t, Validate(cfg))
	})

	t.Run("prod env accepts provider_write_mode=full", func(t *testing.T) {
		cfg := GetDefaultBillingConfig()
		cfg.Env = "prod"
		cfg.ProviderWriteMode = ProviderWriteModeFull
		cfg.DB.Username = "billing_app"
		cfg.DB.Password = "production-db-password"
		cfg.ClickHouse.Username = "prod_analytics"
		cfg.ClickHouse.Password = "production-clickhouse-password"
		assembleDBURL(cfg)
		assert.NoError(t, Validate(cfg))
	})
}

// stripeTestModeConfig builds a minimal Config plus in-memory ProviderAccountSet with a
// single Stripe rail carrying the given secret key and test_mode setting.
func stripeTestModeConfig(secretKey string, testMode bool) (*Config, ProviderAccountSet) {
	return &Config{
			ProviderWriteMode: ProviderWriteModeFull,
			TestMode:          testMode,
		}, ProviderAccountSet{
			"stripe": {
				Rail:   models.RailStripe,
				Stripe: &StripeRailConfig{SecretKey: secretKey},
			},
		}
}

func TestValidateStripeKeyForTestMode(t *testing.T) {
	// Standard secret keys (sk_*)
	t.Run("sk_live_ + test_mode=true is a hard error (#347)", func(t *testing.T) {
		cfg, rails := stripeTestModeConfig("sk_live_abc123", true)
		assert.Error(t, validateStripeKeyForTestMode(cfg, rails), "live key in test env must refuse to boot")
	})

	t.Run("sk_test_ + test_mode=true allowed", func(t *testing.T) {
		cfg, rails := stripeTestModeConfig("sk_test_abc123", true)
		assert.NoError(t, validateStripeKeyForTestMode(cfg, rails))
		assert.Equal(t, "sk_test_abc123", rails["stripe"].Stripe.SecretKey, "test key in test env should be kept")
	})

	t.Run("sk_test_ + test_mode=false disables Stripe", func(t *testing.T) {
		cfg, rails := stripeTestModeConfig("sk_test_abc123", false)
		assert.NoError(t, validateStripeKeyForTestMode(cfg, rails))
		assert.Empty(t, rails["stripe"].Stripe.SecretKey, "test key in live env should be disabled")
	})

	t.Run("sk_live_ + test_mode=false allowed", func(t *testing.T) {
		cfg, rails := stripeTestModeConfig("sk_live_abc123", false)
		assert.NoError(t, validateStripeKeyForTestMode(cfg, rails))
		assert.Equal(t, "sk_live_abc123", rails["stripe"].Stripe.SecretKey, "live key in live env should be kept")
	})

	// Restricted keys (rk_*) — these must be classified the same as sk_* keys.
	t.Run("rk_live_ + test_mode=true is a hard error (#347)", func(t *testing.T) {
		cfg, rails := stripeTestModeConfig("rk_live_abc123", true)
		assert.Error(t, validateStripeKeyForTestMode(cfg, rails), "restricted live key in test env must refuse to boot")
	})

	t.Run("rk_test_ + test_mode=true allowed", func(t *testing.T) {
		cfg, rails := stripeTestModeConfig("rk_test_abc123", true)
		assert.NoError(t, validateStripeKeyForTestMode(cfg, rails))
		assert.Equal(t, "rk_test_abc123", rails["stripe"].Stripe.SecretKey, "restricted test key in test env should be kept")
	})

	t.Run("rk_test_ + test_mode=false disables Stripe", func(t *testing.T) {
		cfg, rails := stripeTestModeConfig("rk_test_abc123", false)
		assert.NoError(t, validateStripeKeyForTestMode(cfg, rails))
		assert.Empty(t, rails["stripe"].Stripe.SecretKey, "restricted test key in live env should be disabled")
	})

	t.Run("rk_live_ + test_mode=false allowed", func(t *testing.T) {
		cfg, rails := stripeTestModeConfig("rk_live_abc123", false)
		assert.NoError(t, validateStripeKeyForTestMode(cfg, rails))
		assert.Equal(t, "rk_live_abc123", rails["stripe"].Stripe.SecretKey, "restricted live key in live env should be kept")
	})

	t.Run("validates every stripe rail", func(t *testing.T) {
		cfg := &Config{ProviderWriteMode: ProviderWriteModeFull, TestMode: false}
		rails := ProviderAccountSet{
			"stripe_primary":  {Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_primary"}},
			"stripe_archived": {Rail: models.RailStripe, Archived: true, Stripe: &StripeRailConfig{SecretKey: "sk_test_legacy"}},
		}
		require.NoError(t, validateStripeKeyForTestMode(cfg, rails))
		require.Equal(t, "sk_live_primary", rails["stripe_primary"].Stripe.SecretKey)
		require.Empty(t, rails["stripe_archived"].Stripe.SecretKey)
	})
}

func TestActiveRailByType(t *testing.T) {
	rails := ProviderAccountSet{
		"stripe_old": {Rail: models.RailStripe, Archived: true, Stripe: &StripeRailConfig{SecretKey: "sk_live_old"}},
		"stripe_new": {Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_new"}},
		"mobius":     {Rail: models.RailNMI, NMI: &NMIRailConfig{SecurityKey: "sec"}},
	}
	key, proc, err := rails.ActiveRailByType(models.RailStripe)
	require.NoError(t, err)
	require.Equal(t, "stripe_new", key)
	require.Equal(t, "sk_live_new", proc.Stripe.SecretKey)
	require.Equal(t, proc, rails.GetStripeRail())

	rails["stripe_other"] = &ProviderAccountConfig{Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_other"}}
	key, proc, err = rails.ActiveRailByType(models.RailStripe)
	require.NoError(t, err)
	require.Equal(t, "stripe_new", key)
	require.Equal(t, "sk_live_new", proc.Stripe.SecretKey)
	require.NoError(t, ValidateRailSet(&Config{ProviderWriteMode: ProviderWriteModeFull}, ProviderAccountSet{
		"stripe_a": {Rail: models.RailStripe, AccountID: "acct_a", Stripe: &StripeRailConfig{SecretKey: "sk_live_a"}},
		"stripe_b": {Rail: models.RailStripe, AccountID: "acct_b", Stripe: &StripeRailConfig{SecretKey: "sk_live_b"}},
	}))

	// Two accounts on a rail without account_id is rejected: the made-up map name
	// can't be the provider identity (#641).
	require.ErrorContains(t, ValidateRailSet(&Config{ProviderWriteMode: ProviderWriteModeFull}, ProviderAccountSet{
		"stripe_a": {Rail: models.RailStripe, Archived: true, Stripe: &StripeRailConfig{SecretKey: "sk_live_a"}},
		"stripe_b": {Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_b"}},
	}), "must declare account_id")
}

// TestCatalogTargetSelectors covers active account selection plus
// FindByAccountID (resolve a specific account by gateway-id).
func TestCatalogTargetSelectors(t *testing.T) {
	rails := ProviderAccountSet{
		"mobius":   {Rail: models.RailNMI, AccountID: "100001", NMI: &NMIRailConfig{SecurityKey: "a"}},
		"paykings": {Rail: models.RailNMI, AccountID: "100002", NMI: &NMIRailConfig{SecurityKey: "b"}},
		"old-nmi":  {Rail: models.RailNMI, AccountID: "100003", Archived: true, NMI: &NMIRailConfig{SecurityKey: "c"}},
	}

	pk, primary, err := rails.ActiveRailByType(models.RailNMI)
	require.NoError(t, err)
	require.Equal(t, "mobius", pk)
	require.Equal(t, "100001", primary.AccountID)

	// Non-archived accounts are catalog-sync targets; archived is excluded.
	require.Equal(t, []string{"mobius", "paykings"}, rails.ActiveRailKeysByType(models.RailNMI))

	// FindByAccountID resolves by operator-declared gateway-id.
	got, ok := rails.FindByAccountID(models.RailNMI, "100002")
	require.True(t, ok)
	require.Equal(t, "b", got.NMI.SecurityKey)
	_, ok = rails.FindByAccountID(models.RailNMI, "999")
	require.False(t, ok)
}

// TestRailEnvironmentMustMatchTestMode covers the #641 "all-test or all-live"
// guard: an account whose declared environment contradicts test_mode is rejected;
// a matching or unset (derived) environment passes.
func TestRailEnvironmentMustMatchTestMode(t *testing.T) {
	// test_mode=true (sandbox) rejects a live-declared account.
	err := ValidateRailSet(&Config{ProviderWriteMode: ProviderWriteModeFull, TestMode: true}, ProviderAccountSet{
		"stripe": {Rail: models.RailStripe, Environment: "live", Stripe: &StripeRailConfig{SecretKey: "sk_test_x"}},
	})
	require.ErrorContains(t, err, "requires environment=test")

	// test_mode=false (production) rejects a test-declared account.
	cfgLive := &Config{ProviderWriteMode: ProviderWriteModeFull, TestMode: false}
	require.ErrorContains(t, ValidateRailSet(cfgLive, ProviderAccountSet{
		"stripe": {Rail: models.RailStripe, Environment: "test", Stripe: &StripeRailConfig{SecretKey: "sk_live_x"}},
	}), "requires environment=live")

	// A garbage environment is rejected.
	require.ErrorContains(t, ValidateRailSet(cfgLive, ProviderAccountSet{
		"stripe": {Rail: models.RailStripe, Environment: "staging", Stripe: &StripeRailConfig{SecretKey: "sk_live_x"}},
	}), "unknown environment")

	// Matching environment passes; unset (derived from test_mode) passes.
	require.NoError(t, ValidateRailSet(cfgLive, ProviderAccountSet{
		"stripe": {Rail: models.RailStripe, Environment: "live", Stripe: &StripeRailConfig{SecretKey: "sk_live_x"}},
	}))
	require.NoError(t, ValidateRailSet(cfgLive, ProviderAccountSet{
		"stripe": {Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_x"}},
	}))
}

func TestStripeLiveKeyRejectedInTestMode(t *testing.T) {
	cfg := GetDefaultBillingConfig()
	cfg.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
	cfg.TestMode = true
	rails := ProviderAccountSet{
		"stripe": {Rail: "stripe", Stripe: &StripeRailConfig{SecretKey: "sk_live_abc123"}},
	}
	require.Error(t, ValidateRailSet(cfg, rails))

	// test key in the test env is fine
	rails["stripe"].Stripe.SecretKey = "sk_test_abc123"
	require.NoError(t, ValidateRailSet(cfg, rails))

	// test key with live credentials expected: disabled with a warning, not fatal
	cfg2 := GetDefaultBillingConfig()
	cfg2.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
	cfg2.ProviderWriteMode = ProviderWriteModeLimited
	rails2 := ProviderAccountSet{
		"stripe": {
			Rail:   "stripe",
			Stripe: &StripeRailConfig{SecretKey: "sk_test_abc123"},
		},
	}
	require.NoError(t, ValidateRailSet(cfg2, rails2))
	require.Equal(t, "", rails2["stripe"].Stripe.SecretKey)
}

func TestRailConfigTypedBlocksAndArchived(t *testing.T) {
	cfg := &Config{}
	rails := ProviderAccountSet{
		"mobius": {
			Rail: models.RailNMI,
			NMI: &NMIRailConfig{
				SecurityKey:     "sec",
				TokenizationKey: "tok",
				WebhookSecret:   "wh",
			},
		},
		"stripe_old": {
			Rail:     models.RailStripe,
			Archived: true,
			Stripe:   &StripeRailConfig{SecretKey: "sk_live_old"},
		},
	}
	require.NoError(t, ValidateRailSet(cfg, rails))
	require.Equal(t, "sec", rails["mobius"].NMI.SecurityKey)
	require.Equal(t, "tok", rails["mobius"].NMI.TokenizationKey)
	require.Equal(t, "sk_live_old", rails["stripe_old"].Stripe.SecretKey)
	require.True(t, rails["stripe_old"].Archived)
}

func TestRailConfigRejectsWrongTypedBlock(t *testing.T) {
	err := ValidateRailSet(&Config{}, ProviderAccountSet{
		"stripe": {Rail: models.RailStripe, NMI: &NMIRailConfig{SecurityKey: "sec"}},
	})
	require.ErrorContains(t, err, "type stripe must use stripe block")
}

func TestSolanaRPCProviderValidation(t *testing.T) {
	cfg := GetDefaultBillingConfig()
	cfg.Env = "production"
	base := func(solana *SolanaRailConfig) ProviderAccountSet {
		return ProviderAccountSet{"solana": {Rail: models.RailSolana, Solana: solana}}
	}

	require.NoError(t, ValidateRailSet(cfg, base(&SolanaRailConfig{RPCProvider: "helius", RPCAPIKey: "key"})))
	require.NoError(t, ValidateRailSet(cfg, base(&SolanaRailConfig{RPCProvider: "", RPCAPIKey: "key"})))
	require.NoError(t, ValidateRailSet(cfg, base(&SolanaRailConfig{RPCProvider: "public"})))

	err := ValidateRailSet(cfg, base(&SolanaRailConfig{RPCProvider: "quicknode", RPCAPIKey: "key"}))
	require.ErrorContains(t, err, "rpc_provider must be helius or public")

	err = ValidateRailSet(cfg, base(&SolanaRailConfig{RPCProvider: "public", RPCAPIKey: "key"}))
	require.ErrorContains(t, err, "rpc_provider public cannot use rpc_api_key")
}

// TestWebhookSecretRequiredOutsideDev verifies that a missing webhook_secret is a
// hard boot error in production but only a warning in development.
func TestWebhookSecretRequiredOutsideDev(t *testing.T) {
	prodCfg := func() *Config {
		cfg := GetDefaultBillingConfig()
		cfg.Env = "production"
		return cfg
	}
	devCfg := func() *Config {
		return GetDefaultBillingConfig() // Env: "development" → isDev=true
	}

	tests := []struct {
		name      string
		cfg       *Config
		rail      *ProviderAccountConfig
		wantError bool
	}{
		// Stripe: missing webhook_secret
		{
			name:      "stripe/missing webhook_secret in dev → no error",
			cfg:       devCfg(),
			rail:      &ProviderAccountConfig{Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_test_dummy", WebhookSecret: ""}},
			wantError: false,
		},
		{
			name:      "stripe/missing webhook_secret in prod → error",
			cfg:       prodCfg(),
			rail:      &ProviderAccountConfig{Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_dummy", WebhookSecret: ""}},
			wantError: true,
		},
		{
			name:      "stripe/present webhook_secret in prod → no error",
			cfg:       prodCfg(),
			rail:      &ProviderAccountConfig{Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_dummy", WebhookSecret: "whsec_test_dummy"}},
			wantError: false,
		},
		// NMI: missing webhook_secret (security_key also required outside dev)
		{
			name:      "nmi/missing webhook_secret in dev → no error",
			cfg:       devCfg(),
			rail:      &ProviderAccountConfig{Rail: models.RailNMI, NMI: &NMIRailConfig{SecurityKey: "sec_dummy", WebhookSecret: ""}},
			wantError: false,
		},
		{
			name:      "nmi/missing webhook_secret in prod → error",
			cfg:       prodCfg(),
			rail:      &ProviderAccountConfig{Rail: models.RailNMI, NMI: &NMIRailConfig{SecurityKey: "sec_dummy", WebhookSecret: ""}},
			wantError: true,
		},
		{
			name:      "nmi/present webhook_secret in prod → no error",
			cfg:       prodCfg(),
			rail:      &ProviderAccountConfig{Rail: models.RailNMI, NMI: &NMIRailConfig{SecurityKey: "sec_dummy", WebhookSecret: "whsec_test_dummy"}},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRailSet(tt.cfg, ProviderAccountSet{"p": tt.rail})
			if tt.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
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
	// unset: nothing limited (dev default = full); test_mode defaults off
	require.False(t, (&Config{}).IsLimitedMode())
	require.False(t, (&Config{}).IsProviderReadOnly())
	require.False(t, (&Config{}).IsTestMode())

	require.False(t, (&Config{ProviderWriteMode: ProviderWriteModeFull}).IsLimitedMode())
	require.False(t, (&Config{ProviderWriteMode: ProviderWriteModeFull}).IsProviderReadOnly())

	limited := &Config{ProviderWriteMode: ProviderWriteModeLimited}
	require.True(t, limited.IsLimitedMode())
	require.False(t, limited.IsProviderReadOnly())

	ro := &Config{ProviderWriteMode: ProviderWriteModeReadOnly}
	require.True(t, ro.IsLimitedMode())
	require.True(t, ro.IsProviderReadOnly())

	// mode is pure behavior: test_mode does not change the behavior gates
	roSandbox := &Config{ProviderWriteMode: ProviderWriteModeReadOnly, TestMode: true}
	require.True(t, roSandbox.IsLimitedMode())
	require.True(t, roSandbox.IsProviderReadOnly())

	// case/space tolerant; unknown normalizes to "" pre-validation
	require.True(t, (&Config{ProviderWriteMode: " Limited "}).IsLimitedMode())
	require.False(t, (&Config{ProviderWriteMode: "redaonly"}).IsLimitedMode())
	require.Error(t, Validate(&Config{ProviderWriteMode: "redaonly"}))

	// hard cut (#355): the old mode values are gone, not aliased
	require.ErrorContains(t, Validate(&Config{ProviderWriteMode: "test"}), "must be one of full, limited, readonly")
	require.ErrorContains(t, Validate(&Config{ProviderWriteMode: "production"}), "must be one of full, limited, readonly")

	// legacy mode remains a compatibility alias while consumers migrate
	require.True(t, (&Config{Mode: ModeLimited}).IsLimitedMode())
	require.ErrorContains(t, Validate(&Config{ProviderWriteMode: ProviderWriteModeFull, Mode: ModeLimited}), "conflicts")
}

func TestFlexiblePortRange(t *testing.T) {
	// #349: int16 wrapped every kernel-ephemeral port (>=32768) negative.
	var p FlexiblePort
	require.NoError(t, p.UnmarshalText([]byte("44553")))
	require.Equal(t, FlexiblePort(44553), p)
	require.NoError(t, p.UnmarshalText([]byte("65535")))
	require.Equal(t, FlexiblePort(65535), p)
	require.NoError(t, p.UnmarshalText([]byte(" 3053 ")))
	require.Equal(t, FlexiblePort(3053), p)
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

func TestValidateEncryption(t *testing.T) {
	valid32Key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	valid16Key := base64.StdEncoding.EncodeToString(make([]byte, 16))

	tests := []struct {
		name      string
		cfg       *EncryptionConfig
		wantError string
	}{
		{
			name:      "nil config — encryption disabled, no error",
			cfg:       nil,
			wantError: "",
		},
		{
			name:      "empty MasterKey — encryption disabled, no error",
			cfg:       &EncryptionConfig{MasterKey: ""},
			wantError: "",
		},
		{
			name:      "whitespace-only MasterKey — treated as empty, no error",
			cfg:       &EncryptionConfig{MasterKey: "   "},
			wantError: "",
		},
		{
			name:      "valid 32-byte base64 key — no error",
			cfg:       &EncryptionConfig{MasterKey: valid32Key},
			wantError: "",
		},
		{
			name:      "non-base64 string — error",
			cfg:       &EncryptionConfig{MasterKey: "not!base64!"},
			wantError: "must be valid base64",
		},
		{
			name:      "valid base64 of wrong length (16 bytes) — error",
			cfg:       &EncryptionConfig{MasterKey: valid16Key},
			wantError: "must decode to 32 bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEncryption(tt.cfg)
			if tt.wantError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.ErrorContains(t, err, tt.wantError)
			}
		})
	}
}
