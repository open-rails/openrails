// Package hostconfig loads the standalone server's configuration: config.yaml,
// mounted secret files, the environment and command-line flags.
package hostconfig

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/open-rails/authkit"
	billing "github.com/open-rails/openrails/internal/config"
)

// Config is the standalone server's configuration (server.Config): the
// engine's, the server's AuthKit, the listener and the connections hostdeps
// opens.
type Config struct {
	*billing.Config
	// DB is the db section: the Postgres the server's pool connects to.
	DB DBConfig
	// Redis is the redis section; nil declares none.
	Redis *RedisConfig
	// ClickHouse is the clickhouse section's connection, the event store
	// usage needs; nil declares none, and usage is off. Its database is
	// the engine's Config.ClickHouse.
	ClickHouse *ClickHouseLogin
	// SMTP is the email_smtp section; nil sends no email.
	SMTP *SMTPConfig
	// Vault is the vault section's login; nil logs in to no Vault. Its
	// mounts are the engine's Config.Vault.
	Vault *VaultLogin
	// Proxies are trusted_proxies and cloudflare_proxies as written. The
	// engine's TrustedProxies is both; AuthKit takes CF-Connecting-IP from
	// Cloudflare's alone.
	Proxies Proxies
	// Auth is the auth section: AuthKit's configuration, by AuthKit's keys.
	Auth authkit.Config
	// SigningKey is AuthKit's inline signing key (AUTHKIT_ACTIVE_KEY_ID,
	// AUTHKIT_ACTIVE_PRIVATE_KEY_PEM, AUTHKIT_PUBLIC_KEYS); without one
	// AuthKit reads auth.keys.path.
	SigningKey SigningKey
	// LocalSignIn serves sign-in to the server's own accounts
	// (local_sign_in); off, people sign in at a trusted issuer.
	LocalSignIn bool
	// Naming is the rename policy for merchant names (naming).
	Naming billing.NamingConfig
	// Host and Port are the HTTP listener (default 0.0.0.0:3053).
	Host string
	Port int
	// PrivatePort is the private listener's, on Host: the operator's
	// /metrics. 0 serves none.
	PrivatePort int
	// MerchantManifestOverlays are YAML files in the merchant manifest's own
	// shape (secrets rendered by Vault Agent or a Kubernetes Secret volume),
	// merged over the boot manifest in order; later wins.
	MerchantManifestOverlays []string
	// RouteGroups turns the server's route groups on (route_groups); each is
	// off by default.
	RouteGroups billing.RouteGroups
	// AdminConsole controls the optional console at /admin and its app sign-in.
	// Nil keeps the console available with no declared app issuers.
	AdminConsole *ConsoleConfig
	// Logger is the binary's logger (logger).
	Logger LoggerConfig
	// EnvFile is the .env file the load consumed; empty when none.
	EnvFile string
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

	UsageBillingEnabled bool   `koanf:"usage_billing_enabled"`
	EventRetention      string `koanf:"event_retention"`

	PublicBillingBaseURL string `koanf:"public_billing_base_url"`

	Database                databaseFile                          `koanf:"database"`
	DB                      *DBConfig                             `koanf:"db"`
	Redis                   *RedisConfig                          `koanf:"redis"`
	ClickHouse              *clickHouseFile                       `koanf:"clickhouse"`
	Logger                  LoggerConfig                          `koanf:"logger"`
	EmailSMTP               *SMTPConfig                           `koanf:"email_smtp"`
	RateLimits              billing.RateLimitsConfig              `koanf:"rate_limits"`
	RateLimitsDisabled      bool                                  `koanf:"rate_limits_disabled"`
	UserActionRateLimit     billing.UserActionRateLimitConfig     `koanf:"user_action_rate_limit"`
	DestructiveActionLimits billing.DestructiveActionLimitsConfig `koanf:"destructive_action_limits"`
	Captcha                 *billing.CaptchaConfig                `koanf:"captcha"`
	Vault                   *vaultFile                            `koanf:"vault"`
	AdminConsole            *ConsoleConfig                        `koanf:"admin_console"`
	CheckoutOrigin          string                                `koanf:"checkout_origin"`
	LLM                     *billing.LLMConfig                    `koanf:"llm"`
	RouteGroups             billing.RouteGroups                   `koanf:"route_groups"`

	MerchantManifestOverlays []string `koanf:"merchant_manifest_overlays"`

	WebhookSecretOverlap string `koanf:"webhook_secret_overlap"`

	ReturnOrigins     []string `koanf:"return_origins"`
	TrustedProxies    []string `koanf:"trusted_proxies"`
	CloudflareProxies []string `koanf:"cloudflare_proxies"`

	ProviderSandbox     *billing.ProviderSandboxConfig `koanf:"provider_sandbox"`
	Solana              *billing.SolanaConfig          `koanf:"solana"`
	HyperSwitch         *billing.HyperSwitchConfig     `koanf:"hyperswitch"`
	CardVault           *billing.CardVaultConfig       `koanf:"card_vault"`
	EngineAdmissionHold bool                           `koanf:"engine_admission_hold"`

	// Auth is AuthKit's configuration, decoded by AuthKit's own keys
	// (authConfig).
	Auth        map[string]any       `koanf:"auth"`
	SigningKey  SigningKey           `koanf:"signing_key"`
	LocalSignIn bool                 `koanf:"local_sign_in"`
	Naming      billing.NamingConfig `koanf:"naming"`
}

// defaults is the file before any source is read: the listener and the
// engine's protective limits, with no security exception. There is no
// default database: one is always named (databaseURL).
func defaults() *fileConfig {
	return &fileConfig{
		Host:       "0.0.0.0",
		Port:       3053,
		DB:         &DBConfig{},
		Database:   databaseFile{Schema: billing.DefaultSchema},
		Logger:     LoggerConfig{Level: "info", Format: "json"},
		RateLimits: billing.DefaultRateLimits(),
		Captcha:    billing.DefaultCaptcha(),
	}
}

// databaseFile is the database section: the engine's schemas. The lease
// pool's size is db.lease_conns.
type databaseFile struct {
	Schema         string `koanf:"schema"`
	RiverSchema    string `koanf:"river_schema"`
	SkipMigrations bool   `koanf:"skip_migrations"`
}

// DBConfig is the db section: a Postgres connection. URL takes precedence;
// otherwise the connection string is built from the parts.
type DBConfig struct {
	URL      string `koanf:"url"`
	Host     string `koanf:"host"`
	Port     string `koanf:"port"`
	Database string `koanf:"database"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
	// SSLMode is libpq's sslmode for the parts: verify-full by default,
	// require for a loopback host or a Unix socket.
	SSLMode string `koanf:"sslmode"`
	// SSLRootCert is the PEM file of the CAs that verify the server's
	// certificate; unset, the system roots.
	SSLRootCert string `koanf:"sslrootcert"`
	// SQLTrace logs every query at debug level.
	SQLTrace bool `koanf:"sql_trace"`
	// LeaseConns is the engine's Database.LeaseConns.
	LeaseConns int `koanf:"lease_conns"`
}

// ConnectionString is URL, or the URL the parts assemble; "" when they are
// incomplete. Credentials and the database name are percent-encoded: a
// password with '@' or '/' must not change the host.
func (c DBConfig) ConnectionString() string {
	if c.URL != "" {
		return c.URL
	}
	if c.Host == "" || c.Port == "" || c.Database == "" || c.Username == "" {
		return ""
	}
	query := url.Values{"sslmode": {cmp.Or(c.SSLMode, defaultSSLMode(c.Host))}}
	if c.SSLRootCert != "" {
		query.Set("sslrootcert", c.SSLRootCert)
	}
	u := url.URL{
		Scheme:   "postgresql",
		User:     url.UserPassword(c.Username, c.Password),
		Host:     net.JoinHostPort(c.Host, c.Port),
		Path:     "/" + c.Database,
		RawQuery: query.Encode(),
	}
	return u.String()
}

// defaultSSLMode verifies the server's certificate and name off this
// machine; a loopback host or a Unix socket only requires TLS.
func defaultSSLMode(host string) string {
	if strings.HasPrefix(host, "/") || strings.EqualFold(host, "localhost") {
		return "require"
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "require"
	}
	return "verify-full"
}

// RedisConfig is the redis section: Addr, or a redis:// or rediss:// URL.
type RedisConfig struct {
	// Addr is host:port.
	Addr string `koanf:"addr"`
	// URL is redis://[user[:password]@]host[:port][/db], or rediss:// over
	// TLS, in place of Addr, DB and TLS. Username and Password, when set, are
	// used over the URL's.
	URL string `koanf:"url"`
	// Username is the ACL user; empty is the default user.
	Username string `koanf:"username"`
	Password string `koanf:"password"`
	DB       int    `koanf:"db"`
	// TLS connects over TLS, verifying the server against the system roots,
	// or against CACert, which implies TLS.
	TLS bool `koanf:"tls"`
	// CACert is the PEM bundle of the CA that signed the server's certificate.
	CACert string `koanf:"ca_cert"`
}

// clickHouseFile is the clickhouse section: the connection and the engine's
// database.
type clickHouseFile struct {
	ClickHouseLogin `koanf:",squash"`
	// Database is the event store's database; empty is "openrails".
	Database string `koanf:"database"`
}

// ClickHouseLogin is how the server connects to ClickHouse, over its native
// protocol. The password is a secret: CLICKHOUSE_PASSWORD, from the
// environment or a mounted secret file.
type ClickHouseLogin struct {
	// Addr is host:port of the native protocol, 9000 by default.
	Addr     string `koanf:"addr"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
}

// split is the server's login and the engine's database; an empty section
// declares none.
func (c *clickHouseFile) split() (*ClickHouseLogin, billing.ClickHouseConfig) {
	if c == nil || strings.TrimSpace(c.Addr) == "" {
		return nil, billing.ClickHouseConfig{}
	}
	login := c.ClickHouseLogin
	login.Addr = strings.TrimSpace(login.Addr)
	return &login, billing.ClickHouseConfig{Database: strings.TrimSpace(c.Database)}
}

// SMTPConfig is the email_smtp section (EMAIL_SMTP_HOST, …), the same names
// every app uses; an empty host sends no email. From is one RFC 5322
// mailbox: "Shop <noreply@shop.example>".
type SMTPConfig struct {
	Host     string `koanf:"host"`
	Port     int    `koanf:"port"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
	From     string `koanf:"from"`
}

// Proxies are the CIDRs whose forwarded client address is trusted.
type Proxies struct {
	Trusted []string
	// Cloudflare are Cloudflare's egress CIDRs, set only where Cloudflare
	// fronts this origin.
	Cloudflare []string
}

// vaultFile is the vault section: the login and the engine's mounts.
type vaultFile struct {
	VaultLogin   `koanf:",squash"`
	ScopePrefix  string `koanf:"scope_prefix"`
	KVMount      string `koanf:"kv_mount"`
	TransitMount string `koanf:"transit_mount"`
}

// VaultLogin is how the server logs in to Vault (helpers/vault).
type VaultLogin struct {
	// Address is the server URL (VAULT_ADDR); empty is Vault's default,
	// https://127.0.0.1:8200.
	Address   string `koanf:"address"`
	Namespace string `koanf:"namespace"`
	// CACert is the path of the PEM bundle of the CA that signed Vault's
	// certificate (VAULT_CACERT). Set, it is the only trust: the system
	// roots are not consulted.
	CACert string `koanf:"ca_cert"`
	// AuthMethod is "token", "approle" or "kubernetes". Empty with a Token is
	// token.
	AuthMethod string `koanf:"auth_method"`
	// Token is a pre-issued Vault token.
	Token    string `koanf:"token"`
	RoleID   string `koanf:"role_id"`
	SecretID string `koanf:"secret_id"`
	// SecretsPath is a directory of operator-mounted secret files named after
	// environment variables. Every approle login reads VAULT_SECRET_ID there,
	// so a rotated secret is used; without one, SecretID.
	SecretsPath string `koanf:"secrets_path"`
	K8sRole     string `koanf:"k8s_role"`
	// AuthMount is where the approle or kubernetes auth method is mounted:
	// the -path of `vault auth enable`, without auth/. Empty is the method's
	// name.
	AuthMount string `koanf:"auth_mount"`
}

// databaseURL is db.url, or the URL db's parts assemble. A missing part
// refuses boot: nothing stands in for it.
func databaseURL(db *DBConfig) (string, error) {
	if db == nil {
		db = &DBConfig{}
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
		return db.ConnectionString(), nil
	case 4:
		return "", fmt.Errorf("no database configured: set db.url (DB_URL), or db.host, db.port, db.database and db.username (DB_HOST, DB_PORT, DB_DATABASE, DB_USERNAME)")
	default:
		return "", fmt.Errorf("database: db.url (DB_URL) is unset and its parts are incomplete: set %s", strings.Join(missing, ", "))
	}
}

// redisSection is the redis section; one that sets nothing (a blank
// REDIS_PASSWORD) declares no Redis.
func redisSection(r *RedisConfig) *RedisConfig {
	if r == nil || *r == (RedisConfig{}) {
		return nil
	}
	return r
}

// smtpSection is the email_smtp section; an empty host declares none.
func smtpSection(s *SMTPConfig) *SMTPConfig {
	if s == nil || strings.TrimSpace(s.Host) == "" {
		return nil
	}
	return s
}

// vault splits the vault section into the server's login and the engine's
// mounts.
func (v *vaultFile) split() (*VaultLogin, *billing.VaultConfig) {
	if v == nil {
		return nil, nil
	}
	login := v.VaultLogin
	return &login, &billing.VaultConfig{KVMount: v.KVMount, ScopePrefix: v.ScopePrefix, TransitMount: v.TransitMount}
}

// config is the loaded file as the server's configuration.
func (f *fileConfig) config() (*Config, error) {
	auth, err := authConfig(f.Auth)
	if err != nil {
		return nil, err
	}
	if _, err := f.Logger.Handler(io.Discard); err != nil {
		return nil, err
	}
	login, mounts := f.Vault.split()
	clickHouse, eventStore := f.ClickHouse.split()
	db := DBConfig{}
	if f.DB != nil {
		db = *f.DB
	}
	return &Config{
		Config: &billing.Config{
			UsageBillingEnabled: f.UsageBillingEnabled,
			EventRetention:      f.EventRetention,
			Database: billing.DatabaseConfig{
				Schema: f.Database.Schema, RiverSchema: f.Database.RiverSchema,
				SkipMigrations: f.Database.SkipMigrations, LeaseConns: db.LeaseConns,
			},
			ClickHouse:              eventStore,
			PublicBillingBaseURL:    f.PublicBillingBaseURL,
			RateLimits:              f.RateLimits,
			UserActionRateLimit:     f.UserActionRateLimit,
			DestructiveActionLimits: f.DestructiveActionLimits,
			Captcha:                 f.Captcha,
			Vault:                   mounts,
			LLM:                     f.LLM,
			WebhookSecretOverlap:    f.WebhookSecretOverlap,
			ReturnOrigins:           f.ReturnOrigins,
			// Cloudflare's egress is a proxy like any other to the engine.
			TrustedProxies:      append(slices.Clone(f.TrustedProxies), f.CloudflareProxies...),
			CheckoutOrigin:      strings.TrimSpace(f.CheckoutOrigin),
			ProviderSandbox:     f.ProviderSandbox,
			Solana:              f.Solana,
			HyperSwitch:         f.HyperSwitch,
			CardVault:           f.CardVault,
			EngineAdmissionHold: f.EngineAdmissionHold,
		},
		DB:                       db,
		Redis:                    redisSection(f.Redis),
		ClickHouse:               clickHouse,
		SMTP:                     smtpSection(f.EmailSMTP),
		Vault:                    login,
		Proxies:                  Proxies{Trusted: f.TrustedProxies, Cloudflare: f.CloudflareProxies},
		Auth:                     auth,
		SigningKey:               f.SigningKey,
		LocalSignIn:              f.LocalSignIn,
		Naming:                   f.Naming,
		Host:                     f.Host,
		Port:                     int(f.Port),
		PrivatePort:              int(f.PrivatePort),
		MerchantManifestOverlays: f.MerchantManifestOverlays,
		RouteGroups:              f.RouteGroups,
		AdminConsole:             f.AdminConsole,
		Logger:                   f.Logger,
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
	if err := billing.Validate(cfg.Config); err != nil {
		return err
	}

	return nil
}
