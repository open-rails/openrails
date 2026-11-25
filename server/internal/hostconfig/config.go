// Package hostconfig loads the openrails binary's configuration: config.yaml,
// mounted secret files and the environment.
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
	"time"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/server"
)

// Config is the openrails binary's configuration: one file, decoded twice.
// Server is what server.New takes, the engine's keys at the top level beside
// the server's; the other fields are the keys only the binary reads, the
// connections it opens and its logger. A key environment variable is its
// upper-cased path joined by "_" (database.url is DATABASE_URL, a list is
// comma-separated or a JSON array); envKeyToConfigKey holds the exceptions.
type Config struct {
	Server server.Config `yaml:"-"`
	// Database is the database section's connection; its schemas and lease
	// pool are Server.Engine.Database.
	Database DatabaseConnection `yaml:"database"`
	// Redis is the redis section; nil declares none.
	Redis *RedisConfig `yaml:"redis"`
	// ClickHouse is the clickhouse section's connection, the event store
	// usage needs; nil declares none, and usage is off. Its database is
	// Server.Engine.ClickHouse.
	ClickHouse *ClickHouseLogin `yaml:"clickhouse"`
	// SMTP is the email_smtp section; nil sends no email.
	SMTP *SMTPConfig `yaml:"email_smtp"`
	// Vault is the vault section's login: declaring any vault key connects;
	// nil logs in to no Vault. Its mounts are Server.Engine.Vault.
	Vault *VaultLogin `yaml:"vault"`
	// SigningKey is AuthKit's inline signing key (AUTHKIT_ACTIVE_KEY_ID,
	// AUTHKIT_ACTIVE_PRIVATE_KEY_PEM, AUTHKIT_PUBLIC_KEYS); without one
	// AuthKit reads auth.keys.path.
	SigningKey SigningKey `yaml:"signing_key"`
	// CloudflareProxies are Cloudflare's egress CIDRs, set only where
	// Cloudflare fronts this origin: trusted like trusted_proxies, and the
	// only peers whose CF-Connecting-IP AuthKit takes and whose CF-IPCountry
	// a checkout records.
	CloudflareProxies []string `yaml:"cloudflare_proxies"`
	// RateLimitsDisabled limits nothing: an empty rate_limits.
	RateLimitsDisabled bool `yaml:"rate_limits_disabled"`
	// MerchantManifestOverlays are YAML files in the merchant manifest's own
	// shape (secrets rendered by Vault Agent or a Kubernetes Secret volume),
	// merged over the boot manifest in order; later wins.
	MerchantManifestOverlays []string `yaml:"merchant_manifest_overlays"`
	// Logger is the binary's logger.
	Logger LoggerConfig `yaml:"logger"`
	// EnvFile is the .env file the load consumed; empty when none.
	EnvFile string `yaml:"-"`
}

type contextKey struct{}

// NewContext carries the loaded configuration to the command that runs.
func NewContext(ctx context.Context, cfg *Config) context.Context {
	return context.WithValue(ctx, contextKey{}, cfg)
}

// FromContext is the configuration NewContext stored, or nil.
func FromContext(ctx context.Context) *Config {
	cfg, _ := ctx.Value(contextKey{}).(*Config)
	return cfg
}

// Engine is the engine's configuration in ctx; nil without one.
func Engine(ctx context.Context) *billing.Config {
	if cfg := FromContext(ctx); cfg != nil {
		return &cfg.Server.Engine
	}
	return nil
}

// defaults is the configuration before any source is read: server.
// DefaultConfig (the engine's and the server's) and the binary's own. They
// run a local server against the Compose dependencies (`docker compose up -d
// postgres garnet`) with nothing else configured; none is a security
// exception, and a production deploy that leaves one unset fails loudly or is
// unharmed. A default that follows another key is a rule below, applied once
// every source is read.
func defaults() *Config {
	return &Config{
		Server: server.DefaultConfig(),
		// database.url: defaultDatabaseURL; database.sslmode: defaultSSLMode.
		// No redis: rate limits and lockouts live in memory, for one instance.
		// No clickhouse.addr: no event store, so usage is off.
		ClickHouse: &ClickHouseLogin{Username: "default"},
		Logger:     LoggerConfig{Level: "info", Format: "json"},
		// public_billing_base_url: defaultPublicBillingBaseURL; auth: authDefaults.
	}
}

// defaultDatabaseURL is Compose's Postgres, used when no database connection
// key is set. A deploy that forgets its database fails to connect; it never
// reaches another.
const defaultDatabaseURL = "postgres://app:app_password@localhost:5434/openrails_db?sslmode=disable"

// defaultPublicBillingBaseURL is the local server: http://localhost:<port>.
func defaultPublicBillingBaseURL(port int) string {
	return "http://localhost:" + strconv.Itoa(port)
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

// authDefaults are AuthKit's settings the binary defaults: the token issuer
// is public_billing_base_url. AuthKit's refresh sessions last 30 days and its
// tables live in "profiles". Registration stays the server's: closed, or
// invite_only with local_sign_in. AuthKit itself creates its one account
// directory, and a loopback issuer's development keys.
func authDefaults(a *authkit.Config, publicBillingBaseURL string) {
	a.Token.Issuer = cmp.Or(strings.TrimSpace(a.Token.Issuer), publicBillingBaseURL)
	a.Token.RefreshTokenDuration = cmp.Or(a.Token.RefreshTokenDuration, 30*24*time.Hour)
	a.Database.Schema = cmp.Or(a.Database.Schema, "profiles")
}

// cloudflare makes Cloudflare's egress a trusted proxy like any other to the
// engine, the only peers whose CF-IPCountry a checkout records, and the only
// peers AuthKit takes CF-Connecting-IP from, unless auth.http sets its own
// client-IP posture.
func cloudflare(s *server.Config, ranges []string) {
	if len(ranges) == 0 {
		return
	}
	http := authkit.HTTPConfig{}
	if s.Auth.HTTP != nil {
		http = *s.Auth.HTTP
	}
	if !http.DirectPeerIP && len(http.TrustedProxies) == 0 && len(http.CloudflareProxies) == 0 {
		http.TrustedProxies, http.CloudflareProxies = slices.Clone(s.Engine.TrustedProxies), ranges
	}
	s.Auth.HTTP = &http
	s.Engine.TrustedProxies = append(slices.Clone(s.Engine.TrustedProxies), ranges...)
	s.Engine.CloudflareProxies = slices.Clone(ranges)
}

// DatabaseConnection is the database section's connection: URL, or the
// connection string its parts assemble.
type DatabaseConnection struct {
	// URL is the connection string; it overrides the parts below.
	URL string `yaml:"url"`
	// Host is the database server, when URL is unset.
	Host string `yaml:"host"`
	// Port is the database server's port.
	Port string `yaml:"port"`
	// Name is the database.
	Name string `yaml:"name"`
	// Username is the role the server connects, migrates and runs as.
	Username string `yaml:"username"`
	// Password is the role's password.
	Password string `yaml:"password"`
	// SSLMode is libpq's sslmode for the parts: verify-full by default,
	// require for a loopback host or a Unix socket.
	SSLMode string `yaml:"sslmode"`
	// SSLRootCert is the PEM file of the CAs that verify the server's
	// certificate; unset, the system roots.
	SSLRootCert string `yaml:"sslrootcert"`
	// SQLTrace logs every query at debug level.
	SQLTrace bool `yaml:"sql_trace"`
}

// ConnectionString is URL, or the URL the parts assemble; "" when they are
// incomplete. Credentials and the database name are percent-encoded: a
// password with '@' or '/' must not change the host.
func (c DatabaseConnection) ConnectionString() string {
	if c.URL != "" {
		return c.URL
	}
	if c.Host == "" || c.Port == "" || c.Name == "" || c.Username == "" {
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
		Path:     "/" + c.Name,
		RawQuery: query.Encode(),
	}
	return u.String()
}

// databaseURL is database.url, or the URL the parts assemble, or with neither
// defaultDatabaseURL. A missing part refuses boot: nothing stands in for it.
func databaseURL(db DatabaseConnection) (string, error) {
	if url := strings.TrimSpace(db.URL); url != "" {
		return url, nil
	}
	var missing []string
	for _, part := range []struct{ key, value string }{
		{"database.host (DATABASE_HOST)", db.Host}, {"database.port (DATABASE_PORT)", db.Port},
		{"database.name (DATABASE_NAME)", db.Name}, {"database.username (DATABASE_USERNAME)", db.Username},
	} {
		if strings.TrimSpace(part.value) == "" {
			missing = append(missing, part.key)
		}
	}
	switch len(missing) {
	case 0:
		return db.ConnectionString(), nil
	case 4:
		return defaultDatabaseURL, nil
	default:
		return "", fmt.Errorf("database: database.url (DATABASE_URL) is unset and its parts are incomplete: set %s", strings.Join(missing, ", "))
	}
}

// RedisConfig is the redis section: Addr, or a redis:// or rediss:// URL.
type RedisConfig struct {
	// Addr is host:port.
	Addr string `yaml:"addr"`
	// URL is redis://[user[:password]@]host[:port][/db], or rediss:// over
	// TLS, in place of Addr, DB and TLS. Username and Password, when set, are
	// used over the URL's.
	URL string `yaml:"url"`
	// Username is the ACL user; empty is the default user.
	Username string `yaml:"username"`
	// Password is the user's password.
	Password string `yaml:"password"`
	// DB is the database number.
	DB int `yaml:"db"`
	// TLS connects over TLS, verifying the server against the system roots,
	// or against CACert, which implies TLS.
	TLS bool `yaml:"tls"`
	// CACert is the PEM bundle of the CA that signed the server's certificate.
	CACert string `yaml:"ca_cert"`
}

// ClickHouseLogin is how the binary connects to ClickHouse, over its native
// protocol. The password is a secret: CLICKHOUSE_PASSWORD, from the
// environment or a mounted secret file.
type ClickHouseLogin struct {
	// Addr is host:port of the native protocol, 9000 by default.
	Addr string `yaml:"addr"`
	// Username is the ClickHouse user.
	Username string `yaml:"username"`
	// Password is the user's password.
	Password string `yaml:"password"`
}

// SMTPConfig is the email_smtp section (EMAIL_SMTP_HOST, …), the same names
// every app uses; an empty host sends no email. From is one RFC 5322
// mailbox: "Shop <noreply@shop.example>".
type SMTPConfig struct {
	// Host is the SMTP server; empty sends no email.
	Host string `yaml:"host"`
	// Port is the server's port: 465 is implicit TLS, others STARTTLS.
	Port int `yaml:"port"`
	// Username is the SMTP login.
	Username string `yaml:"username"`
	// Password is the login's password.
	Password string `yaml:"password"`
	// From is the deployment's own mailbox.
	From string `yaml:"from"`
}

// VaultLogin is how the binary logs in to Vault (helpers/vault).
type VaultLogin struct {
	// Address is the server URL (VAULT_ADDR); empty is Vault's default,
	// https://127.0.0.1:8200.
	Address string `yaml:"address"`
	// Namespace is a Vault Enterprise namespace.
	Namespace string `yaml:"namespace"`
	// CACert is the path of the PEM bundle of the CA that signed Vault's
	// certificate (VAULT_CACERT). Set, it is the only trust: the system
	// roots are not consulted.
	CACert string `yaml:"ca_cert"`
	// AuthMethod is "token", "approle" or "kubernetes". Empty with a Token is
	// token.
	AuthMethod string `yaml:"auth_method"`
	// Token is a pre-issued Vault token.
	Token string `yaml:"token"`
	// RoleID is the AppRole role id.
	RoleID string `yaml:"role_id"`
	// SecretID is the AppRole secret id, when SecretsPath holds none.
	SecretID string `yaml:"secret_id"`
	// SecretsPath is a directory of operator-mounted secret files named after
	// environment variables. Every approle login reads VAULT_SECRET_ID there,
	// so a rotated secret is used; without one, SecretID.
	SecretsPath string `yaml:"secrets_path"`
	// K8sRole is the kubernetes auth method's role.
	K8sRole string `yaml:"k8s_role"`
	// AuthMount is where the approle or kubernetes auth method is mounted:
	// the -path of `vault auth enable`, without auth/. Empty is the method's
	// name.
	AuthMount string `yaml:"auth_mount"`
}

// finish applies the rules that follow other keys once every source is read,
// and drops the sections that declare nothing.
func (c *Config) finish(secretsDir string) error {
	s := &c.Server
	s.Engine.PublicBillingBaseURL = cmp.Or(strings.TrimSpace(s.Engine.PublicBillingBaseURL), defaultPublicBillingBaseURL(cmp.Or(s.Port, server.DefaultConfig().Port)))
	s.Engine.CheckoutOrigin = strings.TrimSpace(s.Engine.CheckoutOrigin)
	authDefaults(&s.Auth, s.Engine.PublicBillingBaseURL)
	cloudflare(s, c.CloudflareProxies)
	if c.RateLimitsDisabled {
		s.Engine.RateLimits, s.Engine.Captcha = billing.RateLimitsConfig{}, billing.CaptchaAccount{}
	}
	*s = s.WithDefaults()
	if _, err := c.Logger.Handler(io.Discard); err != nil {
		return err
	}
	url, err := databaseURL(c.Database)
	if err != nil {
		return err
	}
	c.Database.URL = url
	if c.Redis != nil && *c.Redis == (RedisConfig{}) { // a blank REDIS_PASSWORD
		c.Redis = nil
	}
	if c.ClickHouse != nil {
		if c.ClickHouse.Addr = strings.TrimSpace(c.ClickHouse.Addr); c.ClickHouse.Addr == "" {
			c.ClickHouse = nil
		}
	}
	if c.SMTP != nil && strings.TrimSpace(c.SMTP.Host) == "" {
		c.SMTP = nil
	}
	if c.Vault != nil {
		c.Vault.SecretsPath = cmp.Or(c.Vault.SecretsPath, secretsDir)
	}
	return nil
}

// Validate checks the loaded configuration: the listeners, and what the
// engine cannot serve without.
func Validate(cfg *Config) error {
	s := cfg.Server
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("invalid port %d: must be 1-65535", s.Port)
	}
	if s.PrivatePort < 0 || s.PrivatePort > 65535 || (s.PrivatePort != 0 && s.PrivatePort == s.Port) {
		return fmt.Errorf("invalid private_port %d: must be 1-65535 and not port", s.PrivatePort)
	}
	return config.Validate(&s.Engine)
}
