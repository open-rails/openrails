package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/iputil"
	log "github.com/sirupsen/logrus"
)

// FlexiblePort is a custom type that can unmarshal both strings and integers.
// Plain int, NOT int16 (#349): TCP ports run to 65535 and the kernel's default
// ephemeral range STARTS at 32768, so an int16 wrapped every ephemeral port
// negative (44553 → -20983) and the listener died.
type FlexiblePort int

// UnmarshalText implements the encoding.TextUnmarshaler interface
func (p *FlexiblePort) UnmarshalText(text []byte) error {
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

	*p = FlexiblePort(val)
	return nil
}

const ConfigContextKey string = "config"

type Config struct {
	Env  string       `koanf:"env,omitempty"`
	Port FlexiblePort `koanf:"port,omitempty"` // Standalone only: public HTTP port (default 3053)
	Host string       `koanf:"host,omitempty"` // Standalone only: address to bind to (default 0.0.0.0)

	// ProviderWriteMode is the behavior dial (#346, #355): how much OpenRails is
	// allowed to do against payment providers. It is independent from TestMode.
	// One of:
	//   - "full":     normal operation
	//   - "limited":  no system-initiated provider writes
	//   - "readonly": no provider writes
	// Unset defaults to "full" in development; outside development an explicit
	// provider_write_mode is REQUIRED (Validate refuses to boot without one).
	ProviderWriteMode string `koanf:"provider_write_mode,omitempty"`
	// Mode is a deprecated compatibility alias for provider_write_mode.
	Mode string `koanf:"mode,omitempty"`

	// TestMode is the sandbox-credential axis (#355), orthogonal to Mode. When
	// true, every rail routes to its sandbox environment AND the
	// credential guarantees attach: a live Stripe key (sk_live_/rk_live_)
	// refuses to boot, configured NMI accounts are probed at boot (a decline
	// of the non-issued test card proves production credentials and refuses
	// the boot), CCBill uses the sandbox URL, and Solana derives devnet.
	// When omitted, defaults to sandbox in development and live outside it
	// (Load sets this fail-closed; see #355) — so a local boot is sandbox by
	// default and a prod boot is live by default. test_mode=true is rejected
	// outside env=development — sandbox money is dev-only. Set test_mode=false
	// explicitly to run live credentials locally.
	TestMode bool `koanf:"test_mode,omitempty"`

	// APIURL is the base URL where billing's versioned routes are mounted.
	// Used for generating URLs (e.g., Solana Pay transaction_request URLs).
	//
	// Standalone mode: "https://api.mysite.com" (routes at /v1/*)
	// Embedded mode:   "https://api.mysite.com/billing" (routes at /billing/v1/*)
	//
	// Formula: generated_url = APIURL + {version_path} + "/checkout/:id/solana-pay"
	APIURL string `koanf:"api_url,omitempty"`

	DB         *DBConfig         `koanf:"db,omitempty"`
	Redis      *RedisConfig      `koanf:"redis,omitempty"`
	Auth       *AuthConfig       `koanf:"auth,omitempty"`
	ClickHouse *ClickHouseConfig `koanf:"clickhouse,omitempty"`
	Logger     *LoggerConfig     `koanf:"logger,omitempty"`
	SendGrid   *SendGridConfig   `koanf:"sendgrid,omitempty"`
	RateLimits *RateLimitsConfig `koanf:"rate_limits,omitempty"`
	Captcha    *CaptchaConfig    `koanf:"captcha,omitempty"`
	Encryption *EncryptionConfig `koanf:"encryption,omitempty"`
	Vault      *VaultConfig      `koanf:"vault,omitempty"`

	// SecretBackend declares WHERE merchant secrets physically live: "db" (the
	// DEK-encrypted Postgres store / values-injected) or "vault" (Vault KV-v2).
	// It is declared intent, never auto-detected and never auto-fallback — the data
	// lives in exactly one place (#661). Empty derives from vault.enabled for
	// backward-compat (historically vault.enabled=true implied Vault KV). Env:
	// SECRET_BACKEND / BILLING_SECRET_BACKEND.
	SecretBackend string `koanf:"secret_backend,omitempty"`
}

const (
	SecretBackendDB    = "db"
	SecretBackendVault = "vault"
)

// SecretStoreBackend returns where merchant secrets live: "vault" or "db".
// Explicit secret_backend wins; empty derives from vault.enabled (back-compat).
// Vault Transit signing is orthogonal to this — it can be used with either backend
// (#661); this only selects the KV secret store.
func (cfg *Config) SecretStoreBackend() string {
	if cfg == nil {
		return SecretBackendDB
	}
	switch strings.ToLower(strings.TrimSpace(cfg.SecretBackend)) {
	case SecretBackendVault:
		return SecretBackendVault
	case SecretBackendDB:
		return SecretBackendDB
	}
	if cfg.Vault != nil && cfg.Vault.Enabled {
		return SecretBackendVault
	}
	return SecretBackendDB
}

// EncryptionConfig configures per-merchant encryption-at-rest (issue #227). The
// master key wraps each merchant's Data Encryption Key (envelope encryption); the
// DEK encrypts sensitive at-rest field values (e.g. per-merchant rail
// credentials in openrails.merchant_secrets).
//
// Self-hosted / dev: supply MasterKey (base64 of 32 raw bytes) via config or the
// ENCRYPTION_MASTER_KEY env var. PRODUCTION: the master key should come from a
// KMS (the wrapped DEKs in openrails.merchant_deks stay in the DB; the master key
// that unwraps them never does). When MasterKey is empty, encryption is disabled
// and values are stored in plaintext (back-compat with pre-#227 deployments).
type EncryptionConfig struct {
	// MasterKey is the base64-encoded 32-byte AES-256 master key that wraps
	// per-merchant DEKs. Empty disables at-rest encryption.
	MasterKey string `koanf:"master_key,omitempty"`
}

// VaultConfig selects a HashiCorp Vault backend for per-merchant secrets (issue
// #251). When Enabled, the merchant secret store resolves to Vault KV-v2 (same
// (merchant, name) addressing) instead of DB+envelope, and Solana signing uses
// Vault Transit (the key never leaves Vault). Disabled by default; self-hosted
// uses the DB+envelope store. KV and Transit mounts are fixed to "secret" and
// "transit"; the secret cache TTL is fixed in code.
type VaultConfig struct {
	Enabled    bool   `koanf:"enabled,omitempty"`
	Address    string `koanf:"address,omitempty"`     // VAULT_ADDR; empty uses the api default
	AuthMethod string `koanf:"auth_method,omitempty"` // "token" | "approle" | "kubernetes"
	// Token is a pre-issued Vault token (VAULT_TOKEN). When set with no explicit
	// auth_method, token auth is selected (dev / e2e against a -dev Vault).
	Token    string `koanf:"token,omitempty"`
	RoleID   string `koanf:"role_id,omitempty"`
	SecretID string `koanf:"secret_id,omitempty"`
	K8sRole  string `koanf:"k8s_role,omitempty"`
}

// DBConfig holds database configuration.
// If URL is provided, it takes precedence. Otherwise, a PostgreSQL connection
// string is built from the individual parameters.
type DBConfig struct {
	// Full connection string (optional)
	URL string `koanf:"url"`

	// Individual connection parameters.
	Host     string `koanf:"host"`
	Port     string `koanf:"port"`
	Database string `koanf:"database"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
	SSLMode  string `koanf:"sslmode"`

	// Schema is the Postgres schema OpenRails owns (issue #165, #471). It is used
	// for OpenRails' own DDL/DML (the openrails tables) and, in STANDALONE mode,
	// for the River job-queue tables as well — i.e. standalone River schema ==
	// DB.Schema.
	//
	// Default: "openrails" (zero-config). Configure via config `db.schema` or env
	// `DB_SCHEMA`; OpenRails relocates ALL its tables (DDL + runtime queries) to
	// the configured schema, in both standalone and embedded mode (#471).
	//
	// Embedded/library mode: the host controls where River tables live by injecting
	// its own River client (embedded.SetRiverClient); that client owns its schema and
	// OpenRails never overrides it. See pkg/embedded for the full schema contract.
	//
	// Read the effective value via DBConfig.SchemaName() (it applies the default and
	// normalization). Do not read this field directly.
	Schema string `koanf:"schema"`
}

// GetConnectionString returns the database connection string.
// Priority order:
// 1. If URL is set, use it directly
// 2. If all atomic parameters are present, build connection string from them
// 3. Return empty string (caller should use defaults or error based on environment)
func (c *DBConfig) GetConnectionString() string {
	// 1. If URL is provided, use it
	if c.URL != "" {
		return c.URL
	}

	// 2. Build connection string from atomic parameters if all required fields are present
	if c.Host != "" && c.Port != "" && c.Database != "" && c.Username != "" {
		// Format: postgresql://username:password@host:port/database?sslmode=...
		connStr := fmt.Sprintf(
			"postgresql://%s:%s@%s:%s/%s",
			c.Username,
			c.Password,
			c.Host,
			c.Port,
			c.Database,
		)

		// Add query parameters
		params := []string{}

		// Default to TLS when sslmode is omitted. Local compose sets disable explicitly.
		sslMode := c.SSLMode
		if sslMode == "" {
			sslMode = "require"
		}
		params = append(params, fmt.Sprintf("sslmode=%s", sslMode))

		if len(params) > 0 {
			connStr += "?" + strings.Join(params, "&")
		}

		return connStr
	}

	// 3. No URL and incomplete atomic parameters - return empty (caller handles defaults)
	return ""
}

// DefaultSchema is the Postgres schema OpenRails uses when none is configured
// (#471 renamed it from the historical `billing`). It is also the canonical
// schema OpenRails' SQL is authored against; when a host configures a different
// schema, runtime queries and migration DDL are rewritten from this name to the
// configured one (see internal/db schema rewriting).
const DefaultSchema = "openrails"

// MigratekitApp is the migratekit app/tracking key written to
// public.migrations.app for OpenRails' own (non-River, non-AuthKit) migrations,
// in both Postgres and ClickHouse. It is the "app name" #471 standardized to
// "openrails" (from the historical "billing"). It is deliberately independent of
// the (configurable) schema name — the embedded engine's boot validation greps
// for this value, so hosts must keep it in lockstep.
const MigratekitApp = "openrails"

// RiverSchema is the Postgres schema River job-queue tables (`river_*`) always
// live in, in every mode (#545). It is deliberately NOT the OpenRails billing
// schema: River is runtime/infra state, never portable billing data, so keeping
// it in `public` (River's own documented default, alongside `public.migrations`
// and `pgcrypto`) leaves the OpenRails billing schema 100% portable for the
// embedded↔standalone data move (#544). In embedded mode a host that runs River
// injects its own client (which owns its schema); OpenRails only uses this when
// it constructs its own River client (standalone, or embedded with no injected
// client).
const RiverSchema = "public"

// schemaIdentRe restricts the OpenRails schema to a safe SQL identifier: it must
// start with a letter or underscore and contain only letters, digits, and
// underscores. This forbids quotes, spaces, and dots, so the value can be used to
// build search_path / River schema names without quoting hazards.
var schemaIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SchemaName returns the effective OpenRails Postgres schema (issue #165, #471),
// applying the `openrails` default and normalization (trim + lower-case). All
// OpenRails code that needs the schema (migrator, River client construction,
// runtime query rewriting) MUST go through this accessor rather than reading
// DBConfig.Schema directly or hardcoding "openrails".
func (c *DBConfig) SchemaName() string {
	if c == nil {
		return DefaultSchema
	}
	return normalizeSchema(c.Schema)
}

// normalizeSchema trims and lower-cases a schema identifier, falling back to the
// default when empty. Lower-casing keeps the value consistent with unquoted SQL
// identifier folding.
func normalizeSchema(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return DefaultSchema
	}
	return s
}

// validateSchema ensures a configured schema is a safe SQL identifier (#165).
func validateSchema(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil // empty == use default; valid
	}
	if !schemaIdentRe.MatchString(s) {
		return fmt.Errorf("db.schema %q is not a valid Postgres identifier (letters, digits, underscore only; must start with a letter or underscore)", raw)
	}
	return nil
}

func hasEnvPrefix(prefix string) bool {
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

// Provider-account environments (#641): the credentials' nature. A deployment is
// all-test OR all-live; test_mode is the switch.
const (
	ProviderEnvironmentTest = "test"
	ProviderEnvironmentLive = "live"
)

// ExpectedProviderEnvironment is the environment every provider account must
// declare for the given test_mode: test under sandbox, live in production.
func ExpectedProviderEnvironment(testMode bool) string {
	if testMode {
		return ProviderEnvironmentTest
	}
	return ProviderEnvironmentLive
}

// ReservedAccountRails maps a provider-account name to the rail it implies, so a
// config entry named after a self-contained gateway need not restate its rail.
var ReservedAccountRails = map[string]models.Rail{
	"ccbill": models.RailCCBill,
	"stripe": models.RailStripe,
	"solana": models.RailSolana,
}

// ProviderAccountConfig is one configured provider account: the rail (gateway) it
// is on plus that rail's credentials. The map key in a ProviderAccountSet is the
// operator-chosen account NAME (e.g. "mobius", "paykings" on rail nmi).
//
// For an account named after a self-contained gateway (ccbill, stripe, solana) the
// rail is inferred from the name; other names (e.g. "mobius") must set Rail.
type ProviderAccountConfig struct {
	// Rail is the gateway this account is on: nmi, ccbill, stripe, solana.
	// Required unless the account name is itself a reserved gateway name.
	Rail models.Rail `koanf:"rail"`
	// AccountID is this account's rail-native identity (#641/#655): NMI
	// gateway-id, Stripe acct_…, CCBill client_acc_num[/sub], or Solana wallet
	// (#592: operator-declared). REQUIRED — ValidateRailSet rejects an empty one.
	AccountID string `koanf:"account_id"`
	// Environment is test|live. A deployment is all-test or all-live; ValidateRailSet
	// rejects any account whose environment contradicts test_mode (#641). Empty →
	// derived from test_mode.
	Environment string `koanf:"environment"`
	// Archived keeps an account addressable for existing obligations and inbound
	// provider events, but excludes it from new checkout/subscription work.
	Archived bool `koanf:"archived"`

	// Exactly one provider block is set, matching Type. Credentials live ONLY in
	// the typed block — there is no flat fallback.
	NMI    *NMIRailConfig    `koanf:"nmi"`
	CCBill *CCBillRailConfig `koanf:"ccbill"`
	Stripe *StripeRailConfig `koanf:"stripe"`
	Solana *SolanaRailConfig `koanf:"solana"`
}

type NMIRailConfig struct {
	SecurityKey     string `koanf:"security_key"`
	TokenizationKey string `koanf:"tokenization_key"`
	WebhookSecret   string `koanf:"webhook_secret"`
}

type CCBillRailConfig struct {
	Salt             string `koanf:"salt"`
	ClientSubAcc     string `koanf:"client_sub_acc"`
	ClientAccNum     string `koanf:"client_acc_num"`
	DataLinkUsername string `koanf:"datalink_username"`
	DataLinkPassword string `koanf:"datalink_password"`
	// AllowedCIDRs is the CCBill webhook source allowlist (CIDR notation); empty
	// uses the documented default ranges. Parsed at boot — see iputil.Configure.
	AllowedCIDRs []string `koanf:"allowed_cidrs"`
}

type StripeRailConfig struct {
	SecretKey     string `koanf:"secret_key"`
	WebhookSecret string `koanf:"webhook_secret"`
	// WebhookSecretThin is the signing secret of a Stripe "thin" Event
	// Destination; when set, webhooks verify against either secret.
	WebhookSecretThin string `koanf:"webhook_secret_thin"`
}

type SolanaRailConfig struct {
	// RPCProvider selects the preferred Solana RPC provider. Empty defaults to
	// "helius"; without rpc_api_key the client uses public RPC fallback.
	RPCProvider string `koanf:"rpc_provider"`
	// RPCAPIKey is the API key for the selected RPC provider (currently helius).
	RPCAPIKey                       string                 `koanf:"rpc_api_key"`
	Tokens                          map[string]TokenConfig `koanf:"tokens"`
	SolanaPayRecurringSubscriptions bool                   `koanf:"solana_pay_recurring_subscriptions"`
	// Network is DERIVED from test_mode at startup (devnet under test_mode,
	// mainnet otherwise) — not configurable (#349).
	Network string `koanf:"-"`
}

// ProviderAccountSet is an in-memory set of payment-provider credential entries. It
// is not part of config.yaml/.env; private standalone installs seed provider
// credentials through merchant bootstrap/Vault state, and embedded hosts may
// pass a set programmatically during construction.
type ProviderAccountSet map[string]*ProviderAccountConfig

// EffectiveRail returns the account's rail (gateway), inferring it from a reserved
// account name when Rail is unset.
func (p *ProviderAccountConfig) EffectiveRail(name string) models.Rail {
	if p.Rail != "" {
		return models.Rail(strings.ToLower(string(p.Rail)))
	}
	normalizedName := strings.ToLower(strings.TrimSpace(name))
	if rail, ok := ReservedAccountRails[normalizedName]; ok {
		return rail
	}
	return ""
}

// EffectiveAccountID is the account's operator-declared rail-native identity
// (#641). ValidateRailSet requires account_id — there is NO fallback to the
// config map name (an account is never indexed by a name we made up).
func (p *ProviderAccountConfig) EffectiveAccountID() string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.AccountID)
}

// EffectiveEnvironment returns the account's declared environment (test|live), or
// the test_mode-derived default when unset (#641). An explicitly-set value that
// contradicts test_mode is rejected by ValidateRailSet.
func (p *ProviderAccountConfig) EffectiveEnvironment(testMode bool) string {
	if p != nil {
		if env := strings.ToLower(strings.TrimSpace(p.Environment)); env != "" {
			return env
		}
	}
	return ExpectedProviderEnvironment(testMode)
}

func (p *ProviderAccountConfig) normalizeTypedBlock(name string) error {
	if p == nil {
		return nil
	}
	effectiveType := p.EffectiveRail(name)
	blockCount := 0
	if p.NMI != nil {
		blockCount++
	}
	if p.CCBill != nil {
		blockCount++
	}
	if p.Stripe != nil {
		blockCount++
	}
	if p.Solana != nil {
		blockCount++
	}
	if blockCount == 0 {
		return nil
	}
	if blockCount > 1 {
		return fmt.Errorf("rail '%s' must set exactly one provider block matching type %q", name, effectiveType)
	}
	switch effectiveType {
	case models.RailNMI:
		if p.NMI == nil {
			return fmt.Errorf("rail '%s' type nmi must use nmi block", name)
		}
	case models.RailCCBill:
		if p.CCBill == nil {
			return fmt.Errorf("rail '%s' type ccbill must use ccbill block", name)
		}
	case models.RailStripe:
		if p.Stripe == nil {
			return fmt.Errorf("rail '%s' type stripe must use stripe block", name)
		}
	case models.RailSolana:
		if p.Solana == nil {
			return fmt.Errorf("rail '%s' type solana must use solana block", name)
		}
	default:
		return fmt.Errorf("rail '%s' has unknown type '%s'", name, effectiveType)
	}
	return nil
}

// IsNMI returns true if this rail config is for an NMI-backed rail.
func (p *ProviderAccountConfig) IsNMI(name string) bool {
	return p.EffectiveRail(name) == models.RailNMI
}

// IsCCBill returns true if this rail config is for CCBill.
func (p *ProviderAccountConfig) IsCCBill(name string) bool {
	return p.EffectiveRail(name) == models.RailCCBill
}

// IsStripe returns true if this rail config is for Stripe.
func (p *ProviderAccountConfig) IsStripe(name string) bool {
	return p.EffectiveRail(name) == models.RailStripe
}

// IsSolana returns true if this rail config is for Solana.
func (p *ProviderAccountConfig) IsSolana(name string) bool {
	return p.EffectiveRail(name) == models.RailSolana
}

// ToNMIProviderSettings converts the rail config to NMI provider settings.
// Only valid for NMI-type rails.
func (p *ProviderAccountConfig) ToNMIProviderSettings(name string) *NMIProviderSettings {
	s := &NMIProviderSettings{Name: strings.ToLower(strings.TrimSpace(name))}
	if p.NMI != nil {
		s.SecurityKey = p.NMI.SecurityKey
		s.TokenizationKey = p.NMI.TokenizationKey
		s.WebhookSecret = p.NMI.WebhookSecret
	}
	return s // TestMode set by caller based on test_mode
}

// ToCCBillConfig converts the rail config to CCBillConfig.
// Only valid for CCBill-type rails.
func (p *ProviderAccountConfig) ToCCBillConfig() *CCBillConfig {
	c := &CCBillConfig{} // TestMode set by caller based on global test_mode
	if p.CCBill != nil {
		c.Salt = p.CCBill.Salt
		c.ClientSubAcc = p.CCBill.ClientSubAcc
		c.ClientAccNum = p.CCBill.ClientAccNum
		c.DataLinkUsername = p.CCBill.DataLinkUsername
		c.DataLinkPassword = p.CCBill.DataLinkPassword
		c.AllowedCIDRs = p.CCBill.AllowedCIDRs
	}
	return c
}

type NMIProviderSettings struct {
	Name            string
	SecurityKey     string
	TokenizationKey string
	WebhookSecret   string
	TestMode        bool
}

type CCBillConfig struct {
	Salt         string `koanf:"salt"`
	ClientSubAcc string `koanf:"client_sub_acc"`
	ClientAccNum string `koanf:"client_acc_num"`
	TestMode     bool   `koanf:"test_mode"`

	DataLinkUsername string `koanf:"datalink_username"`
	DataLinkPassword string `koanf:"datalink_password"`

	// AllowedCIDRs is the CCBill webhook source allowlist (CIDR notation).
	// Empty means use iputil.DefaultCCBillIPRanges.
	AllowedCIDRs []string `koanf:"allowed_cidrs"`
}

type RedisConfig struct {
	Addr     string `koanf:"addr"`
	Password string `koanf:"password"`
	DB       int    `koanf:"db"`
}

// AuthConfig holds OpenRails' AuthKit control-plane configuration. Standalone
// OpenRails authenticates users/admins through this control plane; remote
// applications, JWKS/public keys, API keys, users, permission groups, roles, and
// permissions are seeded as AuthKit state rather than trusted from runtime
// config.yaml/.env issuer allow-lists.
type AuthConfig struct {
	// HARDCUT (#312/#537): there is no `auth.operator_tenant_slug` /
	// `auth.operator_tenant_admin_roles`. Admin authority is live merchant-local
	// AuthKit merchant permission-group state (or a deployment-minted admin API
	// key) - deployment authority, not membership in a separate operator group.
	// Load rejects the
	// deprecated keys.

	// Issuer is the AuthKit token issuer OpenRails signs as
	// (e.g. "https://openrails.mysite.com"). Required outside development; in
	// development Load defaults it to api_url (or http://localhost:<port>).
	Issuer string `koanf:"issuer,omitempty"`
}

// TokenConfig defines configuration for a specific Solana token
type TokenConfig struct {
	Mint     string `json:"mint" koanf:"mint"`         // Token mint address accepted on the configured Solana network.
	Name     string `json:"name" koanf:"name"`         // Token name.
	Decimals int    `json:"decimals" koanf:"decimals"` // Token decimal places.
}

// RateLimitsConfig is a map of endpoint identifier -> rate limit config
type RateLimitsConfig map[string]*RateLimit

// Provider write modes (#346, #355) — see Config.ProviderWriteMode. The former
// mode=test is gone (sandbox is the orthogonal test_mode axis) and "production"
// is renamed "full".
const (
	ProviderWriteModeFull     = "full"
	ProviderWriteModeLimited  = "limited"
	ProviderWriteModeReadOnly = "readonly"

	// Deprecated: use ProviderWriteMode*.
	ModeFull     = ProviderWriteModeFull
	ModeLimited  = ProviderWriteModeLimited
	ModeReadOnly = ProviderWriteModeReadOnly
)

// ValidProviderWriteModes contains all valid provider write values ("" = unset;
// dev only).
var ValidProviderWriteModes = map[string]bool{
	"":                        true,
	ProviderWriteModeFull:     true,
	ProviderWriteModeLimited:  true,
	ProviderWriteModeReadOnly: true,
}

// Deprecated: use ValidProviderWriteModes.
var ValidModes = ValidProviderWriteModes

// SendGridConfig holds process-wide SendGrid API configuration. Sender/display
// metadata is merchant-scoped and loaded from merchant_configurations.
type SendGridConfig struct {
	APIKey string `koanf:"api_key"`
}

type ClickHouseConfig struct {
	HTTPAddr   string `koanf:"http_addr"`   // HTTP address for queries, e.g., http://clickhouse:8123
	ClientAddr string `koanf:"client_addr"` // Native client address, e.g., clickhouse:9000
	Database   string `koanf:"db"`          // ClickHouse database name (e.g., analytics)
	Username   string `koanf:"user"`        // Optional username for authentication
	Password   string `koanf:"password"`    // Optional password for authentication
	Cluster    string `koanf:"cluster"`     // ClickHouse cluster name (e.g., billing)
}

// LoggerConfig holds logging configuration
type LoggerConfig struct {
	Level string `koanf:"level"` // debug | info | error
}

// RateLimit defines a rate limit policy.
// All rate limits use a fixed 1-minute window.
type RateLimit struct {
	// RequestsPerMinute is the maximum number of requests allowed per minute.
	RequestsPerMinute int `koanf:"requests_per_minute"`
}

const (
	CaptchaProviderTurnstile   = "turnstile"
	CaptchaProviderRecaptchaV3 = "recaptcha-v3"
	CaptchaProviderHCaptcha    = "hcaptcha"
)

// CaptchaConfig controls captcha challenges enabled after extreme rate-limit hits.
// CaptchaConfig is deliberately minimal (#353): provider choice + the account
// credentials. Everything else (verify/script URLs, action, score threshold,
// challenge TTL, escalation multiplier, challenged buckets) is hardcoded —
// they are protocol/policy constants, not deployment choices.
type CaptchaConfig struct {
	Provider  string `koanf:"provider"`
	SiteKey   string `koanf:"site_key"`
	SecretKey string `koanf:"secret_key"`
}

// IsEnabled reports whether captcha challenges are active. There is no
// enabled knob (#353): configuring the credentials IS the enablement signal —
// the system already applies captcha selectively (only after extreme
// rate-limit escalation, only on the challenged buckets).
func (c *CaptchaConfig) IsEnabled() bool {
	return c != nil && strings.TrimSpace(c.SiteKey) != "" && strings.TrimSpace(c.SecretKey) != ""
}

func (c *CaptchaConfig) EffectiveProvider() string {
	if c == nil || strings.TrimSpace(c.Provider) == "" {
		return CaptchaProviderTurnstile
	}
	return strings.ToLower(strings.TrimSpace(c.Provider))
}

func (c *CaptchaConfig) EffectiveVerifyURL() string {
	switch c.EffectiveProvider() {
	case CaptchaProviderRecaptchaV3:
		return "https://www.google.com/recaptcha/api/siteverify"
	case CaptchaProviderHCaptcha:
		return "https://hcaptcha.com/siteverify"
	default:
		return "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	}
}

func (c *CaptchaConfig) EffectiveScriptURL() string {
	switch c.EffectiveProvider() {
	case CaptchaProviderRecaptchaV3:
		siteKey := ""
		if c != nil {
			siteKey = strings.TrimSpace(c.SiteKey)
		}
		return "https://www.google.com/recaptcha/api.js?render=" + url.QueryEscape(siteKey)
	case CaptchaProviderHCaptcha:
		return "https://js.hcaptcha.com/1/api.js?render=explicit"
	default:
		return "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit"
	}
}

func (c *CaptchaConfig) EffectiveChallengeTTL() time.Duration {
	return 15 * time.Minute
}

func (c *CaptchaConfig) EffectiveExtremeMultiplier() int {
	return 3
}

func (c *CaptchaConfig) EffectiveMinScore() float64 {
	return 0.5
}

func (c *CaptchaConfig) EffectiveAction() string {
	return "billing_challenge"
}

func (c *CaptchaConfig) EffectiveChallengeBuckets() []string {
	return []string{"checkout", "payment-methods", "subscriptions"}
}

// Validate validates the billing configuration
func Validate(cfg *Config) error {
	// Skip strict validation in development environments
	// Provider write mode must be a known value — a typo (e.g. "redaonly") must
	// never silently boot with full behavior (#346).
	providerWriteMode, err := cfg.normalizedProviderWriteMode()
	if err != nil {
		return err
	}
	if !ValidProviderWriteModes[providerWriteMode] {
		return fmt.Errorf("invalid provider_write_mode %q: must be one of full, limited, readonly", providerWriteMode)
	}

	// Port range (#349): UnmarshalText validates string-typed values, but an
	// integer yaml value decodes straight into the field and must be checked
	// here. 0 = unset (the default port applies).
	if cfg.Port != 0 && (cfg.Port < 1 || cfg.Port > 65535) {
		return fmt.Errorf("invalid port %d: must be 1-65535", cfg.Port)
	}

	isDev := cfg.Env == "development" || cfg.Env == "dev" || cfg.Env == ""
	if !isDev {
		// Sandbox credentials are dev-only (#355): a production deployment
		// must never boot pointed at sandbox rails.
		if cfg.TestMode {
			return fmt.Errorf("test_mode=true is not allowed outside development")
		}
		// Outside development the operating mode must be declared explicitly —
		// "I forgot to set it" must never silently pick a behavior.
		switch cfg.GetProviderWriteMode() {
		case ProviderWriteModeFull, ProviderWriteModeLimited, ProviderWriteModeReadOnly:
		default:
			return fmt.Errorf("provider_write_mode is required outside development: set provider_write_mode (or env PROVIDER_WRITE_MODE) to one of full, limited, readonly")
		}
		if cfg.DB != nil {
			if strings.TrimSpace(cfg.DB.Username) == "admin" || strings.TrimSpace(cfg.DB.Password) == "admin_password" {
				return fmt.Errorf("default database credentials are not allowed outside development")
			}
		}
		if cfg.ClickHouse != nil {
			if strings.TrimSpace(cfg.ClickHouse.Username) == "analytics_user" || strings.TrimSpace(cfg.ClickHouse.Password) == "analytics_password" {
				return fmt.Errorf("default ClickHouse credentials are not allowed outside development")
			}
		}
	}
	if err := validateCaptcha(cfg.Captcha); err != nil {
		return fmt.Errorf("captcha config validation failed: %w", err)
	}

	// Always validate database configuration
	if err := validateDatabase(cfg.DB); err != nil {
		return fmt.Errorf("database config validation failed: %w", err)
	}

	if err := validateEncryption(cfg.Encryption); err != nil {
		return fmt.Errorf("encryption config validation failed: %w", err)
	}

	if err := validateSecretBackend(cfg); err != nil {
		return fmt.Errorf("secret_backend config validation failed: %w", err)
	}

	return nil
}

// validateSecretBackend checks the declared secret backend is valid and reachable.
// secret_backend=vault needs a Vault connection to serve the KV store (#661).
func validateSecretBackend(cfg *Config) error {
	switch strings.ToLower(strings.TrimSpace(cfg.SecretBackend)) {
	case "", SecretBackendDB, SecretBackendVault:
	default:
		return fmt.Errorf("secret_backend must be %q or %q", SecretBackendDB, SecretBackendVault)
	}
	if cfg.SecretStoreBackend() == SecretBackendVault && (cfg.Vault == nil || !cfg.Vault.Enabled) {
		return fmt.Errorf("secret_backend=vault requires vault.enabled (secrets declared in Vault KV need a Vault connection)")
	}
	return nil
}

func validateCaptcha(cfg *CaptchaConfig) error {
	if cfg == nil {
		return nil
	}

	switch cfg.EffectiveProvider() {
	case CaptchaProviderTurnstile, CaptchaProviderRecaptchaV3, CaptchaProviderHCaptcha:
	default:
		return fmt.Errorf("unsupported provider %q", cfg.Provider)
	}

	// Credentials ARE the enablement signal — a half-configured pair is the
	// one state that can only be a mistake.
	siteKey := strings.TrimSpace(cfg.SiteKey) != ""
	secretKey := strings.TrimSpace(cfg.SecretKey) != ""
	if siteKey != secretKey {
		return fmt.Errorf("captcha requires BOTH site_key and secret_key (set both to enable, neither to disable)")
	}
	return nil
}

// validateEncryption fails fast on a malformed at-rest encryption master key.
// An empty key is a legitimate state (encryption disabled) but is surfaced as a
// warning so the plaintext-storage downgrade is never silent (#227).
func validateEncryption(cfg *EncryptionConfig) error {
	if cfg == nil || strings.TrimSpace(cfg.MasterKey) == "" {
		log.Warn("encryption.master_key not set; per-merchant secrets are stored WITHOUT at-rest encryption")
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cfg.MasterKey))
	if err != nil {
		return fmt.Errorf("encryption.master_key must be valid base64: %w", err)
	}
	if len(raw) != 32 {
		return fmt.Errorf("encryption.master_key must decode to 32 bytes (AES-256); got %d", len(raw))
	}
	return nil
}

// validateStripeKeyForTestMode checks if the Stripe API key prefix matches the
// test_mode axis. If there's a mismatch, it logs a warning and clears the key to
// disable Stripe. This prevents accidentally processing real charges in a test
// environment or test charges in a live one.
func ValidateRailSet(cfg *Config, rails ProviderAccountSet) error {
	if len(rails) == 0 {
		return nil
	}
	isDev := true
	if cfg != nil {
		isDev = cfg.IsDev()
	}
	if err := validateRails(cfg, rails, isDev); err != nil {
		return fmt.Errorf("rails validation failed: %w", err)
	}
	return validateStripeKeyForTestMode(cfg, rails)
}

func validateStripeKeyForTestMode(cfg *Config, rails ProviderAccountSet) error {
	if cfg == nil {
		cfg = &Config{}
	}
	for name, stripeProc := range rails {
		if stripeProc == nil || stripeProc.EffectiveRail(name) != models.RailStripe || stripeProc.Stripe == nil {
			continue
		}

		secretKey := strings.TrimSpace(stripeProc.Stripe.SecretKey)
		if secretKey == "" {
			continue
		}

		// Both standard secret keys (sk_*) and restricted keys (rk_*) carry the
		// live/test mode in their prefix, so classify either form.
		isLiveKey := strings.HasPrefix(secretKey, "sk_live_") || strings.HasPrefix(secretKey, "rk_live_")
		isTestKey := strings.HasPrefix(secretKey, "sk_test_") || strings.HasPrefix(secretKey, "rk_test_")

		if cfg.IsTestMode() && isLiveKey {
			// Hard guarantee (#347): the sandbox environment must never hold a live
			// key — a mistakenly-test-modeed production system would otherwise carry
			// a credential that can move real money.
			return fmt.Errorf("stripe rail %q: live key (sk_live_/rk_live_) is not allowed when test_mode is enabled; use a test key or unset test_mode", strings.ToLower(strings.TrimSpace(name)))
		}
		if !cfg.IsTestMode() && isTestKey {
			log.Warnf("⚠️  Stripe test key provided for rail %q but test_mode is disabled (live credentials) - disabling Stripe", strings.ToLower(strings.TrimSpace(name)))
			log.Warn("   Use a live-mode key (sk_live_/rk_live_), or set test_mode=true for sandbox testing")
			stripeProc.Stripe.SecretKey = ""
		}
	}
	return nil
}

// validateRails validates all rails in the new Rails map
func validateRails(cfg *Config, rails ProviderAccountSet, isDev bool) error {
	// Count accounts per rail: with more than one, each must declare account_id
	// (the made-up map name can't be the provider identity, #641).
	countByRail := map[models.Rail]int{}
	for name, proc := range rails {
		if proc != nil {
			countByRail[proc.EffectiveRail(name)]++
		}
	}
	for name, proc := range rails {
		if proc == nil {
			continue
		}
		if err := proc.normalizeTypedBlock(name); err != nil {
			return err
		}

		// #641: all-test or all-live — a declared environment must match test_mode
		// (empty is derived and always passes).
		if env := strings.ToLower(strings.TrimSpace(proc.Environment)); env != "" {
			if env != ProviderEnvironmentTest && env != ProviderEnvironmentLive {
				return fmt.Errorf("rail '%s' has unknown environment '%s' (want test|live)", name, env)
			}
			testMode := cfg != nil && cfg.IsTestMode()
			if want := ExpectedProviderEnvironment(testMode); env != want {
				return fmt.Errorf("rail '%s' declares environment=%s but test_mode=%v requires environment=%s (a deployment is all-test or all-live; never mix)", name, env, testMode, want)
			}
		}

		effectiveType := proc.EffectiveRail(name)
		// #641: with multiple accounts on a rail, the made-up map name can't serve
		// as the provider identity — each must declare its real account_id. Solana is
		// exempt: its identity is derived from the signer, so account_id is ignored
		// there and never a required disambiguator.
		if effectiveType != models.RailSolana && countByRail[effectiveType] > 1 && strings.TrimSpace(proc.AccountID) == "" {
			return fmt.Errorf("rail '%s' shares rail %q with another account, so it must declare account_id (the rail-native id; no name fallback)", name, effectiveType)
		}
		switch effectiveType {
		case models.RailNMI:
			if err := validateNMIRail(name, proc, isDev); err != nil {
				return err
			}
		case models.RailCCBill:
			if err := validateCCBillRail(name, proc, isDev); err != nil {
				return err
			}
		case models.RailStripe:
			if err := validateStripeRail(name, proc, isDev); err != nil {
				return err
			}
		case models.RailSolana:
			if err := validateSolanaRail(name, proc, isDev); err != nil {
				return err
			}
		default:
			return fmt.Errorf("rail '%s' has unknown type '%s'", name, effectiveType)
		}
	}
	return nil
}

// validateNMIRail validates an NMI-type rail
func validateNMIRail(name string, proc *ProviderAccountConfig, isDev bool) error {
	if isDev {
		return nil // Skip strict validation in dev
	}
	nmi := proc.NMI
	if nmi == nil {
		return fmt.Errorf("rail '%s' (nmi): nmi block is required", name)
	}

	if strings.TrimSpace(nmi.SecurityKey) == "" {
		return fmt.Errorf("rail '%s' (nmi): security_key is required", name)
	}

	if strings.TrimSpace(nmi.WebhookSecret) == "" {
		return fmt.Errorf("rail '%s' (nmi): webhook_secret is required outside development (signature verification cannot be disabled in production)", name)
	}

	return nil
}

// validateCCBillRail validates a CCBill-type rail
func validateCCBillRail(name string, proc *ProviderAccountConfig, isDev bool) error {
	if isDev {
		return nil // Skip strict validation in dev
	}
	ccbill := proc.CCBill
	if ccbill == nil {
		return fmt.Errorf("rail '%s' (ccbill): ccbill block is required", name)
	}

	if strings.TrimSpace(ccbill.ClientAccNum) == "" {
		return fmt.Errorf("rail '%s' (ccbill): client_acc_num is required", name)
	}

	if strings.TrimSpace(ccbill.ClientSubAcc) == "" {
		return fmt.Errorf("rail '%s' (ccbill): client_sub_acc is required", name)
	}

	hasUsername := strings.TrimSpace(ccbill.DataLinkUsername) != ""
	hasPassword := strings.TrimSpace(ccbill.DataLinkPassword) != ""
	if hasUsername != hasPassword {
		return fmt.Errorf("rail '%s' (ccbill): both datalink_username and datalink_password must be provided when configuring DataLink", name)
	}

	return nil
}

// validateStripeRail validates a Stripe-type rail
func validateStripeRail(name string, proc *ProviderAccountConfig, isDev bool) error {
	stripe := proc.Stripe
	if stripe == nil {
		return fmt.Errorf("rail '%s' (stripe): stripe block is required", name)
	}
	if strings.TrimSpace(stripe.SecretKey) == "" {
		log.Warnf("rail '%s' (stripe): secret_key not configured; checkout unavailable", name)
	}

	if strings.TrimSpace(stripe.WebhookSecret) == "" {
		if !isDev {
			return fmt.Errorf("rail '%s' (stripe): webhook_secret is required outside development (signature verification cannot be disabled in production)", name)
		}
		log.Warnf("rail '%s' (stripe): webhook_secret not configured; signature verification disabled", name)
	}

	return nil
}

// validateSolanaRail validates only config-loading concerns. Solana token
// pricing/default policy belongs to internal/modules/solana/tokens and is
// applied at runtime by configureSolanaRail.
func validateSolanaRail(name string, proc *ProviderAccountConfig, isDev bool) error {
	solana := proc.Solana
	if solana == nil {
		return fmt.Errorf("rail '%s' (solana): solana block is required", name)
	}
	rpcProvider := strings.ToLower(strings.TrimSpace(solana.RPCProvider))
	switch rpcProvider {
	case "", "helius", "public":
	default:
		return fmt.Errorf("rail '%s' (solana): rpc_provider must be helius or public", name)
	}
	rpcAPIKey := strings.TrimSpace(solana.RPCAPIKey)
	if rpcProvider == "public" && rpcAPIKey != "" {
		return fmt.Errorf("rail '%s' (solana): rpc_provider public cannot use rpc_api_key", name)
	}

	return nil
}

// ByRail returns all provider accounts on the given rail, keyed by account name.
func (set ProviderAccountSet) ByRail(rail models.Rail) map[string]*ProviderAccountConfig {
	result := make(map[string]*ProviderAccountConfig)
	if set == nil {
		return result
	}
	for name, proc := range set {
		if proc != nil && proc.EffectiveRail(name) == rail {
			result[strings.ToLower(name)] = proc
		}
	}
	return result
}

// RailKeysByType returns configured account names on the given rail,
// sorted for deterministic diagnostics and selection.
func (set ProviderAccountSet) RailKeysByType(rail models.Rail) []string {
	if set == nil {
		return nil
	}
	keys := make([]string, 0)
	for name, proc := range set {
		if proc == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(name))
		if key != "" && proc.EffectiveRail(key) == rail {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// ActiveRailByType returns a deterministic non-archived provider account for a
// rail. Database-backed new-work selection uses created_at to pick the newest
// active account; config-only callers do not have that timestamp, so they use
// sorted config keys.
func (set ProviderAccountSet) ActiveRailByType(rail models.Rail) (string, *ProviderAccountConfig, error) {
	keys := set.RailKeysByType(rail)
	for _, key := range keys {
		proc := set[key]
		if proc == nil || proc.Archived {
			continue
		}
		return key, proc, nil
	}
	return "", nil, nil
}

// ActiveRailKeysByType returns the sorted names of non-archived accounts on a rail.
func (set ProviderAccountSet) ActiveRailKeysByType(rail models.Rail) []string {
	var out []string
	for _, key := range set.RailKeysByType(rail) {
		if !set[key].Archived {
			out = append(out, key)
		}
	}
	return out
}

// FindByAccountID returns the configured account on a rail whose EffectiveAccountID
// matches accountID (#641), used to target a specific provider account.
func (set ProviderAccountSet) FindByAccountID(rail models.Rail, accountID string) (*ProviderAccountConfig, bool) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, false
	}
	for _, key := range set.RailKeysByType(rail) {
		if set[key].EffectiveAccountID() == accountID {
			return set[key], true
		}
	}
	return nil, false
}

// GetCCBillRail returns the configured active CCBill rail.
func (set ProviderAccountSet) GetCCBillRail() *ProviderAccountConfig {
	_, proc, _ := set.ActiveRailByType(models.RailCCBill)
	return proc
}

// GetStripeRail returns the configured active Stripe rail.
func (set ProviderAccountSet) GetStripeRail() *ProviderAccountConfig {
	_, proc, _ := set.ActiveRailByType(models.RailStripe)
	return proc
}

// GetSolanaRail returns the configured active Solana rail.
func (set ProviderAccountSet) GetSolanaRail() *ProviderAccountConfig {
	_, proc, _ := set.ActiveRailByType(models.RailSolana)
	return proc
}

// GetRail returns a rail config by name.
func (set ProviderAccountSet) GetRail(name string) *ProviderAccountConfig {
	if set == nil {
		return nil
	}
	normalizedName := strings.ToLower(strings.TrimSpace(name))
	if proc, ok := set[normalizedName]; ok && proc != nil {
		return proc
	}
	return nil
}

// RailOf returns the rail of the named provider account, or "" if not found.
func (set ProviderAccountSet) RailOf(name string) models.Rail {
	proc := set.GetRail(name)
	if proc == nil {
		return ""
	}
	return proc.EffectiveRail(name)
}

func (cfg *Config) normalizedProviderWriteMode() (string, error) {
	if cfg == nil {
		return "", nil
	}
	providerWriteMode := strings.ToLower(strings.TrimSpace(cfg.ProviderWriteMode))
	legacyMode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if providerWriteMode != "" && legacyMode != "" && providerWriteMode != legacyMode {
		return "", fmt.Errorf("provider_write_mode %q conflicts with legacy mode %q", cfg.ProviderWriteMode, cfg.Mode)
	}
	if providerWriteMode == "" {
		providerWriteMode = legacyMode
	}
	return providerWriteMode, nil
}

// GetProviderWriteMode returns the normalized provider write mode ("" when unset).
// Unknown values are rejected by Validate; here they normalize to "" so a
// pre-validation caller never misreads a typo as a real policy. Unset means
// the dev default (full behavior) — that only ever serves development, because
// Validate requires an explicit provider_write_mode outside it.
func (cfg *Config) GetProviderWriteMode() string {
	mode, err := cfg.normalizedProviderWriteMode()
	if err != nil || !ValidProviderWriteModes[mode] {
		return ""
	}
	return mode
}

// GetMode is a deprecated compatibility wrapper for GetProviderWriteMode.
func (cfg *Config) GetMode() string {
	return cfg.GetProviderWriteMode()
}

// IsTestMode returns true if payment rails should use sandbox/test
// environments and the sandbox-credential guarantees apply (#355): Stripe
// live-key refusal, NMI boot probe, CCBill sandbox URL, Solana devnet.
// Orthogonal to ProviderWriteMode (pure behavior). When unset, Load defaults it
// to sandbox in development and live outside it (#355); Validate rejects
// test_mode=true outside development.
func (cfg *Config) IsTestMode() bool {
	return cfg.TestMode
}

// IsLimitedMode returns true if proactive payment-provider operations
// (dunning charges/cancellations, auto-top-ups, arrears collection, Solana
// pulls, catalog provider-object writes) are disabled, leaving only reactive,
// user-initiated operations. True in limited and readonly modes.
func (cfg *Config) IsLimitedMode() bool {
	mode := cfg.GetProviderWriteMode()
	return mode == ProviderWriteModeLimited || mode == ProviderWriteModeReadOnly
}

// IsProviderReadOnly returns true if EVERY provider write — even reactive,
// user-initiated ones — must be blocked (provider_write_mode=readonly). Reads
// (query APIs, verification) stay allowed.
func (cfg *Config) IsProviderReadOnly() bool {
	return cfg.GetProviderWriteMode() == ProviderWriteModeReadOnly
}

// IsDev returns true if the environment is development.
func (cfg *Config) IsDev() bool {
	return cfg.Env == "" || cfg.Env == "dev" || cfg.Env == "development"
}

// RequiresRLS reports whether startup must fail if the connected Postgres role
// bypasses row-level security. Development may use a privileged local DB role;
// every non-development environment must connect as an RLS-enforcing role.
func (cfg *Config) RequiresRLS() bool {
	return cfg != nil && !cfg.IsDev()
}

// assembleDBURL builds the database URL from atomic parameters if not explicitly set
func assembleDBURL(cfg *Config) {
	if cfg.DB == nil {
		return
	}

	// If URL is already explicitly set, nothing to do
	if cfg.DB.URL != "" {
		return
	}

	connStr := cfg.DB.GetConnectionString()
	if connStr == "" {
		return
	}

	cfg.DB.URL = connStr
}

// validateDatabase validates database configuration
func validateDatabase(cfg *DBConfig) error {
	if cfg == nil {
		return fmt.Errorf("database configuration is required")
	}

	// Database is always PostgreSQL
	// After assembleDBURL, cfg.URL should always be set
	if cfg.URL == "" {
		return fmt.Errorf("database URL could not be determined")
	}

	// OpenRails Postgres schema must be a safe identifier (#165).
	if err := validateSchema(cfg.Schema); err != nil {
		return err
	}

	return nil
}

// GetDefaultBillingConfig returns a billing configuration with sensible defaults
func GetDefaultBillingConfig() *Config {
	return &Config{
		Env:    "development",
		Host:   "0.0.0.0",
		Port:   3053,
		APIURL: "http://localhost:3053",
		DB: &DBConfig{
			Host:     "localhost",
			Port:     "5434",
			Database: "openrails_db",
			Username: "admin",
			Password: "admin_password",
			SSLMode:  "disable",
			Schema:   DefaultSchema,
		},
		Redis: &RedisConfig{
			// Match docker-compose's host-published Garnet port.
			Addr:     "localhost:6380",
			Password: "",
			DB:       0,
		},
		Auth: &AuthConfig{},
		ClickHouse: &ClickHouseConfig{
			HTTPAddr:   "http://localhost:8124",
			ClientAddr: "localhost:9003",
			Database:   "analytics",
			Username:   "analytics_user",     // Match docker-compose CLICKHOUSE_USER
			Password:   "analytics_password", // Match docker-compose CLICKHOUSE_PASSWORD
			Cluster:    "openrails",          // Match docker-compose cluster name
		},
		Logger: &LoggerConfig{
			Level: "info", // Default to info level (options: debug, info, warn, error, fatal, panic)
		},
		RateLimits: &RateLimitsConfig{
			"subscribe": &RateLimit{
				RequestsPerMinute: 20, // Mutation endpoint; per-user limit is the real fraud control
			},
			"checkout": &RateLimit{
				RequestsPerMinute: 10, // Kept tight to deter card-testing/abuse
			},
			"webhook": &RateLimit{
				// Per source IP. All webhooks from a rail share one bucket
				// (fixed rail IPs), so this must absorb rebill runs / event
				// bursts without 429-ing legit payment events. Webhooks are already
				// authenticated (signature + IP allowlist + body caps); this is a
				// DoS floor, not the primary control.
				RequestsPerMinute: 1200,
			},
			"payment": &RateLimit{
				RequestsPerMinute: 40,
			},
			"default": &RateLimit{
				RequestsPerMinute: 300, // SPA/NAT friendly (multiple users behind one IP)
			},
		},
		Captcha: &CaptchaConfig{
			Provider: CaptchaProviderTurnstile,
		},
	}
}

func loadConfigIfExists(k *koanf.Koanf, path string) error {
	if path == "" {
		return nil
	}
	candidates := []string{path}
	if !filepath.IsAbs(path) {
		candidates = append(candidates, filepath.Join("config", path))
		candidates = append(candidates, filepath.Join("./config", path))
	}
	visited := make(map[string]struct{})
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, ok := visited[candidate]; ok {
			continue
		}
		visited[candidate] = struct{}{}
		// Normalize the path before touching the filesystem. The path is
		// operator-supplied (CLI flag / env var / built-in default), but cleaning
		// it removes any "../" traversal segments defensively.
		cleaned := filepath.Clean(candidate)
		if _, err := os.Stat(cleaned); err == nil {
			if err := k.Load(file.Provider(cleaned), yaml.Parser()); err != nil {
				return fmt.Errorf("loading config file %s: %w", cleaned, err)
			}
			return nil
		}
	}
	return nil
}

func Load(configPath string) (*Config, error) {
	k := koanf.New(".")

	// Start from sensible defaults so zero-config works in containers/compose.
	cfg := GetDefaultBillingConfig()

	if err := godotenv.Load(); err != nil {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return nil, err
		}
	}

	if configPath == "" {
		if envPath := strings.TrimSpace(os.Getenv("OPENRAILS_CONFIG")); envPath != "" {
			configPath = envPath
		} else if envPath := strings.TrimSpace(os.Getenv("BILLING_CONFIG")); envPath != "" {
			configPath = envPath
		} else {
			configPath = "config.yaml"
		}
	}

	if err := loadConfigIfExists(k, configPath); err != nil {
		return nil, err
	}

	// Load environment variables using koanf's env provider.
	//
	// This follows the same approach as other Go services in this workspace:
	// - Lowercase env keys
	// - Apply targeted hardcoded mappings for tricky cases
	// - Otherwise, replace ONLY the first underscore with a dot (preserves snake_case field names)
	//
	// Examples:
	// - DB_URL -> db.url
	// - CLICKHOUSE_HTTP_ADDR -> clickhouse.http_addr
	envKeyToConfigKey := func(s string) string {
		s = strings.ToLower(s)

		// Special case: API_URL -> api_url (top-level, not nested api.url)
		if s == "api_url" {
			return "api_url"
		}

		// Special case: TEST_MODE -> test_mode (top-level bool, not nested test.env)
		if s == "test_mode" {
			return "test_mode"
		}
		if s == "provider_write_mode" {
			return "provider_write_mode"
		}
		if s == "mode" {
			return "mode"
		}

		// Canonical Vault env vars: VAULT_ADDR is HashiCorp's standard name for
		// the server URL, so map it to vault.address (the default first-underscore
		// split would yield vault.addr). VAULT_TOKEN already splits correctly.
		if s == "vault_addr" {
			return "vault.address"
		}

		// Replace only the first underscore for other nested config keys.
		if !strings.Contains(s, "_") {
			return s
		}
		return strings.Replace(s, "_", ".", 1)
	}

	envCallbackWithValue := func(key string, value string) (string, interface{}) {
		upperKey := strings.ToUpper(key)
		if upperKey == "MERCHANT" || upperKey == "AUTH_ISSUERS" || upperKey == "CORS_ORIGINS" || strings.HasPrefix(upperKey, "RAILS_") || strings.HasPrefix(upperKey, "STORE_") {
			return "", nil
		}

		mapped := envKeyToConfigKey(key)
		if mapped == "" {
			return "", nil
		}

		v := strings.TrimSpace(value)

		// Allow JSON for arrays/objects where the remaining infrastructure config
		// uses structured values.
		if len(v) >= 2 {
			if (v[0] == '[' && v[len(v)-1] == ']') || (v[0] == '{' && v[len(v)-1] == '}') {
				var decoded interface{}
				if err := json.Unmarshal([]byte(v), &decoded); err == nil {
					return mapped, decoded
				}
			}
		}

		return mapped, v
	}

	if err := k.Load(env.ProviderWithValue("", ".", envCallbackWithValue), nil); err != nil {
		return nil, fmt.Errorf("loading environment variables: %w", err)
	}

	// HARD CUT (#469): the AuthKit control plane is always on in standalone
	// mode; the verifier-only deployment mode is gone. The former
	// auth.control_plane.enabled knob is rejected — not silently ignored — so a
	// deployment that believed it was toggling the control plane finds out at
	// load time. Private posture is fixed in this repo, not a disabled control
	// plane.
	if k.Exists("auth.control_plane.enabled") {
		return nil, fmt.Errorf("auth.control_plane.enabled was removed (#469): the control plane is always on in standalone mode — delete the key; private/self-hosted registration is the default")
	}
	if k.Exists("merchant_cors") {
		return nil, fmt.Errorf("merchant_cors was removed (#519): browser CORS belongs to the host app, not OpenRails merchant config")
	}
	if k.Exists("billing_hot_path") || os.Getenv("OPENRAILS_BILLING_HOT_PATH_FAIL_POLICY") != "" || os.Getenv("BILLING_HOT_PATH_FAIL_POLICY") != "" {
		return nil, fmt.Errorf("billing_hot_path was removed: OpenRails does not enforce client degraded-mode policy; configure any fail-open/fail-closed behavior in the calling client")
	}

	ignoredPrivatePort := k.Exists("private_port") || os.Getenv("PRIVATE_PORT") != ""
	ignoredStoreConfig := k.Exists("store") || hasEnvPrefix("STORE_")
	ignoredMerchantConfig := k.Exists("merchant") || os.Getenv("MERCHANT") != ""
	ignoredCORSConfig := k.Exists("cors_origins") || os.Getenv("CORS_ORIGINS") != ""
	ignoredDBRequireRLS := k.Exists("db.require_rls") || os.Getenv("DB_REQUIRE_RLS") != ""
	ignoredAuthIssuers := k.Exists("auth.issuers") || k.Exists("auth.expected_audience") ||
		os.Getenv("AUTH_ISSUERS") != "" || os.Getenv("AUTH_EXPECTED_AUDIENCE") != ""
	ignoredRails := k.Exists("rails") || hasEnvPrefix("RAILS_")
	ignoredControlPlaneLegacy := k.Exists("auth.control_plane.issuer") ||
		k.Exists("auth.control_plane.issued_audiences") ||
		k.Exists("auth.control_plane.expected_audiences") ||
		k.Exists("auth.control_plane.public_user_registration") ||
		k.Exists("auth.control_plane.public_tenant_registration") ||
		k.Exists("auth.control_plane.public_hosted") ||
		k.Exists("auth.control_plane.token_prefix") ||
		k.Exists("auth.control_plane.platform_admin_user_id") ||
		os.Getenv("AUTH_CONTROL_PLANE_ISSUER") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_ISSUED_AUDIENCES") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_EXPECTED_AUDIENCES") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PUBLIC_USER_REGISTRATION") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PUBLIC_TENANT_REGISTRATION") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PUBLIC_HOSTED") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_TOKEN_PREFIX") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_BOOTSTRAP_ADMIN_SERVICE_TOKEN_NAME") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PLATFORM_ADMIN_USER_ID") != ""
	if ignoredStoreConfig {
		log.Warn("ignoring retired store config (#520): seed merchant profile fields with openrails push-merchant-config under merchants[].profile")
	}
	if ignoredMerchantConfig {
		log.Warn("ignoring retired merchant config (#520/#521): seed merchants with openrails push-merchant-config; standalone no longer pins a process-wide merchant")
	}
	if ignoredCORSConfig {
		log.Warn("ignoring retired cors_origins config (#519): browser CORS belongs to the host app, not OpenRails")
	}
	if ignoredDBRequireRLS {
		log.Warn("ignoring retired db.require_rls config: RLS enforcement is derived from env; development may bypass RLS, every other env requires an RLS-enforcing DB role")
	}
	if ignoredAuthIssuers {
		log.Warn("ignoring retired auth issuer/audience config (#521/#527): declare each merchant's host-app trust under merchants[].remote_application in the merchant config manifest")
	}
	if ignoredRails {
		log.Warn("ignoring retired rails config (#521): seed merchant provider_accounts and secrets with openrails push-merchant-config under merchants[].provider_accounts")
	}
	if ignoredControlPlaneLegacy {
		log.Warn("ignoring retired auth.control_plane config (#521): use auth.issuer / AUTH_ISSUER; audiences are fixed to openrails, standalone public hosted registration is unavailable in this repo, and platform-superadmin belongs in openrails-saas")
	}
	if ignoredPrivatePort {
		log.Warn("ignoring private_port: OpenRails serves a single HTTP listener; there is no separate internal port")
	}

	// Unmarshal into config struct (overlay onto defaults)
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}

	// These sections are intentionally tolerated for operator visibility during
	// migration, but they no longer participate in runtime configuration. Keep
	// Load permissive while ensuring the returned struct reflects the supported
	// infrastructure-only config surface.
	// Sandbox-by-default in development (#355): when test_mode is not explicitly
	// provided, a dev-like boot defaults to sandbox credentials so a local run
	// can never accidentally move real money against live rail credentials
	// — the dangerous case is silent ("forgot to set it"), so the safe value is
	// the one you get by omission. Outside development the default stays live
	// (false), and an explicit test_mode=true is still rejected by Validate: prod
	// opts into live only by omission and can never opt into sandbox. To run
	// live locally, set test_mode=false explicitly (env TEST_MODE=false, flag
	// --test-mode=false). This only governs standalone Load(); embedded hosts
	// build the Config programmatically and supply their own value.
	if !k.Exists("test_mode") {
		cfg.TestMode = cfg.IsDev()
	}

	// The control plane is mandatory in standalone mode (#469). In development
	// auth.issuer defaults to the deployment's own base URL so zero-config dev
	// boots; outside development a missing issuer fails fast at control-plane
	// construction.
	if cfg.Auth == nil {
		cfg.Auth = &AuthConfig{}
	}
	if strings.TrimSpace(cfg.Auth.Issuer) == "" {
		if isDev := cfg.Env == "development" || cfg.Env == "dev" || cfg.Env == ""; isDev {
			issuer := strings.TrimSpace(cfg.APIURL)
			if issuer == "" {
				port := int(cfg.Port)
				if port == 0 {
					port = 3053
				}
				issuer = fmt.Sprintf("http://localhost:%d", port)
			}
			cfg.Auth.Issuer = strings.TrimRight(issuer, "/")
		}
	}

	// Assemble DB URL from pieces if not explicitly set
	assembleDBURL(cfg)

	// Normalize the OpenRails Postgres schema to its canonical form (#165) so the
	// stored config value matches what SchemaName() resolves to. Validation of the
	// identifier happens in Validate(). Defaults to `openrails` (config.DefaultSchema).
	if cfg.DB != nil {
		cfg.DB.Schema = normalizeSchema(cfg.DB.Schema)
	}

	// Validate the loaded configuration
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}

// ConfigureProcessGlobals applies runtime process-level configuration derived
// from Config. It is intentionally outside Load: one-off CLIs should be able to
// parse and validate config without mutating webhook globals or emitting server
// startup banners.
func ConfigureProcessGlobals(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}

	// Configure the CCBill webhook IP allowlist from config. Empty list falls
	// back to the documented default ranges.
	if err := iputil.Configure(nil); err != nil {
		return fmt.Errorf("config validation failed: ccbill allowed_cidrs: %w", err)
	}
	return nil
}

// LogStartupStatus writes the operator-facing posture banners for long-running
// OpenRails processes. Keep this out of Load so one-off CLIs do not look like
// they started the payment server.
func LogStartupStatus(cfg *Config) {
	logTestModeStatus(cfg)
	logOperatingModeStatus(cfg)
}

func logOperatingModeStatus(cfg *Config) {
	switch cfg.GetProviderWriteMode() {
	case ProviderWriteModeReadOnly:
		log.Warn("⚠️  PROVIDER_WRITE_MODE=readonly - ZERO payment-provider writes; even user-initiated charges fail loudly")
		log.Info("   Reconciliation/forensics posture: provider reads + local serving only")
		log.Info("   Set provider_write_mode=limited or provider_write_mode=full to allow writes")
	case ProviderWriteModeLimited:
		log.Warn("⚠️  PROVIDER_WRITE_MODE=limited - No proactive payment-provider operations will be performed")
		log.Info("   Dunning charges/cancellations, auto-top-ups, arrears collection, Solana pulls and catalog provider writes are paused")
		log.Info("   Reactive operations (checkout, vault saves, user/admin cancels, webhooks) work normally")
		log.Info("   Set provider_write_mode=full to resume proactive operations")
	case ProviderWriteModeFull:
		log.Info("Provider write mode: full (complete behavior - charges, dunning, deletes all run)")
	}
}

// logTestModeStatus logs the credential environment at startup.
// This helps operators confirm whether they're on sandbox or live credentials.
func logTestModeStatus(cfg *Config) {
	if cfg.IsTestMode() {
		log.Warn("⚠️  TEST ENV ENABLED - No real charges will be processed")
		log.Info("   Payment providers will use sandbox/test environments:")
		log.Info("   - NMI: secure.networkmerchants.com with test-mode transactions")
		log.Info("   - CCBill: sandbox-api.ccbill.com")
		log.Info("   - Stripe: requires sk_test_* key")
		log.Info("   - Solana: devnet")
	} else {
		log.Warn("🔴 LIVE CREDENTIALS - Real charges enabled")
		log.Info("   Payment providers will use production environments")

		// Warn if running real charges in dev environment. This only happens when
		// TEST_MODE=false is set explicitly; omitted test_mode defaults to sandbox in dev.
		if cfg.IsDev() {
			log.Warn("⚠️  Real payment processing enabled in dev environment")
			log.Warn("   Set test_mode=true (env TEST_MODE=true, flag --test-mode) to use sandbox environments")
		}
	}
}
