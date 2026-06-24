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

	// Mode is the behavior dial (#346, #355): how much OpenRails is allowed to
	// do against payment providers. It is independent from TestEnv. One of:
	//   - "full":     normal operation
	//   - "limited":  no system-initiated provider writes
	//   - "readonly": no provider writes
	// Unset defaults to "full" in development; outside development an explicit
	// mode is REQUIRED (Validate refuses to boot without one).
	Mode string `koanf:"mode,omitempty"`

	// TestEnv is the sandbox-credential axis (#355), orthogonal to Mode. When
	// true, every processor routes to its sandbox environment AND the
	// credential guarantees attach: a live Stripe key (sk_live_/rk_live_)
	// refuses to boot, configured NMI accounts are probed at boot (a decline
	// of the non-issued test card proves production credentials and refuses
	// the boot), CCBill uses the sandbox URL, and Solana derives devnet.
	// When omitted, defaults to sandbox in development and live outside it
	// (Load sets this fail-closed; see #355) — so a local boot is sandbox by
	// default and a prod boot is live by default. test_env=true is rejected
	// outside env=development — sandbox money is dev-only. Set test_env=false
	// explicitly to run live credentials locally.
	TestEnv bool `koanf:"test_env,omitempty"`

	// APIURL is the base URL where billing's versioned routes are mounted.
	// Used for generating URLs (e.g., Solana Pay transaction_request URLs).
	//
	// Standalone mode: "https://api.mysite.com" (routes at /v1/*)
	// Embedded mode:   "https://api.mysite.com/billing" (routes at /billing/v1/*)
	//
	// Formula: generated_url = APIURL + {version_path} + "/checkout/:id/solana-pay"
	APIURL string `koanf:"api_url,omitempty"`

	// USDCFunding configures external wallet-funding providers. These providers
	// only move USDC into the user's self-custody wallet; OpenRails checkout still
	// collects payment from that wallet afterwards.
	USDCFunding *USDCFundingConfig `koanf:"usdc_funding,omitempty"`

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
}

// EncryptionConfig configures per-merchant encryption-at-rest (issue #227). The
// master key wraps each merchant's Data Encryption Key (envelope encryption); the
// DEK encrypts sensitive at-rest field values (e.g. per-merchant processor
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

// ProcessorType constants for the unified processor config
const (
	ProcessorTypeNMI    = "nmi"
	ProcessorTypeCCBill = "ccbill"
	ProcessorTypeStripe = "stripe"
	ProcessorTypeSolana = "solana"

	ProcessorRolePrimary   = "primary"
	ProcessorRoleSecondary = "secondary"
	ProcessorRoleLegacy    = "legacy"
)

// ReservedProcessorNames maps processor names that imply their type.
// These names don't require an explicit "type" field in config.
var ReservedProcessorNames = map[string]string{
	"ccbill": ProcessorTypeCCBill,
	"stripe": ProcessorTypeStripe,
	"solana": ProcessorTypeSolana,
}

// ProcessorConfig is the unified configuration for all payment processors.
// The Type field determines which fields are relevant:
//   - type: nmi     → NMI fields (security_key, webhook_secret, etc.)
//   - type: ccbill  → CCBill fields (client_acc_num, client_sub_acc, salt, etc.)
//   - type: stripe  → Stripe fields (secret_key, webhook_secret)
//   - type: solana  → Solana fields (recipient_wallet, rpc_endpoint, etc.)
//
// Reserved names (ccbill, stripe, solana) don't need explicit type - it's implied.
// Non-reserved names (e.g., "acme") require type: nmi.
type ProcessorConfig struct {
	// Type specifies the processor type: "nmi", "ccbill", "stripe", "solana"
	// Required for non-reserved processor names.
	// For reserved names (ccbill, stripe, solana), type is inferred from the name.
	Type string `koanf:"type"`
	// Role selects routine routing for this configured credential set. Empty is
	// primary for existing single-provider configs. secondary is enabled for
	// explicit/manual targeting; legacy is retained for old rows/rebills/refunds
	// and should not receive new default work.
	Role string `koanf:"role"`

	// --- NMI fields (type: nmi) ---
	SecurityKey     string `koanf:"security_key"`
	TokenizationKey string `koanf:"tokenization_key"`
	WebhookSecret   string `koanf:"webhook_secret"`

	// --- CCBill fields (type: ccbill) ---
	Salt             string `koanf:"salt"`
	ClientSubAcc     string `koanf:"client_sub_acc"`
	ClientAccNum     string `koanf:"client_acc_num"`
	DataLinkUsername string `koanf:"datalink_username"`
	DataLinkPassword string `koanf:"datalink_password"`
	// AllowedCIDRs is the CCBill webhook source allowlist (CIDR notation). When
	// empty, the documented default ranges are used. Supplying it via config/env
	// lets the ranges be rotated without a code deploy. Parsed at boot (fail-fast
	// on invalid entries) — see iputil.Configure.
	AllowedCIDRs []string `koanf:"allowed_cidrs"`

	// --- Stripe fields (type: stripe) ---
	SecretKey string `koanf:"secret_key"`
	// WebhookSecret is shared with NMI (same field name); for Stripe it is the
	// signing secret of the classic / "snapshot" event destination.
	// WebhookSecretThin is the signing secret of a Stripe "thin" Event
	// Destination pointed at the same webhook URL. When set, incoming Stripe
	// webhooks are verified against either secret, and thin payloads are
	// hydrated into the classic event shape before processing.
	WebhookSecretThin string `koanf:"webhook_secret_thin"`

	// --- Solana fields (type: solana) ---
	// There is no rpc_endpoint knob (#352): with a Helius key, Helius is the
	// primary RPC and the public endpoints are the fallback chain; without one,
	// the public chain alone serves. One key, zero endpoint plumbing.
	HeliusAPIKey string `koanf:"helius_api_key"`
	// Network is DERIVED from the test_env axis at startup (devnet under
	// test_env, mainnet otherwise) — it is deliberately NOT configurable
	// (#349): test_env already answers the question, and a second selector
	// could only contradict it.
	Network         string                 `koanf:"-"`
	RecipientWallet string                 `koanf:"recipient_wallet"`
	Tokens          map[string]TokenConfig `koanf:"tokens"`
	// SolanaPayRecurringSubscriptions advertises recurring Solana Pay v2
	// transaction-request support to browser clients. Keep disabled unless the
	// deployment has validated its target wallet set with merchant co-signed txs.
	SolanaPayRecurringSubscriptions bool `koanf:"solana_pay_recurring_subscriptions"`

	// PrivateKey is the merchant/cranker Solana signing keypair (base58) for a
	// SINGLE-TENANT install that configures Solana via global config rather than
	// the per-merchant secret store. At boot it is seeded into the default merchant's
	// secret store as solana/private_key (idempotently, never overwriting an
	// existing secret) so recurring Solana can sign (issue #253). Leave empty in
	// multi-merchant / Vault deployments, where each merchant supplies its own key.
	PrivateKey string `koanf:"private_key"`
}

// ProcessorSet is an in-memory set of payment-provider credential entries. It
// is not part of config.yaml/.env; private standalone installs seed provider
// credentials through merchant bootstrap/Vault state, and embedded hosts may
// pass a set programmatically during construction.
type ProcessorSet map[string]*ProcessorConfig

// GetEffectiveType returns the processor type, inferring from reserved names if needed.
func (p *ProcessorConfig) GetEffectiveType(name string) string {
	if p.Type != "" {
		return strings.ToLower(p.Type)
	}
	// Check if it's a reserved name
	normalizedName := strings.ToLower(strings.TrimSpace(name))
	if impliedType, ok := ReservedProcessorNames[normalizedName]; ok {
		return impliedType
	}
	return ""
}

// EffectiveRole returns primary when role is omitted, preserving the existing
// one-config-per-provider behavior.
func (p *ProcessorConfig) EffectiveRole() string {
	if p == nil {
		return ""
	}
	role := strings.ToLower(strings.TrimSpace(p.Role))
	if role == "" {
		return ProcessorRolePrimary
	}
	return role
}

// IsNMI returns true if this processor config is for an NMI-backed processor.
func (p *ProcessorConfig) IsNMI(name string) bool {
	return p.GetEffectiveType(name) == ProcessorTypeNMI
}

// IsCCBill returns true if this processor config is for CCBill.
func (p *ProcessorConfig) IsCCBill(name string) bool {
	return p.GetEffectiveType(name) == ProcessorTypeCCBill
}

// IsStripe returns true if this processor config is for Stripe.
func (p *ProcessorConfig) IsStripe(name string) bool {
	return p.GetEffectiveType(name) == ProcessorTypeStripe
}

// IsSolana returns true if this processor config is for Solana.
func (p *ProcessorConfig) IsSolana(name string) bool {
	return p.GetEffectiveType(name) == ProcessorTypeSolana
}

// ToNMIProviderSettings converts the processor config to NMI provider settings.
// Only valid for NMI-type processors.
func (p *ProcessorConfig) ToNMIProviderSettings(name string) *NMIProviderSettings {
	return &NMIProviderSettings{
		Name:            strings.ToLower(strings.TrimSpace(name)),
		SecurityKey:     p.SecurityKey,
		TokenizationKey: p.TokenizationKey,
		WebhookSecret:   p.WebhookSecret,
		TestMode:        false, // Will be set by caller based on test_env
	}
}

// ToCCBillConfig converts the processor config to CCBillConfig.
// Only valid for CCBill-type processors.
func (p *ProcessorConfig) ToCCBillConfig() *CCBillConfig {
	return &CCBillConfig{
		Salt:             p.Salt,
		ClientSubAcc:     p.ClientSubAcc,
		ClientAccNum:     p.ClientAccNum,
		DataLinkUsername: p.DataLinkUsername,
		DataLinkPassword: p.DataLinkPassword,
		AllowedCIDRs:     p.AllowedCIDRs,
		TestMode:         false, // Will be set by caller based on global test_mode
	}
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
// applications, JWKS/public keys, API keys, users, orgs, roles, and
// permissions are seeded as AuthKit state rather than trusted from runtime
// config.yaml/.env issuer allow-lists.
type AuthConfig struct {
	// HARDCUT (#312/#537): there is no `auth.operator_tenant_slug` /
	// `auth.operator_tenant_admin_roles`. Admin authority is live merchant-local
	// AuthKit org permission state (or a deployment-minted admin API key) -
	// deployment authority, not
	// membership in a separate "operator" AuthKit org. Load rejects the
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

// Operating modes (#346, #355) — see Config.Mode. The former mode=test is
// gone (sandbox is the orthogonal test_env axis) and "production" is renamed
// "full".
const (
	ModeFull     = "full"
	ModeLimited  = "limited"
	ModeReadOnly = "readonly"
)

// ValidModes contains all valid operating-mode values ("" = unset; dev only).
var ValidModes = map[string]bool{
	"":           true,
	ModeFull:     true,
	ModeLimited:  true,
	ModeReadOnly: true,
}

// SendGridConfig holds process-wide SendGrid API configuration. Sender/display
// metadata is merchant-scoped and loaded from merchant_configurations.
type SendGridConfig struct {
	APIKey string `koanf:"api_key"`
}

type USDCFundingConfig struct {
	Providers map[string]*USDCFundingProviderConfig `koanf:"providers,omitempty"`
}

type USDCFundingProviderConfig struct {
	Enabled           bool     `koanf:"enabled"`
	SupportedNetworks []string `koanf:"supported_networks,omitempty"`

	// LaunchURLTemplate is used for provider handoffs where the partner surface
	// exposes a configured URL/widget rather than a public session-create API.
	// Supported placeholders: {wallet}, {network}, {asset}, {amount},
	// {session_id}, {return_url}.
	LaunchURLTemplate string `koanf:"launch_url_template,omitempty"`

	// Coinbase-hosted Onramp session API configuration. Prefer APIKeyID +
	// APIKeySecret so OpenRails can generate the short-lived CDP JWT required for
	// server-to-server calls. APIKey is retained as an escape hatch for tests or
	// manually supplied short-lived bearer tokens; never expose these to host apps.
	APIBaseURL   string `koanf:"api_base_url,omitempty"`
	APIKeyID     string `koanf:"api_key_id,omitempty"`
	APIKeySecret string `koanf:"api_key_secret,omitempty"`
	APIKey       string `koanf:"api_key,omitempty"`
	// WebhookSecret is the Coinbase webhook subscription secret returned when
	// creating the Onramp webhook subscription. It verifies X-Hook0-Signature.
	WebhookSecret string `koanf:"webhook_secret,omitempty"`
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
	// Operating mode must be a known value — a typo'd mode (e.g. "redaonly")
	// must never silently boot with full behavior (#346).
	if !ValidModes[strings.ToLower(strings.TrimSpace(cfg.Mode))] {
		return fmt.Errorf("invalid mode %q: must be one of full, limited, readonly", cfg.Mode)
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
		// must never boot pointed at sandbox processors.
		if cfg.TestEnv {
			return fmt.Errorf("test_env=true is not allowed outside development")
		}
		// Outside development the operating mode must be declared explicitly —
		// "I forgot to set it" must never silently pick a behavior.
		switch cfg.GetMode() {
		case ModeFull, ModeLimited, ModeReadOnly:
		default:
			return fmt.Errorf("mode is required outside development: set mode (or env MODE) to one of full, limited, readonly")
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

// validateStripeKeyForTestEnv checks if the Stripe API key prefix matches the
// test_env axis. If there's a mismatch, it logs a warning and clears the key to
// disable Stripe. This prevents accidentally processing real charges in a test
// environment or test charges in a live one.
func ValidateProcessorSet(cfg *Config, processors ProcessorSet) error {
	if len(processors) == 0 {
		return nil
	}
	isDev := true
	if cfg != nil {
		isDev = cfg.IsDev()
	}
	if err := validateProcessors(cfg, processors, isDev); err != nil {
		return fmt.Errorf("processors validation failed: %w", err)
	}
	return validateStripeKeyForTestEnv(cfg, processors)
}

func validateStripeKeyForTestEnv(cfg *Config, processors ProcessorSet) error {
	if cfg == nil {
		cfg = &Config{}
	}
	for name, stripeProc := range processors {
		if stripeProc == nil || stripeProc.GetEffectiveType(name) != ProcessorTypeStripe {
			continue
		}

		secretKey := strings.TrimSpace(stripeProc.SecretKey)
		if secretKey == "" {
			continue
		}

		// Both standard secret keys (sk_*) and restricted keys (rk_*) carry the
		// live/test mode in their prefix, so classify either form.
		isLiveKey := strings.HasPrefix(secretKey, "sk_live_") || strings.HasPrefix(secretKey, "rk_live_")
		isTestKey := strings.HasPrefix(secretKey, "sk_test_") || strings.HasPrefix(secretKey, "rk_test_")

		if cfg.IsTestEnv() && isLiveKey {
			// Hard guarantee (#347): the sandbox environment must never hold a live
			// key — a mistakenly-test-enved production system would otherwise carry
			// a credential that can move real money.
			return fmt.Errorf("stripe processor %q: live key (sk_live_/rk_live_) is not allowed when test_env is enabled; use a test key or unset test_env", strings.ToLower(strings.TrimSpace(name)))
		}
		if !cfg.IsTestEnv() && isTestKey {
			log.Warnf("⚠️  Stripe test key provided for processor %q but test_env is disabled (live credentials) - disabling Stripe", strings.ToLower(strings.TrimSpace(name)))
			log.Warn("   Use a live-mode key (sk_live_/rk_live_), or set test_env=true for sandbox testing")
			stripeProc.SecretKey = ""
		}
	}
	return nil
}

// validateProcessors validates all processors in the new Processors map
func validateProcessors(cfg *Config, processors ProcessorSet, isDev bool) error {
	primaryByType := map[string]string{}
	for name, proc := range processors {
		if proc == nil {
			continue
		}
		switch role := proc.EffectiveRole(); role {
		case ProcessorRolePrimary, ProcessorRoleSecondary, ProcessorRoleLegacy:
		default:
			return fmt.Errorf("processor '%s' has unknown role '%s'", name, proc.Role)
		}

		effectiveType := proc.GetEffectiveType(name)
		if proc.EffectiveRole() == ProcessorRolePrimary {
			if existing := primaryByType[effectiveType]; existing != "" {
				return fmt.Errorf("multiple primary processors configured for type %q: %q and %q", effectiveType, existing, strings.ToLower(strings.TrimSpace(name)))
			}
			primaryByType[effectiveType] = strings.ToLower(strings.TrimSpace(name))
		}
		switch effectiveType {
		case ProcessorTypeNMI:
			if err := validateNMIProcessor(name, proc, isDev); err != nil {
				return err
			}
		case ProcessorTypeCCBill:
			if err := validateCCBillProcessor(name, proc, isDev); err != nil {
				return err
			}
		case ProcessorTypeStripe:
			if err := validateStripeProcessor(name, proc, isDev); err != nil {
				return err
			}
		case ProcessorTypeSolana:
			if err := validateSolanaProcessor(name, proc, isDev); err != nil {
				return err
			}
		default:
			return fmt.Errorf("processor '%s' has unknown type '%s'", name, effectiveType)
		}
	}
	return nil
}

// validateNMIProcessor validates an NMI-type processor
func validateNMIProcessor(name string, proc *ProcessorConfig, isDev bool) error {
	if isDev {
		return nil // Skip strict validation in dev
	}

	if strings.TrimSpace(proc.SecurityKey) == "" {
		return fmt.Errorf("processor '%s' (nmi): security_key is required", name)
	}

	if strings.TrimSpace(proc.WebhookSecret) == "" {
		return fmt.Errorf("processor '%s' (nmi): webhook_secret is required outside development (signature verification cannot be disabled in production)", name)
	}

	return nil
}

// validateCCBillProcessor validates a CCBill-type processor
func validateCCBillProcessor(name string, proc *ProcessorConfig, isDev bool) error {
	if isDev {
		return nil // Skip strict validation in dev
	}

	if strings.TrimSpace(proc.ClientAccNum) == "" {
		return fmt.Errorf("processor '%s' (ccbill): client_acc_num is required", name)
	}

	if strings.TrimSpace(proc.ClientSubAcc) == "" {
		return fmt.Errorf("processor '%s' (ccbill): client_sub_acc is required", name)
	}

	// DataLink credentials: either both or neither
	hasUsername := strings.TrimSpace(proc.DataLinkUsername) != ""
	hasPassword := strings.TrimSpace(proc.DataLinkPassword) != ""
	if hasUsername != hasPassword {
		return fmt.Errorf("processor '%s' (ccbill): both datalink_username and datalink_password must be provided when configuring DataLink", name)
	}

	return nil
}

// validateStripeProcessor validates a Stripe-type processor
func validateStripeProcessor(name string, proc *ProcessorConfig, isDev bool) error {
	if strings.TrimSpace(proc.SecretKey) == "" {
		log.Warnf("processor '%s' (stripe): secret_key not configured; checkout unavailable", name)
	}

	if strings.TrimSpace(proc.WebhookSecret) == "" {
		if !isDev {
			return fmt.Errorf("processor '%s' (stripe): webhook_secret is required outside development (signature verification cannot be disabled in production)", name)
		}
		log.Warnf("processor '%s' (stripe): webhook_secret not configured; signature verification disabled", name)
	}

	return nil
}

// validateSolanaProcessor validates only config-loading concerns. Solana token
// pricing/default policy belongs to internal/modules/solana/tokens and is
// applied at runtime by configureSolanaProcessor.
func validateSolanaProcessor(name string, proc *ProcessorConfig, isDev bool) error {
	if strings.TrimSpace(proc.RecipientWallet) == "" {
		if !isDev {
			return fmt.Errorf("processor '%s' (solana): recipient_wallet is required outside development (Solana payments cannot be processed without it)", name)
		}
		log.Warnf("processor '%s' (solana): recipient_wallet not configured; Solana payments disabled", name)
	}

	return nil
}

// GetNMIProcessors returns all NMI-backed processor configs.
func (set ProcessorSet) GetNMIProcessors() map[string]*ProcessorConfig {
	result := make(map[string]*ProcessorConfig)
	if set == nil {
		return result
	}

	for name, proc := range set {
		if proc != nil && proc.IsNMI(name) {
			result[strings.ToLower(name)] = proc
		}
	}

	return result
}

// ProcessorKeysByType returns configured processor names for the provider type,
// sorted for deterministic diagnostics and selection.
func (set ProcessorSet) ProcessorKeysByType(providerType string) []string {
	if set == nil {
		return nil
	}
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	keys := make([]string, 0)
	for name, proc := range set {
		if proc == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(name))
		if key != "" && proc.GetEffectiveType(key) == providerType {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// PrimaryProcessorByType returns the configured primary credential set for a
// provider type. Existing configs with one entry and no role keep working
// because an empty role is primary. Multiple primaries are a configuration
// error: OpenRails cannot guess where default new work should route.
func (set ProcessorSet) PrimaryProcessorByType(providerType string) (string, *ProcessorConfig, error) {
	keys := set.ProcessorKeysByType(providerType)
	var (
		primaryKey string
		primary    *ProcessorConfig
	)
	for _, key := range keys {
		proc := set[key]
		if proc.EffectiveRole() != ProcessorRolePrimary {
			continue
		}
		if primary != nil {
			return "", nil, fmt.Errorf("multiple primary processors configured for type %q: %q and %q", providerType, primaryKey, key)
		}
		primaryKey, primary = key, proc
	}
	return primaryKey, primary, nil
}

// GetCCBillProcessor returns the configured primary CCBill processor.
func (set ProcessorSet) GetCCBillProcessor() *ProcessorConfig {
	_, proc, _ := set.PrimaryProcessorByType(ProcessorTypeCCBill)
	return proc
}

// GetStripeProcessor returns the configured primary Stripe processor.
func (set ProcessorSet) GetStripeProcessor() *ProcessorConfig {
	_, proc, _ := set.PrimaryProcessorByType(ProcessorTypeStripe)
	return proc
}

// GetSolanaProcessor returns the configured primary Solana processor.
func (set ProcessorSet) GetSolanaProcessor() *ProcessorConfig {
	_, proc, _ := set.PrimaryProcessorByType(ProcessorTypeSolana)
	return proc
}

// GetProcessor returns a processor config by name.
func (set ProcessorSet) GetProcessor(name string) *ProcessorConfig {
	if set == nil {
		return nil
	}
	normalizedName := strings.ToLower(strings.TrimSpace(name))
	if proc, ok := set[normalizedName]; ok && proc != nil {
		return proc
	}
	return nil
}

// GetProcessorType returns the type of a processor by name.
// Returns empty string if processor not found.
func (set ProcessorSet) GetProcessorType(name string) string {
	proc := set.GetProcessor(name)
	if proc == nil {
		return ""
	}
	return proc.GetEffectiveType(name)
}

// IsNMIProcessor returns true if the named processor is NMI-backed.
func (set ProcessorSet) IsNMIProcessor(name string) bool {
	return set.GetProcessorType(name) == ProcessorTypeNMI
}

// GetMode returns the normalized operating mode ("" when unset).
// Unknown values are rejected by Validate; here they normalize to "" so a
// pre-validation caller never misreads a typo as a real mode. Unset means the
// dev default (full behavior) — that only ever serves development, because
// Validate requires an explicit mode outside it.
func (cfg *Config) GetMode() string {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if !ValidModes[mode] {
		return ""
	}
	return mode
}

// IsTestEnv returns true if payment processors should use sandbox/test
// environments and the sandbox-credential guarantees apply (#355): Stripe
// live-key refusal, NMI boot probe, CCBill sandbox URL, Solana devnet.
// Orthogonal to Mode (pure behavior). When unset, Load defaults it to sandbox
// in development and live outside it (#355); Validate rejects test_env=true
// outside development.
func (cfg *Config) IsTestEnv() bool {
	return cfg.TestEnv
}

// IsLimitedMode returns true if proactive payment-provider operations
// (dunning charges/cancellations, auto-top-ups, arrears collection, Solana
// pulls, catalog provider-object writes) are disabled, leaving only reactive,
// user-initiated operations. True in limited and readonly modes.
func (cfg *Config) IsLimitedMode() bool {
	mode := cfg.GetMode()
	return mode == ModeLimited || mode == ModeReadOnly
}

// IsProviderReadOnly returns true if EVERY provider write — even reactive,
// user-initiated ones — must be blocked (mode=readonly). Reads (query APIs,
// verification) stay allowed.
func (cfg *Config) IsProviderReadOnly() bool {
	return cfg.GetMode() == ModeReadOnly
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
				// Per source IP. All webhooks from a processor share one bucket
				// (fixed processor IPs), so this must absorb rebill runs / event
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

		// Special case: TEST_ENV -> test_env (top-level bool, not nested test.env)
		if s == "test_env" {
			return "test_env"
		}

		// Canonical Vault env vars: VAULT_ADDR is HashiCorp's standard name for
		// the server URL, so map it to vault.address (the default first-underscore
		// split would yield vault.addr). VAULT_TOKEN already splits correctly.
		if s == "vault_addr" {
			return "vault.address"
		}

		// USDC_FUNDING_PROVIDERS_<NAME>_<FIELD> -> usdc_funding.providers.<name>.<field>
		if strings.HasPrefix(s, "usdc_funding_providers_") {
			rest := strings.TrimPrefix(s, "usdc_funding_providers_")
			parts := strings.SplitN(rest, "_", 2)
			if len(parts) == 2 {
				return fmt.Sprintf("usdc_funding.providers.%s.%s", parts[0], parts[1])
			}
		}

		// Replace only the first underscore for other nested config keys.
		if !strings.Contains(s, "_") {
			return s
		}
		return strings.Replace(s, "_", ".", 1)
	}

	envCallbackWithValue := func(key string, value string) (string, interface{}) {
		upperKey := strings.ToUpper(key)
		if upperKey == "MERCHANT" || upperKey == "AUTH_ISSUERS" || upperKey == "CORS_ORIGINS" || strings.HasPrefix(upperKey, "PROCESSORS_") || strings.HasPrefix(upperKey, "STORE_") {
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
		return nil, fmt.Errorf("merchant_cors was removed (#519): configure browser origins on AuthKit remote_application.allowed_origins")
	}
	if k.Exists("billing_hot_path") || os.Getenv("OPENRAILS_BILLING_HOT_PATH_FAIL_POLICY") != "" || os.Getenv("BILLING_HOT_PATH_FAIL_POLICY") != "" {
		return nil, fmt.Errorf("billing_hot_path was removed: OpenRails does not enforce client degraded-mode policy; configure any fail-open/fail-closed behavior in the calling client")
	}
	if k.Exists("test_mode") || os.Getenv("TEST_MODE") != "" {
		return nil, fmt.Errorf("test_mode was renamed to test_env (#355): set test_env (env TEST_ENV) — sandbox vs live is the test_env axis; a stale test_mode would silently no-op")
	}

	ignoredPrivatePort := k.Exists("private_port") || os.Getenv("PRIVATE_PORT") != ""
	ignoredStoreConfig := k.Exists("store") || hasEnvPrefix("STORE_")
	ignoredMerchantConfig := k.Exists("merchant") || os.Getenv("MERCHANT") != ""
	ignoredCORSConfig := k.Exists("cors_origins") || os.Getenv("CORS_ORIGINS") != ""
	ignoredDBRequireRLS := k.Exists("db.require_rls") || os.Getenv("DB_REQUIRE_RLS") != ""
	ignoredAuthIssuers := k.Exists("auth.issuers") || k.Exists("auth.expected_audience") ||
		os.Getenv("AUTH_ISSUERS") != "" || os.Getenv("AUTH_EXPECTED_AUDIENCE") != ""
	ignoredProcessors := k.Exists("processors") || hasEnvPrefix("PROCESSORS_")
	ignoredControlPlaneLegacy := k.Exists("auth.control_plane.issuer") ||
		k.Exists("auth.control_plane.issued_audiences") ||
		k.Exists("auth.control_plane.expected_audiences") ||
		k.Exists("auth.control_plane.public_user_registration") ||
		k.Exists("auth.control_plane.public_tenant_registration") ||
		k.Exists("auth.control_plane.public_hosted") ||
		k.Exists("auth.control_plane.token_prefix") ||
		k.Exists("auth.control_plane.platform_org_slug") ||
		k.Exists("auth.control_plane.platform_admin_user_id") ||
		os.Getenv("AUTH_CONTROL_PLANE_ISSUER") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_ISSUED_AUDIENCES") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_EXPECTED_AUDIENCES") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PUBLIC_USER_REGISTRATION") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PUBLIC_TENANT_REGISTRATION") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PUBLIC_HOSTED") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_TOKEN_PREFIX") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_BOOTSTRAP_ADMIN_SERVICE_TOKEN_NAME") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PLATFORM_ORG_SLUG") != "" ||
		os.Getenv("AUTH_CONTROL_PLANE_PLATFORM_ADMIN_USER_ID") != ""
	if ignoredStoreConfig {
		log.Warn("ignoring retired store config (#520): seed merchant profile fields with openrails push-merchant-config under merchants[].profile")
	}
	if ignoredMerchantConfig {
		log.Warn("ignoring retired merchant config (#520/#521): seed merchants with openrails push-merchant-config; standalone no longer pins a process-wide merchant")
	}
	if ignoredCORSConfig {
		log.Warn("ignoring retired cors_origins config (#519): browser CORS origins come from AuthKit remote_application.allowed_origins")
	}
	if ignoredDBRequireRLS {
		log.Warn("ignoring retired db.require_rls config: RLS enforcement is derived from env; development may bypass RLS, every other env requires an RLS-enforcing DB role")
	}
	if ignoredAuthIssuers {
		log.Warn("ignoring retired auth issuer/audience config (#521/#527): declare each merchant's host-app issuer inline under merchants[].issuer in the bootstrap manifest (registered as the merchant org owner)")
	}
	if ignoredProcessors {
		log.Warn("ignoring retired processors config (#521): seed merchant provider_accounts and secrets with openrails push-merchant-config under merchants[].provider_accounts")
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
	// Sandbox-by-default in development (#355): when test_env is not explicitly
	// provided, a dev-like boot defaults to sandbox credentials so a local run
	// can never accidentally move real money against live processor credentials
	// — the dangerous case is silent ("forgot to set it"), so the safe value is
	// the one you get by omission. Outside development the default stays live
	// (false), and an explicit test_env=true is still rejected by Validate: prod
	// opts into live only by omission and can never opt into sandbox. To run
	// live locally, set test_env=false explicitly (env TEST_ENV=false, flag
	// --test-env=false). This only governs standalone Load(); embedded hosts
	// build the Config programmatically and supply their own value.
	if !k.Exists("test_env") {
		cfg.TestEnv = cfg.IsDev()
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
	logTestEnvStatus(cfg)
	logOperatingModeStatus(cfg)
}

func logOperatingModeStatus(cfg *Config) {
	switch cfg.GetMode() {
	case ModeReadOnly:
		log.Warn("⚠️  MODE=readonly - ZERO payment-provider writes; even user-initiated charges fail loudly")
		log.Info("   Reconciliation/forensics posture: provider reads + local serving only")
		log.Info("   Set mode=limited or mode=full to allow writes")
	case ModeLimited:
		log.Warn("⚠️  MODE=limited - No proactive payment-provider operations will be performed")
		log.Info("   Dunning charges/cancellations, auto-top-ups, arrears collection, Solana pulls and catalog provider writes are paused")
		log.Info("   Reactive operations (checkout, vault saves, user/admin cancels, webhooks) work normally")
		log.Info("   Set mode=full to resume proactive operations")
	case ModeFull:
		log.Info("Mode: full (complete behavior - charges, dunning, deletes all run)")
	}
}

// logTestEnvStatus logs the credential environment at startup.
// This helps operators confirm whether they're on sandbox or live credentials.
func logTestEnvStatus(cfg *Config) {
	if cfg.IsTestEnv() {
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
		// TEST_ENV=false is set explicitly; omitted test_env defaults to sandbox in dev.
		if cfg.IsDev() {
			log.Warn("⚠️  Real payment processing enabled in dev environment")
			log.Warn("   Set test_env=true (env TEST_ENV=true, flag --test-env) to use sandbox environments")
		}
	}
}
