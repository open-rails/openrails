package config

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func validationConfig(env string) *Config {
	cfg := GetDefaultBillingConfig()
	cfg.TestMode = CredentialPostureSandbox
	cfg.ProviderWriteMode = ProviderWriteModeFull
	cfg.DB.Username, cfg.DB.Password = "billing_app", "database-password"
	assembleDBURL(cfg)
	return cfg
}

func TestConfigurationPostureValidation(t *testing.T) {
	for _, env := range []string{"", "dev", "development", "prod", "production", "DEV"} {
		cfg := &Config{}
		require.True(t, cfg.RequiresSecretEncryption(), env)
		if env == "" {
			require.True(t, cfg.RequiresSecretEncryption())
		}
	}
	for _, posture := range []CredentialPosture{"", CredentialPostureLive, CredentialPostureSandbox} {
		for _, row := range []struct {
			mode              string
			limited, readonly bool
		}{
			{"", true, true}, {ProviderWriteModeFull, false, false}, {ProviderWriteModeLimited, true, false},
			{ProviderWriteModeReadOnly, true, true}, {" Limited ", true, false}, {"redaonly", true, true},
		} {
			cfg := &Config{TestMode: posture, ProviderWriteMode: row.mode}
			require.Equal(t, posture == CredentialPostureSandbox, cfg.IsTestMode())
			require.Equal(t, row.limited, cfg.IsLimitedMode())
			require.Equal(t, row.readonly, cfg.IsProviderReadOnly())
		}
	}
	for _, mode := range []string{"redaonly", "test", "production"} {
		require.ErrorContains(t, Validate(&Config{ProviderWriteMode: mode}), "must be one of full, limited, readonly")
	}
	for _, row := range []struct {
		name, env string
		edit      func(*Config)
		message   string
	}{
		{"production sandbox", "prod", func(c *Config) { c.TestMode = CredentialPostureSandbox }, ""},
		{"staging sandbox", "staging", func(c *Config) {
			c.TestMode = CredentialPostureSandbox
		}, ""},
		{"production full", "prod", func(c *Config) {}, ""},
		{"dev sandbox", "dev", func(c *Config) { c.TestMode = CredentialPostureSandbox }, ""},
		{"missing write posture stays read-only", "prod", func(c *Config) { c.ProviderWriteMode = "" }, ""},
		{"invalid credential posture", "dev", func(c *Config) { c.TestMode = "yes" }, `invalid test_mode "yes"`},
		{"required rate limits", "prod", func(c *Config) { c.RateLimits = nil }, "rate_limits is required"},
		{"explicit rate-limit opt out", "prod", func(c *Config) { c.RateLimits = nil; c.RateLimitsDisabled = true }, ""},
		{"required rates regardless of posture", "dev", func(c *Config) { c.RateLimits = nil }, "rate_limits is required"},
		{"captcha half pair", "dev", func(c *Config) { c.Captcha = &CaptchaConfig{Provider: CaptchaProviderTurnstile, SecretKey: "secret"} }, "BOTH site_key and secret_key"},
		{"captcha unsupported", "dev", func(c *Config) {
			c.Captcha = &CaptchaConfig{Provider: "recaptcha", SiteKey: "site", SecretKey: "secret"}
		}, "unsupported provider"},
	} {
		t.Run(row.name, func(t *testing.T) {
			cfg := validationConfig(row.env)
			row.edit(cfg)
			err := Validate(cfg)
			if row.message == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, row.message)
			}
		})
	}
	cfg := validationConfig("dev")
	cfg.ProviderWriteMode = ""
	cfg.TestMode = ""
	require.False(t, cfg.IsTestMode())
	require.True(t, cfg.IsProviderReadOnly())
	require.ErrorContains(t, Validate(cfg), "test_mode is required")
	cfg.TestMode = CredentialPostureSandbox
	require.NoError(t, Validate(cfg))
	require.True(t, cfg.IsProviderReadOnly(), "omitted write mode never enables provider writes")
	cfg.ProviderWriteMode = ProviderWriteModeFull
	for _, enabled := range []bool{false, true} {
		cfg.Captcha = &CaptchaConfig{Provider: CaptchaProviderTurnstile}
		if enabled {
			cfg.Captcha.SiteKey, cfg.Captcha.SecretKey = "site", "secret"
		}
		require.NoError(t, Validate(cfg))
		require.Equal(t, enabled, cfg.Captcha.IsEnabled())
	}
}

func TestConfigurationStorageAndSecrets(t *testing.T) {
	var nilCfg *Config
	require.Equal(t, SecretBackendSnapshot, nilCfg.SecretStoreBackend())
	for _, backend := range []string{"", SecretBackendSnapshot, SecretBackendVault} {
		cfg := validationConfig("")
		cfg.SecretBackend = backend
		for _, published := range []bool{false, true} {
			cfg.MerchantConfigHTTP = published
			require.NoError(t, Validate(cfg))
		}
	}
	for _, posture := range []CredentialPosture{CredentialPostureSandbox, CredentialPostureLive} {
		cfg := validationConfig("")
		cfg.TestMode, cfg.SecretBackend = posture, SecretBackendDB
		require.ErrorContains(t, Validate(cfg), "encryption.master_key")
		cfg.Encryption = &EncryptionConfig{MasterKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}
		require.NoError(t, Validate(cfg))
	}
	require.Error(t, validateSecretBackend(&Config{SecretBackend: "consul"}))
	for _, row := range []struct {
		cfg     *EncryptionConfig
		message string
	}{
		{nil, ""}, {&EncryptionConfig{}, ""}, {&EncryptionConfig{MasterKey: "   "}, ""},
		{&EncryptionConfig{MasterKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}, ""},
		{&EncryptionConfig{MasterKey: "not!base64!"}, "must be valid base64"},
		{&EncryptionConfig{MasterKey: base64.StdEncoding.EncodeToString(make([]byte, 16))}, "must decode to 32 bytes"},
	} {
		err := validateEncryption(row.cfg)
		if row.message == "" {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, row.message)
		}
	}
}

func TestConfigurationScalarBoundaries(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": 24 * time.Hour, "36h": 36 * time.Hour} {
		got, err := (&Config{ProviderBillingQuiescenceInterval: raw}).ProviderBillingQuiescence()
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for _, raw := range []string{"0", "500ms", "1500ms", "-1h", "tomorrow"} {
		_, err := (&Config{ProviderBillingQuiescenceInterval: raw}).ProviderBillingQuiescence()
		require.Error(t, err)
	}
	for raw, want := range map[string]string{"": "billing", "host_billing": "host_billing", "  custom  ": "custom", "Openrails": "openrails"} {
		require.Equal(t, want, (&DBConfig{Schema: raw}).SchemaName())
	}
	var nilDB *DBConfig
	require.Equal(t, "billing", nilDB.SchemaName())
	require.Equal(t, "billing", DefaultSchema)
	require.Equal(t, "openrails", CanonicalSchema)
	for _, raw := range []string{"", "billing", "host_billing", "_x", "Schema1"} {
		require.NoError(t, validateSchema(raw))
	}
	for _, raw := range []string{"1schema", "bad schema", "bad-schema", "bill;drop", `"quoted"`, "a.b"} {
		require.Error(t, validateSchema(raw))
	}
	for raw, want := range map[string]FlexiblePort{"44553": 44553, "65535": 65535, " 3053 ": 3053, "": 0} {
		var got FlexiblePort
		require.NoError(t, got.UnmarshalText([]byte(raw)))
		require.Equal(t, want, got)
	}
	for _, raw := range []string{"65536", "0", "-1", "not-a-port"} {
		var got FlexiblePort
		require.Error(t, got.UnmarshalText([]byte(raw)))
	}
	cfg := validationConfig("dev")
	cfg.Port = 44553
	require.NoError(t, Validate(cfg))
	for _, port := range []FlexiblePort{70000, -20983} {
		cfg.Port = port
		require.ErrorContains(t, Validate(cfg), "invalid port")
	}
}

func TestConfigurationLLMValidation(t *testing.T) {
	for _, row := range []struct{ provider, url, env, message string }{
		{"anthropic", "", "dev", ""}, {"openai", "", "dev", ""}, {"", "", "dev", ""},
		{"gemini", "", "dev", `invalid llm.provider "gemini"`},
		{"openai", "api.openai.com/v1", "dev", "invalid llm.base_url"}, {"openai", "/v1", "dev", "invalid llm.base_url"},
		{"openai", "ftp://host/v1", "dev", "invalid llm.base_url"}, {"openai", "://nope", "dev", "invalid llm.base_url"},
		{"openai", "http://localhost:11434/v1", "dev", "must use https"}, {"openai", "http://localhost:11434/v1", "prod", "must use https"},
		{"openai", "https://api.groq.com/openai/v1", "prod", ""},
	} {
		cfg := validationConfig(row.env)
		cfg.LLM = &LLMConfig{Provider: row.provider, BaseURL: row.url, APIKey: "k"}
		err := Validate(cfg)
		if row.message == "" {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, row.message)
		}
	}
	for _, row := range []struct {
		cfg   LLMConfig
		model string
	}{
		{LLMConfig{}, LLMDefaultModelAnthropic}, {LLMConfig{Provider: "anthropic"}, LLMDefaultModelAnthropic},
		{LLMConfig{Provider: "openai"}, LLMDefaultModelOpenAI}, {LLMConfig{Provider: "openai", Model: " custom-model "}, "custom-model"},
	} {
		require.Equal(t, row.model, row.cfg.ResolvedModel())
	}
}
