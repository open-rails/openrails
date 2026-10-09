// Package hostconfig loads the standalone server's configuration: config.yaml,
// mounted secret files, the environment and command-line flags.
package hostconfig

import (
	"context"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	billing "github.com/open-rails/openrails/internal/config"
)

// Config is the standalone server's configuration (server.Config): the
// engine's, the server's AuthKit and the listener.
type Config struct {
	*billing.Config
	Auth *AuthConfig
	// ResourceServer accepts trusted issuers' access tokens on the merchant
	// API (resource_server).
	ResourceServer *ResourceServerConfig
	// LocalSignIn serves sign-in to the server's own accounts
	// (local_sign_in); off, people sign in at a trusted issuer.
	LocalSignIn bool
	// Host and Port are the HTTP listener (default 0.0.0.0:3053).
	Host string
	Port int
	// PrivatePort is the private listener's, on Host: the operator's
	// /metrics. 0 serves none.
	PrivatePort int
	// DrainDelay and ShutdownTimeout are server.Config's.
	DrainDelay      time.Duration
	ShutdownTimeout time.Duration
	// MerchantManifestOverlays are YAML files in the merchant manifest's own
	// shape (secrets rendered by Vault Agent or a Kubernetes Secret volume),
	// merged over the boot manifest in order; later wins.
	MerchantManifestOverlays []string
	// RouteGroups turns the server's route groups on (route_groups); each is
	// off by default.
	RouteGroups billing.RouteGroups
	// AdminConsole serves the merchant admin console; nil, the default,
	// serves none. ConsoleIssuer (admin_console.issuer) signs staff in to it
	// at a trusted issuer.
	AdminConsole  *billing.ConsoleMount
	ConsoleIssuer *ConsoleIssuer
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
	Port        port   `koanf:"port"`
	Host        string `koanf:"host"`
	PrivatePort port   `koanf:"private_port"`

	DrainDelay      time.Duration `koanf:"drain_delay"`
	ShutdownTimeout time.Duration `koanf:"shutdown_timeout"`

	ProviderWriteMode string `koanf:"provider_write_mode"`
	TestMode          string `koanf:"test_mode"`

	PublicBillingBaseURL string `koanf:"public_billing_base_url"`
	DashboardBaseURL     string `koanf:"dashboard_base_url"`

	Database           billing.DatabaseConfig    `koanf:"database"`
	DB                 *billing.DBConfig         `koanf:"db"`
	Redis              *billing.RedisConfig      `koanf:"redis"`
	Logger             *billing.LoggerConfig     `koanf:"logger"`
	EmailSMTP          *smtpFile                 `koanf:"email_smtp"`
	RateLimits         *billing.RateLimitsConfig `koanf:"rate_limits"`
	RateLimitsDisabled bool                      `koanf:"rate_limits_disabled"`
	Captcha            *billing.CaptchaConfig    `koanf:"captcha"`
	Encryption         *billing.EncryptionConfig `koanf:"encryption"`
	Vault              *billing.VaultConfig      `koanf:"vault"`
	AdminConsole       *adminConsoleFile         `koanf:"admin_console"`
	LLM                *billing.LLMConfig        `koanf:"llm"`
	RouteGroups        billing.RouteGroups       `koanf:"route_groups"`

	SecretBackend        string `koanf:"secret_backend"`
	CredentialSnapshotID string `koanf:"credential_snapshot_id"`
	CredentialReadOnly   bool   `koanf:"credential_read_only"`
	AlertSecretBackend   string `koanf:"alert_secret_backend"`

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

	Auth *AuthConfig `koanf:"auth"`

	ResourceServer *ResourceServerConfig `koanf:"resource_server"`
	LocalSignIn    bool                  `koanf:"local_sign_in"`
}

// defaults is the file before any source is read: the listener and the
// engine's protective limits, with no security exception. There is no
// default database: one is always named (databaseURL).
func defaults() *fileConfig {
	return &fileConfig{
		Host: "0.0.0.0",
		Port: 3053,
		// Long enough for endpoints to drop the instance before it stops
		// accepting; together within Kubernetes' default 30s grace period.
		DrainDelay:      5 * time.Second,
		ShutdownTimeout: 20 * time.Second,
		DB:              &billing.DBConfig{},
		Database:        billing.DatabaseConfig{Schema: billing.DefaultSchema},
		Logger:          &billing.LoggerConfig{Level: "info"},
		RateLimits:      billing.DefaultRateLimits(),
		Captcha:         billing.DefaultCaptcha(),
		Auth:            &AuthConfig{},
	}
}

// databaseURL is db.url, or the URL db's parts assemble. A missing part
// refuses boot: nothing stands in for it.
func databaseURL(db *billing.DBConfig) (string, error) {
	if db == nil {
		db = &billing.DBConfig{}
	}
	if url := strings.TrimSpace(db.URL); url != "" {
		return url, nil
	}
	var missing []string
	for _, part := range []struct{ key, value string }{
		{"db.host (DB_HOST)", db.Host}, {"db.port (DB_PORT)", db.Port},
		{"db.database (DB_DATABASE)", db.Database}, {"db.username (DB_USERNAME)", db.Username},
	} {
		if strings.TrimSpace(part.value) == "" {
			missing = append(missing, part.key)
		}
	}
	switch len(missing) {
	case 0:
		return billing.DBConnectionString(db), nil
	case 4:
		return "", fmt.Errorf("no database configured: set db.url (DB_URL), or db.host, db.port, db.database and db.username (DB_HOST, DB_PORT, DB_DATABASE, DB_USERNAME)")
	default:
		return "", fmt.Errorf("database: db.url (DB_URL) is unset and its parts are incomplete: set %s", strings.Join(missing, ", "))
	}
}

// redisSection is the redis section; one that sets nothing (a blank
// REDIS_PASSWORD) declares no Redis.
func redisSection(r *billing.RedisConfig) *billing.RedisConfig {
	if r == nil || *r == (billing.RedisConfig{}) {
		return nil
	}
	return r
}

// smtpFile is the email_smtp section (EMAIL_SMTP_HOST, …), the same names
// every app uses; an empty host sends no email. From is one RFC 5322 mailbox:
// "Shop <noreply@shop.example>".
type smtpFile struct {
	Host     string `koanf:"host"`
	Port     int    `koanf:"port"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
	From     string `koanf:"from"`
}

func (s *smtpFile) config() (*billing.SMTPConfig, error) {
	if s == nil {
		return nil, nil
	}
	var from billing.EmailAddress
	if raw := strings.TrimSpace(s.From); raw != "" {
		a, err := mail.ParseAddress(raw)
		if err != nil {
			return nil, fmt.Errorf("email_smtp.from (EMAIL_SMTP_FROM) %q: %w", s.From, err)
		}
		from = billing.EmailAddress{Name: a.Name, Address: a.Address}
	}
	if strings.TrimSpace(s.Host) == "" {
		return nil, nil
	}
	return &billing.SMTPConfig{Host: s.Host, Port: s.Port, Username: s.Username, Password: s.Password, From: from}, nil
}

// adminConsoleFile is the admin_console section: the server serves the
// console at path only while enabled.
type adminConsoleFile struct {
	Enabled bool           `koanf:"enabled"`
	Path    string         `koanf:"path"`
	Issuer  *ConsoleIssuer `koanf:"issuer"`
}

func (a *adminConsoleFile) mount() *billing.ConsoleMount {
	if a == nil || !a.Enabled {
		return nil
	}
	return &billing.ConsoleMount{Path: a.Path}
}

func (a *adminConsoleFile) issuer() *ConsoleIssuer {
	if a == nil || !a.Enabled {
		return nil
	}
	return a.Issuer
}

// config is the loaded file as the server's configuration.
func (f *fileConfig) config() (*Config, error) {
	posture, err := billing.ParseCredentialPosture(f.TestMode)
	if err != nil {
		return nil, err
	}
	smtp, err := f.EmailSMTP.config()
	if err != nil {
		return nil, err
	}
	return &Config{
		Config: &billing.Config{
			ProviderWriteMode:                 f.ProviderWriteMode,
			TestMode:                          posture,
			Database:                          f.Database,
			PublicBillingBaseURL:              f.PublicBillingBaseURL,
			DashboardBaseURL:                  f.DashboardBaseURL,
			DB:                                f.DB,
			Redis:                             redisSection(f.Redis),
			Logger:                            f.Logger,
			SMTP:                              smtp,
			RateLimits:                        f.RateLimits,
			RateLimitsDisabled:                f.RateLimitsDisabled,
			Captcha:                           f.Captcha,
			Encryption:                        f.Encryption,
			Vault:                             f.Vault,
			LLM:                               f.LLM,
			SecretBackend:                     f.SecretBackend,
			CredentialSnapshotID:              f.CredentialSnapshotID,
			CredentialReadOnly:                f.CredentialReadOnly,
			AlertSecretBackend:                f.AlertSecretBackend,
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
		ResourceServer:           f.ResourceServer,
		LocalSignIn:              f.LocalSignIn,
		Host:                     f.Host,
		Port:                     int(f.Port),
		PrivatePort:              int(f.PrivatePort),
		DrainDelay:               f.DrainDelay,
		ShutdownTimeout:          f.ShutdownTimeout,
		MerchantManifestOverlays: f.MerchantManifestOverlays,
		RouteGroups:              f.RouteGroups,
		AdminConsole:             f.AdminConsole.mount(),
		ConsoleIssuer:            f.AdminConsole.issuer(),
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
	if cfg.PrivatePort < 0 || cfg.PrivatePort > 65535 || (cfg.PrivatePort != 0 && cfg.PrivatePort == cfg.Port) {
		return fmt.Errorf("invalid private_port %d: must be 1-65535 and not port", cfg.PrivatePort)
	}
	if cfg.DrainDelay < 0 || cfg.ShutdownTimeout < 0 {
		return fmt.Errorf("drain_delay and shutdown_timeout must not be negative")
	}
	if err := billing.Validate(cfg.Config); err != nil {
		return err
	}
	if cfg.AdminConsole != nil {
		if err := billing.ValidateMountPath("admin_console.path", billing.AdminConsolePath(cfg.AdminConsole)); err != nil {
			return err
		}
	}
	if err := ValidateAuthTransport(cfg.Auth); err != nil {
		return err
	}
	allowLoopback := cfg.Auth != nil && cfg.Auth.AllowLoopbackHTTP
	return ValidateResourceServer(cfg.ResourceServer, allowLoopback)
}
