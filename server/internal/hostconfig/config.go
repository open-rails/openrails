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
	"strings"

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
	// Database is the database section's connection; its schema is
	// Server.Engine.Database.
	Database DatabaseConnection `yaml:"database"`
	// Redis is the redis section; nil declares none.
	Redis *RedisConfig `yaml:"redis"`
	// ClickHouse is the clickhouse section's connection, the event store
	// usage needs; nil (an empty url) declares none, and usage is off. Its
	// database becomes Server.Engine.ClickHouse.
	ClickHouse *ClickHouseConnection `yaml:"clickhouse"`
	// SMTP is the email_smtp section; nil sends no email.
	SMTP *SMTPConfig `yaml:"email_smtp"`
	// Auth is where the server's AuthKit keeps its keys. OpenRails derives
	// the rest of its configuration.
	Auth AuthConfig `yaml:"auth"`
	// Vault is the vault section's login: declaring any vault key connects;
	// nil logs in to no Vault. Its mounts are Server.Engine.Vault.
	Vault *VaultLogin `yaml:"vault"`
	// Logger is the binary's logger.
	Logger LoggerConfig `yaml:"logger"`
	// EnvFile is the .env file the load consumed; empty when none.
	EnvFile string `yaml:"-"`
}

// AuthConfig is the auth section.
type AuthConfig struct {
	// Keys is AuthKit's key directory.
	Keys AuthKeys `yaml:"keys"`
}

// AuthKeys is AuthConfig.Keys.
type AuthKeys struct {
	// Path is the directory holding AuthKit's signing keys (keys.json) and its
	// authenticator-app key (totp.key), reloaded when they change. Empty is
	// /vault/auth, or for a loopback public_url development keys AuthKit
	// generates beneath XDG_STATE_HOME.
	Path string `yaml:"path"`
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
// run a local server against the Compose dependencies (`docker compose up
// -d`) with nothing else configured; none is a security exception, and a
// production deploy that leaves one unset fails loudly or is unharmed. A
// default that follows another key is a rule below, applied once every
// source is read.
func defaults() *Config {
	return &Config{
		Server: server.DefaultConfig(),
		// database.url: DSN. No redis: rate limits and lockouts live in
		// memory, for one instance. public_url has no default: boot refuses
		// without it.
		ClickHouse: &ClickHouseConnection{URL: defaultClickHouseURL, Default: true},
		Logger:     LoggerConfig{Level: "info"},
	}
}

// defaultDatabaseURL is Compose's Postgres, used when neither database.url
// nor a libpq variable names a connection. A deploy that forgets its
// database fails to connect; it never reaches another.
const defaultDatabaseURL = "postgres://app:app_password@localhost:5434/openrails_db?sslmode=disable"

// defaultClickHouseURL is Compose's ClickHouse. Unlike a URL a source names,
// it is optional: unreachable at boot, the server runs without usage.
const defaultClickHouseURL = "clickhouse://localhost:9000/openrails"

// libpqConnection are the libpq variables that name a connection.
var libpqConnection = []string{"PGHOST", "PGHOSTADDR", "PGPORT", "PGDATABASE", "PGUSER", "PGSERVICE"}

// loopbackHost reports a host on this machine, or a Unix socket.
func loopbackHost(host string) bool {
	if strings.HasPrefix(host, "/") || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DatabaseConnection is the database section's connection.
type DatabaseConnection struct {
	// URL is the connection string. Unset, libpq's variables name it
	// (PGHOST, PGPORT, PGDATABASE, PGUSER, PGPASSWORD or PGPASSFILE,
	// PGSSLMODE, PGSSLROOTCERT); with neither, Compose's local Postgres.
	URL string `yaml:"url"`
	// SessionURL is the same database and role without the pooler, set when
	// URL is a transaction pooler (PgBouncer in transaction mode): listeners,
	// session locks, migrations and AuthKit connect through it. Unset, URL
	// reaches PostgreSQL directly or through a session pooler.
	SessionURL string `yaml:"session_url"`
}

// DSN is the connection string the pool opens, getenv reading the process
// environment: URL; else "" for pgx to read the libpq variables, with
// sslmode verify-full off this machine unless PGSSLMODE names another; else
// defaultDatabaseURL.
func (c DatabaseConnection) DSN(getenv func(string) string) string {
	if url := strings.TrimSpace(c.URL); url != "" {
		return url
	}
	named := false
	for _, v := range libpqConnection {
		named = named || getenv(v) != ""
	}
	switch {
	case !named:
		return defaultDatabaseURL
	case getenv("PGSSLMODE") == "" && !loopbackHost(cmp.Or(getenv("PGHOST"), getenv("PGHOSTADDR"), "localhost")):
		return "sslmode=verify-full"
	}
	return ""
}

// RedisConfig is the redis section.
type RedisConfig struct {
	// URL is redis://[user[:password]@]host[:port][/db], or rediss:// over
	// TLS.
	URL string `yaml:"url"`
	// CACert is the PEM bundle of the CA that signed the server's
	// certificate, for a rediss:// URL; unset, the system roots.
	CACert string `yaml:"ca_cert"`
}

// ClickHouseConnection is how the binary connects to ClickHouse, over its
// native protocol.
type ClickHouseConnection struct {
	// URL is clickhouse://[user[:password]@]host[:port][/database], port 9000
	// by default; the path names the event store's database, openrails by
	// default; empty declares no event store. A secret: CLICKHOUSE_URL, from
	// the environment or a mounted secret file.
	URL string `yaml:"url"`
	// Default reports that no source named URL: it is Compose's ClickHouse,
	// and the server runs without usage when it does not answer at boot. A
	// URL a source names is waited for.
	Default bool `yaml:"-"`
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
	s.Engine.PublicURL = strings.TrimSpace(s.Engine.PublicURL)
	s.Engine.CheckoutOrigin = strings.TrimSpace(s.Engine.CheckoutOrigin)
	*s = s.WithDefaults()
	if _, err := c.Logger.Handler(io.Discard); err != nil {
		return err
	}
	if c.Redis != nil && strings.TrimSpace(c.Redis.URL) == "" && strings.TrimSpace(c.Redis.CACert) == "" { // a blank REDIS_URL
		c.Redis = nil
	}
	if c.ClickHouse != nil && strings.TrimSpace(c.ClickHouse.URL) == "" {
		c.ClickHouse = nil
	}
	if c.ClickHouse != nil {
		u, err := url.Parse(strings.TrimSpace(c.ClickHouse.URL))
		if err != nil || u.Scheme != "clickhouse" || u.Host == "" {
			return fmt.Errorf("clickhouse.url (CLICKHOUSE_URL) must be clickhouse://[user[:password]@]host[:port][/database]")
		}
		if name := strings.Trim(u.Path, "/"); name != "" {
			s.Engine.ClickHouse.Database = name
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
	if s.MetricsPort < 0 || s.MetricsPort > 65535 || (s.MetricsPort != 0 && s.MetricsPort == s.Port) {
		return fmt.Errorf("invalid metrics_port %d: must be 1-65535 and not port", s.MetricsPort)
	}
	if s.Engine.PublicURL == "" {
		return fmt.Errorf("public_url (PUBLIC_URL) is required: where browsers and PSPs reach this server, without /v1 (http://localhost:%d locally)", s.Port)
	}
	return config.Validate(&s.Engine)
}
