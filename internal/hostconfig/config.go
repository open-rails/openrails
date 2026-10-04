// Package hostconfig loads the standalone server's configuration: config.yaml,
// mounted secret files, the environment and command-line flags.
package hostconfig

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	billing "github.com/open-rails/openrails/internal/config"
)

// Config is the standalone server's configuration: the engine's, the control
// plane's identity and the listener.
type Config struct {
	*billing.Config
	Auth *billing.AuthConfig
	// Host and Port are the HTTP listener (default 0.0.0.0:3053).
	Host string
	Port int
	// MerchantManifestOverlays are YAML files in the merchant manifest's own
	// shape (secrets rendered by Vault Agent or a Kubernetes Secret volume),
	// merged over the boot manifest in order; later wins.
	MerchantManifestOverlays []string
}

type contextKey struct{}

// NewContext carries the loaded configuration to the command that runs.
func NewContext(ctx context.Context, cfg *Config) context.Context {
	return context.WithValue(ctx, contextKey{}, cfg)
}

// FromContext is the configuration NewContext stored; a zero Config without one.
func FromContext(ctx context.Context) *Config {
	if cfg, ok := ctx.Value(contextKey{}).(*Config); ok {
		return cfg
	}
	return &Config{}
}

// fileConfig is config.yaml: every key the standalone server reads. A key's
// environment variable is its upper-cased path joined by "_" (db.url is
// DB_URL, a list is comma-separated or a JSON array); envKeyToConfigKey holds
// the exceptions. Keys inside a section match the section's Go field names
// without their underscores (db.sql_trace is DBConfig.SQLTrace).
type fileConfig struct {
	Port port   `koanf:"port"`
	Host string `koanf:"host"`

	ProviderWriteMode string `koanf:"provider_write_mode"`
	TestMode          string `koanf:"test_mode"`
	Schema            string `koanf:"schema"`

	PublicBillingBaseURL string `koanf:"public_billing_base_url"`
	DashboardBaseURL     string `koanf:"dashboard_base_url"`

	DB                 *billing.DBConfig           `koanf:"db"`
	Redis              *billing.RedisConfig        `koanf:"redis"`
	Logger             *billing.LoggerConfig       `koanf:"logger"`
	SendGrid           *billing.SendGridConfig     `koanf:"sendgrid"`
	RateLimits         *billing.RateLimitsConfig   `koanf:"rate_limits"`
	RateLimitsDisabled bool                        `koanf:"rate_limits_disabled"`
	Captcha            *billing.CaptchaConfig      `koanf:"captcha"`
	Encryption         *billing.EncryptionConfig   `koanf:"encryption"`
	Vault              *billing.VaultConfig        `koanf:"vault"`
	AdminConsole       *billing.AdminConsoleConfig `koanf:"admin_console"`
	LLM                *billing.LLMConfig          `koanf:"llm"`

	SecretBackend        string `koanf:"secret_backend"`
	CredentialSnapshotID string `koanf:"credential_snapshot_id"`
	CredentialReadOnly   bool   `koanf:"credential_read_only"`
	AlertSecretBackend   string `koanf:"alert_secret_backend"`
	MerchantConfigHTTP   bool   `koanf:"merchant_config_http"`
	AllowCatalogUpdates  bool   `koanf:"allow_catalog_updates"`

	MerchantManifestOverlays []string `koanf:"merchant_manifest_overlays"`

	CatalogReconciliationInterval     string `koanf:"catalog_reconciliation_interval"`
	ProviderBillingQuiescenceInterval string `koanf:"provider_billing_quiescence_interval"`
	WebhookSecretOverlap              string `koanf:"webhook_secret_overlap"`

	ReturnOrigins            []string `koanf:"return_origins"`
	TrustedProxies           []string `koanf:"trusted_proxies"`
	CloudflareProxies        []string `koanf:"cloudflare_proxies"`
	CCBillWebhookIPAllowlist []string `koanf:"ccbill_webhook_ip_allowlist"`

	ProviderSandbox     *billing.ProviderSandboxConfig `koanf:"provider_sandbox"`
	HyperSwitch         *billing.HyperSwitchConfig     `koanf:"hyperswitch"`
	EngineAdmissionHold bool                           `koanf:"engine_admission_hold"`

	Auth *billing.AuthConfig `koanf:"auth"`
}

// defaults is the file before any source is read: local infrastructure and
// the engine's protective limits, with no security exception.
func defaults() *fileConfig {
	return &fileConfig{
		Host: "0.0.0.0",
		Port: 3053,
		DB: &billing.DBConfig{
			Host: "localhost", Port: "5434", Database: "openrails_db",
			// Application login used by the local Docker setup.
			Username: "app", Password: "app_password", SSLMode: "disable",
		},
		Schema: billing.DefaultSchema,
		// Match docker-compose's host-published Garnet port.
		Redis:      &billing.RedisConfig{Addr: "localhost:6380"},
		Logger:     &billing.LoggerConfig{Level: "info"},
		RateLimits: billing.DefaultRateLimits(),
		Captcha:    billing.DefaultCaptcha(),
		Auth:       &billing.AuthConfig{},
	}
}

// config is the loaded file as the server's configuration.
func (f *fileConfig) config() (*Config, error) {
	posture, err := billing.ParseCredentialPosture(f.TestMode)
	if err != nil {
		return nil, err
	}
	return &Config{
		Config: &billing.Config{
			ProviderWriteMode:                 f.ProviderWriteMode,
			TestMode:                          posture,
			Schema:                            f.Schema,
			PublicBillingBaseURL:              f.PublicBillingBaseURL,
			DashboardBaseURL:                  f.DashboardBaseURL,
			DB:                                f.DB,
			Redis:                             f.Redis,
			Logger:                            f.Logger,
			SendGrid:                          f.SendGrid,
			RateLimits:                        f.RateLimits,
			RateLimitsDisabled:                f.RateLimitsDisabled,
			Captcha:                           f.Captcha,
			Encryption:                        f.Encryption,
			Vault:                             f.Vault,
			AdminConsole:                      f.AdminConsole,
			LLM:                               f.LLM,
			SecretBackend:                     f.SecretBackend,
			CredentialSnapshotID:              f.CredentialSnapshotID,
			CredentialReadOnly:                f.CredentialReadOnly,
			AlertSecretBackend:                f.AlertSecretBackend,
			MerchantConfigHTTP:                f.MerchantConfigHTTP,
			AllowCatalogUpdates:               f.AllowCatalogUpdates,
			CatalogReconciliationInterval:     f.CatalogReconciliationInterval,
			ProviderBillingQuiescenceInterval: f.ProviderBillingQuiescenceInterval,
			WebhookSecretOverlap:              f.WebhookSecretOverlap,
			ReturnOrigins:                     f.ReturnOrigins,
			TrustedProxies:                    f.TrustedProxies,
			CloudflareProxies:                 f.CloudflareProxies,
			CCBillWebhookIPAllowlist:          f.CCBillWebhookIPAllowlist,
			ProviderSandbox:                   f.ProviderSandbox,
			HyperSwitch:                       f.HyperSwitch,
			EngineAdmissionHold:               f.EngineAdmissionHold,
		},
		Auth:                     f.Auth,
		Host:                     f.Host,
		Port:                     int(f.Port),
		MerchantManifestOverlays: f.MerchantManifestOverlays,
	}, nil
}

// fieldAliases are the keys whose Go field is not the key without underscores.
var fieldAliases = map[string]string{"public_keys": "PublicKeysJSON"}

// matchField reports whether a file key names a Go field.
func matchField(key, field string) bool {
	if alias, ok := fieldAliases[key]; ok {
		return alias == field
	}
	return key == field || strings.EqualFold(strings.ReplaceAll(key, "_", ""), field)
}

// port is a TCP port written as a number or a string. It is an int: ports run
// to 65535, past an int16.
type port int

// UnmarshalText reads a port from an environment variable or a quoted value.
func (p *port) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*p = 0
		return nil
	}
	val, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return fmt.Errorf("invalid port value: %w", err)
	}
	if val < 1 || val > 65535 {
		return fmt.Errorf("invalid port value %d: must be 1-65535", val)
	}
	*p = port(val)
	return nil
}

// Validate checks the composed standalone host configuration.
func Validate(cfg *Config) error {
	if cfg == nil || cfg.Config == nil {
		return fmt.Errorf("standalone config is required")
	}
	// A number in config.yaml decodes without UnmarshalText; 0 is unset.
	if cfg.Port < 0 || cfg.Port > 65535 {
		return fmt.Errorf("invalid port %d: must be 1-65535", cfg.Port)
	}
	if err := billing.Validate(cfg.Config); err != nil {
		return err
	}
	return billing.ValidateAuthTransport(cfg.Auth)
}
