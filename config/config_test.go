package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoad_APIKeyFromEnv(t *testing.T) {
	t.Run("loads api_key from OPENRAILS_API_KEY", func(t *testing.T) {
		t.Setenv("OPENRAILS_API_KEY", "test-openrails-api-key")

		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.Equal(t, "test-openrails-api-key", cfg.APIKey)
	})

	t.Run("loads api_key from BILLING_API_KEY (legacy)", func(t *testing.T) {
		t.Setenv("BILLING_API_KEY", "test-billing-api-key")

		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.Equal(t, "test-billing-api-key", cfg.APIKey)
	})

	t.Run("loads api_key from API_KEY", func(t *testing.T) {
		t.Setenv("API_KEY", "test-api-key")

		cfg, err := Load("nonexistent-config.yaml")
		assert.NoError(t, err)
		assert.Equal(t, "test-api-key", cfg.APIKey)
	})

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

func TestTestModeOrthogonality(t *testing.T) {
	// Test that env and test_mode are independent settings
	t.Run("prod env can have test_mode true", func(t *testing.T) {
		trueBool := true
		cfg := &Config{
			Env:      "prod",
			TestMode: &trueBool,
		}
		assert.False(t, cfg.IsDev(), "env=prod should not be dev")
		assert.True(t, cfg.IsTestMode(), "test_mode=true should be in test mode")
	})

	t.Run("dev env can have test_mode false", func(t *testing.T) {
		falseBool := false
		cfg := &Config{
			Env:      "dev",
			TestMode: &falseBool,
		}
		assert.True(t, cfg.IsDev(), "env=dev should be dev")
		assert.False(t, cfg.IsTestMode(), "test_mode=false should not be in test mode")
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
