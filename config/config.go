package config

import (
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

const EnvProd string = "prod"
const EnvDev string = "dev"

const ConfigContextKey string = "config"

// DefaultLogoURL is a simple billing/payment icon (white dollar sign on purple circle)
// SVG: <svg xmlns="http://www.w3.org/2000/svg" width="64" height="64" viewBox="0 0 64 64">
//
//	<circle cx="32" cy="32" r="30" fill="#9945FF"/>
//	<text x="32" y="44" font-family="Arial" font-size="36" font-weight="bold" fill="white" text-anchor="middle">$</text>
//	</svg>
const DefaultLogoURL = "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSI2NCIgaGVpZ2h0PSI2NCIgdmlld0JveD0iMCAwIDY0IDY0Ij48Y2lyY2xlIGN4PSIzMiIgY3k9IjMyIiByPSIzMCIgZmlsbD0iIzk5NDVGRiIvPjx0ZXh0IHg9IjMyIiB5PSI0NCIgZm9udC1mYW1pbHk9IkFyaWFsIiBmb250LXNpemU9IjM2IiBmb250LXdlaWdodD0iYm9sZCIgZmlsbD0id2hpdGUiIHRleHQtYW5jaG9yPSJtaWRkbGUiPiQ8L3RleHQ+PC9zdmc+"

// StoreConfig holds merchant/store branding configuration.
// Used across the system for consistent branding (Solana Pay, emails, etc.)
type StoreConfig struct {
	// Name is the merchant/store name displayed to customers (e.g., in Solana Pay QR codes, emails)
	Name string `koanf:"name"`
	// LogoURL is the URL to the store logo/icon (used in Solana Pay QR codes, etc.)
	// Must be an absolute HTTPS URL to an SVG, PNG, or WebP image
	LogoURL string `koanf:"logo_url"`
	// FromEmail is the sender email address for all outgoing emails (receipts, notifications, etc.)
	// Example: "noreply@mystore.com" or "billing@mystore.com"
	FromEmail string `koanf:"from_email"`
	// CustomerPortalURL is the customer-facing URL where users can manage billing settings
	// (e.g., update payment method, manage subscription).
	CustomerPortalURL string `koanf:"customer_portal_url"`
}

type Config struct {
	Env  string       `koanf:"env,omitempty"`
	Port FlexiblePort `koanf:"port,omitempty"` // Standalone only: public HTTP port (default 2053)
	Host string       `koanf:"host,omitempty"` // Standalone only: address to bind to (default 0.0.0.0)

	// Mode is the single operating-mode dial (#346): how much OpenRails is
	// allowed to do against the payment providers. One of:
	//   - "test":       sandbox processors, FULL behavior (charges, dunning,
	//                   deletes all run — no real money moves)
	//   - "production": live processors, full behavior
	//   - "limited":    live processors, REACTIVE-ONLY — no system-initiated
	//                   provider actions (no dunning charges or window-expiry
	//                   cancellations, no auto-top-ups, no arrears collection,
	//                   no Solana pulls, no catalog provider-object writes).
	//                   User/admin-initiated operations work normally,
	//                   including their processor-side deletes.
	//   - "readonly":   live credentials, ZERO provider writes — even reactive
	//                   ones fail loudly. For reconciliation/forensics boots.
	//                   Implies limited + the processor-deletion kill switch.
	// Unset defaults to "test" in development; outside development an explicit
	// mode is REQUIRED (Validate refuses to boot without one). Feature flags
	// remain fine-grained dials on top; the strictest setting always wins.
	Mode string `koanf:"mode,omitempty"`

	// APIURL is the base URL where billing's versioned routes are mounted.
	// Used for generating URLs (e.g., Solana Pay transaction_request URLs).
	//
	// Standalone mode: "https://api.mysite.com" (routes at /v1/*)
	// Embedded mode:   "https://api.mysite.com/billing" (routes at /billing/v1/*)
	//
	// Formula: generated_url = APIURL + {version_path} + "/checkout/:id/solana-pay"
	APIURL string       `koanf:"api_url,omitempty"`
	Store  *StoreConfig `koanf:"store,omitempty"`

	// Processors is the unified configuration for all payment processors.
	// Each key is the processor name (e.g., "mobius", "ccbill", "stripe", "solana").
	// Reserved names (ccbill, stripe, solana) don't need explicit "type" field.
	// Non-reserved names (e.g., "mobius", "acme") require "type: nmi".
	//
	// Example:
	//   processors:
	//     mobius:
	//       type: nmi
	//       security_key: "..."
	//     ccbill:
	//       client_acc_num: "..."
	//     stripe:
	//       secret_key: "sk_..."
	//     solana:
	//       recipient_wallet: "..."
	Processors map[string]*ProcessorConfig `koanf:"processors,omitempty"`

	// USDCFunding configures external wallet-funding providers. These providers
	// only move USDC into the user's self-custody wallet; OpenRails checkout still
	// collects payment from that wallet afterwards.
	USDCFunding *USDCFundingConfig `koanf:"usdc_funding,omitempty"`

	DB          *DBConfig         `koanf:"db,omitempty"`
	Redis       *RedisConfig      `koanf:"redis,omitempty"`
	Auth        *AuthConfig       `koanf:"auth,omitempty"`
	ClickHouse  *ClickHouseConfig `koanf:"clickhouse,omitempty"`
	Logger      *LoggerConfig     `koanf:"logger,omitempty"`
	SendGrid    *SendGridConfig   `koanf:"sendgrid,omitempty"`
	Pyth        *PythConfig       `koanf:"pyth,omitempty"`
	CorsOrigins []string          `koanf:"cors_origins,omitempty"`
	// TenantCORS configures per-tenant browser-direct allowed origins (issue #222
	// browser tier). It is keyed by tenant slug; each entry lists the exact
	// origins a browser on that tenant's domain may use to call OpenRails
	// directly (e.g. the /v1/self/* self-service surface with a delegated access
	// token). These origins are added to the allow-list ALONGSIDE CorsOrigins —
	// they are an additive union, never a wildcard. Preflight succeeds for any
	// listed origin and is denied for any origin that is not listed in either
	// CorsOrigins or some tenant's allowed origins.
	TenantCORS   map[string]*TenantCORSConfig `koanf:"tenant_cors,omitempty"`
	RateLimits   *RateLimitsConfig            `koanf:"rate_limits,omitempty"`
	Captcha      *CaptchaConfig               `koanf:"captcha,omitempty"`
	FeatureFlags *FeatureFlags                `koanf:"feature_flags,omitempty"`
	Encryption   *EncryptionConfig            `koanf:"encryption,omitempty"`
	Vault        *VaultConfig                 `koanf:"vault,omitempty"`
	// BillingHotPath configures the degraded-mode behavior of the per-invocation
	// billing authorize call (issue #248). EXPLICIT, never a silent default.
	BillingHotPath *BillingHotPathConfig `koanf:"billing_hot_path,omitempty"`
}

// Billing hot-path fail policies (issue #248). The per-invocation authorize/hold
// call is a NETWORK dependency on every generation; when OpenRails is unreachable
// or slow, the client (gen-orchestrator) must apply an EXPLICIT, documented
// policy — never an accidental default.
const (
	// BillingFailClosed denies the invocation when authorize cannot be reached
	// (no hold => no run). The safe default for arrears/untrusted payers.
	BillingFailClosed = "fail_closed"
	// BillingFailOpen permits the invocation when authorize cannot be reached,
	// with deferred settlement reconciled on recovery. ONLY for TRUSTED prepaid
	// payers, bounded/capped per payer per outage window (the cap + replay live in
	// the gen-orchestrator client, Wave 3).
	BillingFailOpen = "fail_open"
)

// BillingHotPathConfig is the OpenRails-side declaration of the billing hot-path
// degraded-mode contract (issue #248). The per-invocation hold call is a network
// dependency on every generation; this knob makes the fail policy EXPLICIT.
//
// CONTRACT: the ENFORCEMENT (short timeout + circuit breaker + bounded fail-open
// cap + deferred-hold replay) lives in the gen-orchestrator CLIENT (Wave 3). This
// config is the documented source of truth the client reads so the policy is
// never an accidental default:
//
//   - fail_closed (DEFAULT): authorize unreachable/slow => DENY the run. No hold,
//     no run. Required for arrears/untrusted payers.
//   - fail_open: authorize unreachable => PERMIT the run for TRUSTED prepaid
//     payers only, bounded per payer per outage window, with deferred holds
//     queued and replayed idempotently on recovery (reconciliation, #243, is the
//     backstop). NEVER silently grant unbounded free usage.
type BillingHotPathConfig struct {
	// FailPolicy selects the degraded-mode behavior: "fail_closed" (default) or
	// "fail_open". Validated at config load (config.Validate).
	FailPolicy string `koanf:"fail_policy,omitempty"`
}

// EffectiveFailPolicy returns the configured fail policy, defaulting to
// fail_closed (the safe default) when unset.
func (c *BillingHotPathConfig) EffectiveFailPolicy() string {
	if c == nil {
		return BillingFailClosed
	}
	p := strings.ToLower(strings.TrimSpace(c.FailPolicy))
	if p == "" {
		return BillingFailClosed
	}
	return p
}

// EncryptionConfig configures per-tenant encryption-at-rest (issue #227). The
// master key wraps each tenant's Data Encryption Key (envelope encryption); the
// DEK encrypts sensitive at-rest field values (e.g. per-tenant processor
// credentials in billing.tenant_secrets).
//
// Self-hosted / dev: supply MasterKey (base64 of 32 raw bytes) via config or the
// ENCRYPTION_MASTER_KEY env var. PRODUCTION: the master key should come from a
// KMS (the wrapped DEKs in billing.tenant_deks stay in the DB; the master key
// that unwraps them never does). When MasterKey is empty, encryption is disabled
// and values are stored in plaintext (back-compat with pre-#227 deployments).
type EncryptionConfig struct {
	// MasterKey is the base64-encoded 32-byte AES-256 master key that wraps
	// per-tenant DEKs. Empty disables at-rest encryption.
	MasterKey string `koanf:"master_key,omitempty"`
}

// VaultConfig selects a HashiCorp Vault backend for per-tenant secrets (issue
// #251). When Enabled, the tenant secret store resolves to Vault KV-v2 (same
// (tenant, name) addressing) instead of DB+envelope, and Solana signing can use
// Vault Transit (the key never leaves Vault). Disabled by default; self-hosted
// uses the DB+envelope store.
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
	// KVMount is the KV-v2 mount for tenant secrets (default "secret").
	KVMount string `koanf:"kv_mount,omitempty"`
	// TransitMount is the Transit mount for tenant signing keys (default "transit").
	TransitMount string `koanf:"transit_mount,omitempty"`
	// UseTransitForSolana signs the per-tenant Solana key via Vault Transit
	// (non-extractable) rather than fetching solana/private_key from KV.
	UseTransitForSolana bool `koanf:"use_transit_for_solana,omitempty"`
	// SecretCacheTTLSeconds is the in-process per-(tenant,name) secret cache TTL.
	// Workers/handlers resolve a tenant's secret once per TTL window instead of
	// hitting the backend per row/request. 0 uses the default (15m); negative
	// disables caching. A rotation (Put/Delete in this process) invalidates the
	// entry immediately; cross-process rotations take effect within one TTL.
	SecretCacheTTLSeconds int `koanf:"secret_cache_ttl_seconds,omitempty"`
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

	// Schema is the Postgres schema OpenRails owns (issue #165). It is used for
	// OpenRails' own DDL/DML (billing tables) and, in STANDALONE mode, for the
	// River job-queue tables as well — i.e. standalone River schema == DB.Schema.
	//
	// Default: "billing" (zero-config; preserves historical behavior). Configure
	// via config `db.schema` or env `DB_SCHEMA`.
	//
	// Embedded/library mode: the host controls where River tables live by injecting
	// its own River client (embedded.SetRiverClient); that client owns its schema and
	// OpenRails never overrides it. See pkg/embedded for the full schema contract.
	//
	// Read the effective value via DBConfig.SchemaName() (it applies the default and
	// normalization). Do not read this field directly.
	Schema string `koanf:"schema"`

	// RequireRLS makes startup FAIL if the connected role does not enforce Row
	// Level Security (i.e. it is a superuser or a BYPASSRLS role). Set this in
	// managed multi-tenant deployments, where the app MUST connect as the
	// unprivileged openrails_app role (created by 001_schema.up.sql) so the per-tenant RLS
	// policies actually constrain queries (issue #227). Left false for self-hosted
	// single-tenant deployments, where RLS is a backstop and running as a
	// privileged role is acceptable.
	RequireRLS bool `koanf:"require_rls"`
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

// DefaultSchema is the Postgres schema OpenRails uses when none is configured.
// It preserves historical (pre-#165) behavior where everything lived in `billing`.
const DefaultSchema = "billing"

// schemaIdentRe restricts the OpenRails schema to a safe SQL identifier: it must
// start with a letter or underscore and contain only letters, digits, and
// underscores. This forbids quotes, spaces, and dots, so the value can be used to
// build search_path / River schema names without quoting hazards.
var schemaIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SchemaName returns the effective OpenRails Postgres schema (issue #165),
// applying the `billing` default and normalization (trim + lower-case). All
// OpenRails code that needs the schema (migrator, River client construction)
// MUST go through this accessor rather than reading DBConfig.Schema directly or
// hardcoding "billing".
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

type StripeConfig struct {
	SecretKey     string `koanf:"secret_key"`
	WebhookSecret string `koanf:"webhook_secret"`
	SuccessURL    string `koanf:"success_url"`
	CancelURL     string `koanf:"cancel_url"`
}

// ProcessorType constants for the unified processor config
const (
	ProcessorTypeNMI    = "nmi"
	ProcessorTypeCCBill = "ccbill"
	ProcessorTypeStripe = "stripe"
	ProcessorTypeSolana = "solana"
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
//   - type: stripe  → Stripe fields (secret_key, webhook_secret, success_url, cancel_url)
//   - type: solana  → Solana fields (recipient_wallet, rpc_endpoint, etc.)
//
// Reserved names (ccbill, stripe, solana) don't need explicit type - it's implied.
// Non-reserved names (e.g., "acme") require type: nmi.
type ProcessorConfig struct {
	// Type specifies the processor type: "nmi", "ccbill", "stripe", "solana"
	// Required for non-reserved processor names.
	// For reserved names (ccbill, stripe, solana), type is inferred from the name.
	Type string `koanf:"type"`

	// --- NMI fields (type: nmi) ---
	SecurityKey     string `koanf:"security_key"`
	TokenizationKey string `koanf:"tokenization_key"`
	// TokenizationURL is the Collect.js script URL used client-side for tokenization (e.g., https://secure.networkmerchants.com/token/Collect.js).
	// Billing does not fetch this URL; it is intended for configuration parity and sandbox experimentation.
	TokenizationURL string `koanf:"tokenization_url"`
	WebhookSecret   string `koanf:"webhook_secret"`

	// --- CCBill fields (type: ccbill) ---
	Salt               string `koanf:"salt"`
	ClientSubAcc       string `koanf:"client_sub_acc"`
	ClientAccNum       string `koanf:"client_acc_num"`
	SubscriptionTypeId string `koanf:"subscription_type_id"`
	DataLinkUsername   string `koanf:"datalink_username"`
	DataLinkPassword   string `koanf:"datalink_password"`

	// --- Stripe fields (type: stripe) ---
	SecretKey  string `koanf:"secret_key"`
	SuccessURL string `koanf:"success_url"`
	CancelURL  string `koanf:"cancel_url"`
	// WebhookSecret is shared with NMI (same field name); for Stripe it is the
	// signing secret of the classic / "snapshot" event destination.
	// WebhookSecretThin is the signing secret of a Stripe "thin" Event
	// Destination pointed at the same webhook URL. When set, incoming Stripe
	// webhooks are verified against either secret, and thin payloads are
	// hydrated into the classic event shape before processing.
	WebhookSecretThin string `koanf:"webhook_secret_thin"`

	// --- Solana fields (type: solana) ---
	RPCEndpoint  string `koanf:"rpc_endpoint"`
	HeliusAPIKey string `koanf:"helius_api_key"`
	// Network is DERIVED from the operating mode at startup (devnet under test
	// mode, mainnet otherwise) — it is deliberately NOT configurable (#349):
	// the mode already answers the question, and a second selector could only
	// contradict it.
	Network         string                 `koanf:"-"`
	RecipientWallet string                 `koanf:"recipient_wallet"`
	Tokens          map[string]TokenConfig `koanf:"tokens"`
	// SolanaPayRecurringSubscriptions advertises recurring Solana Pay v2
	// transaction-request support to browser clients. Keep disabled unless the
	// deployment has validated its target wallet set with merchant co-signed txs.
	SolanaPayRecurringSubscriptions bool `koanf:"solana_pay_recurring_subscriptions"`

	// PrivateKey is the merchant/cranker Solana signing keypair (base58) for a
	// SINGLE-TENANT install that configures Solana via global config rather than
	// the per-tenant secret store. At boot it is seeded into the default tenant's
	// secret store as solana/private_key (idempotently, never overwriting an
	// existing secret) so recurring Solana can sign (issue #253). Leave empty in
	// multi-tenant / Vault deployments, where each tenant supplies its own key.
	PrivateKey string `koanf:"private_key"`
}

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
		TestMode:        false, // Will be set by caller based on the operating mode
	}
}

// ToCCBillConfig converts the processor config to CCBillConfig.
// Only valid for CCBill-type processors.
func (p *ProcessorConfig) ToCCBillConfig() *CCBillConfig {
	return &CCBillConfig{
		Salt:               p.Salt,
		ClientSubAcc:       p.ClientSubAcc,
		ClientAccNum:       p.ClientAccNum,
		SubscriptionTypeId: p.SubscriptionTypeId,
		DataLinkUsername:   p.DataLinkUsername,
		DataLinkPassword:   p.DataLinkPassword,
		TestMode:           false, // Will be set by caller based on the operating mode
	}
}

// ToStripeConfig converts the processor config to StripeConfig.
// Only valid for Stripe-type processors.
func (p *ProcessorConfig) ToStripeConfig() *StripeConfig {
	return &StripeConfig{
		SecretKey:     p.SecretKey,
		WebhookSecret: p.WebhookSecret,
		SuccessURL:    p.SuccessURL,
		CancelURL:     p.CancelURL,
	}
}

// ToSolanaConfig converts the processor config to SolanaConfig.
// Only valid for Solana-type processors.
func (p *ProcessorConfig) ToSolanaConfig() *SolanaConfig {
	return &SolanaConfig{
		RPCEndpoint:                     p.RPCEndpoint,
		HeliusAPIKey:                    p.HeliusAPIKey,
		Network:                         p.Network,
		RecipientWallet:                 p.RecipientWallet,
		Tokens:                          p.Tokens,
		SolanaPayRecurringSubscriptions: p.SolanaPayRecurringSubscriptions,
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
	Salt               string `koanf:"salt"`
	ClientSubAcc       string `koanf:"client_sub_acc"`
	ClientAccNum       string `koanf:"client_acc_num"`
	SubscriptionTypeId string `koanf:"subscription_type_id"`
	TestMode           bool   `koanf:"test_mode"`

	DataLinkUsername string `koanf:"datalink_username"`
	DataLinkPassword string `koanf:"datalink_password"`
}

type RedisConfig struct {
	Addr     string `koanf:"addr"`
	Password string `koanf:"password"`
	DB       int    `koanf:"db"`
}

// AuthConfig holds JWT verification configuration for OpenRails.
// Billing is a JWT verifier (not issuer) - it validates tokens issued by your IdP.
// TenantCORSConfig holds the browser-direct CORS policy for a single tenant
// (issue #222 browser tier).
type TenantCORSConfig struct {
	// AllowedOrigins is the exact set of browser origins (scheme+host+port) on
	// this tenant's domain that may call OpenRails directly. Exact-match only; no
	// wildcards. Example: ["https://app.example.com", "https://portal.example.com"].
	AllowedOrigins []string `koanf:"allowed_origins,omitempty"`
}

// AllowedCORSOrigins returns the de-duplicated union of the global CorsOrigins
// and every tenant's per-tenant allowed origins (issue #222 browser tier). This
// is the explicit allow-list handed to the CORS middleware so browsers on a
// configured tenant domain can call OpenRails directly without weakening CORS to
// a wildcard. Order is stable (global origins first, then tenant origins in
// tenant-slug order) so the allow-list is deterministic.
func (c *Config) AllowedCORSOrigins() []string {
	if c == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(o string) {
		o = strings.TrimSpace(o)
		if o == "" {
			return
		}
		if _, ok := seen[o]; ok {
			return
		}
		seen[o] = struct{}{}
		out = append(out, o)
	}
	for _, o := range c.CorsOrigins {
		add(o)
	}
	slugs := make([]string, 0, len(c.TenantCORS))
	for slug := range c.TenantCORS {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		tc := c.TenantCORS[slug]
		if tc == nil {
			continue
		}
		for _, o := range tc.AllowedOrigins {
			add(o)
		}
	}
	return out
}

type AuthConfig struct {
	Issuers          []string `koanf:"issuers"`           // List of expected token issuers (e.g., ["https://issuer.example.com"])
	ExpectedAudience string   `koanf:"expected_audience"` // Accept token only if it contains this audience (e.g., "openrails-app")

	// HARDCUT (#312): there is no `auth.operator_tenant_slug` /
	// `auth.operator_tenant_admin_roles`. Admin authority is the LIVE
	// openrails:admin permission held in the caller's OWN tenant (or carried on a
	// deployment-minted admin service token) — deployment authority, not
	// membership in a separate "operator" AuthKit tenant. Load rejects the
	// deprecated keys.

	// ControlPlane configures OpenRails' OpenRails-owned AuthKit control plane
	// (issue #224). When nil/disabled, OpenRails behaves as a pure JWT verifier
	// (current default). When enabled, OpenRails builds an in-process AuthKit
	// core/service, can selectively mount AuthKit route groups, and bootstraps
	// the default tenant's operator tenant + roles + permission catalog + initial
	// operator service token.
	ControlPlane *ControlPlaneConfig `koanf:"control_plane,omitempty"`
}

// ControlPlaneConfig configures the OpenRails-owned AuthKit control plane
// (issue #224). It is OPTIONAL and OFF by default: a deployment that only
// verifies externally-issued JWTs does not set this. A self-hosted, locked-down
// deployment enables it to own user/org/role/service token operations in-process.
type ControlPlaneConfig struct {
	// Enabled turns on the in-process AuthKit control plane. When false (the
	// default), OpenRails does not construct an AuthKit core/service and does not
	// run control-plane bootstrap.
	Enabled bool `koanf:"enabled,omitempty"`

	// Issuer is the AuthKit token issuer this OpenRails control plane signs as
	// (e.g. "https://billing.mysite.com"). Required when Enabled.
	Issuer string `koanf:"issuer,omitempty"`

	// IssuedAudiences are the audiences placed on tokens this control plane
	// issues. ExpectedAudiences are the audiences it accepts. Both default to
	// the verifier's ExpectedAudience when left empty.
	IssuedAudiences   []string `koanf:"issued_audiences,omitempty"`
	ExpectedAudiences []string `koanf:"expected_audiences,omitempty"`

	// TokenPrefix is the brand prefix for minted service tokens (e.g. "openrails" ->
	// `openrails_st_<key_id>_<secret>`). Empty -> bare service-token marker.
	TokenPrefix string `koanf:"token_prefix,omitempty"`

	// PublicUserRegistration enables public native-user self-registration. Default
	// false (restricted: admin/bootstrap-only). Maps to authkit
	// NativeUserRegistrationMode. (authkit issue 60 — independent registration axes.)
	PublicUserRegistration bool `koanf:"public_user_registration,omitempty"`

	// PublicTenantRegistration enables public tenant onboarding/management. Default
	// false (restricted). Maps to authkit TenantRegistrationMode. Independent of
	// PublicUserRegistration — restrict either, both (the typical self-hosted
	// posture), or neither.
	PublicTenantRegistration bool `koanf:"public_tenant_registration,omitempty"`

	// BootstrapAdminServiceTokenName is the human-readable name of the initial
	// deployment admin service token minted at bootstrap (#312). Defaults to
	// "openrails-bootstrap-admin".
	BootstrapAdminServiceTokenName string `koanf:"bootstrap_admin_service_token_name,omitempty"`

	// OperatorServiceTokenName is a deprecated compatibility field for older
	// committed OpenRails call sites. It is intentionally not loadable from
	// config; new deployments use BootstrapAdminServiceTokenName.
	OperatorServiceTokenName string `koanf:"-"`

	// PlatformTenantSlug is the AuthKit tenant slug for the managed-hosting PLATFORM
	// superadmin org (issue #226), DISTINCT from any tenant operator tenant. The
	// platform tenant holds the openrails-platform-superadmin role with the
	// openrails:platform:superadmin permission and gates the cross-tenant
	// /v1/platform/* surface. When empty (the default), platform-superadmin
	// bootstrap and the /v1/platform/* routes are NOT enabled — a single-tenant
	// or non-managed deployment never grows a cross-tenant superadmin.
	PlatformTenantSlug string `koanf:"platform_tenant_slug,omitempty"`

	// PlatformAdminUserID, when set, is assigned the platform-superadmin role in
	// the platform tenant at bootstrap (issue #226). Optional: the platform tenant can
	// be seeded empty and admins added later.
	PlatformAdminUserID string `koanf:"platform_admin_user_id,omitempty"`
}

// PlatformTenantEnabled reports whether a managed-hosting platform-superadmin org
// is configured (issue #226). When false, the cross-tenant /v1/platform/*
// surface is not mounted and no platform-superadmin is bootstrapped.
func (cp *ControlPlaneConfig) PlatformTenantEnabled() bool {
	return cp != nil && strings.TrimSpace(cp.PlatformTenantSlug) != ""
}

// ControlPlaneEnabled reports whether the OpenRails-owned AuthKit control plane
// is enabled for this deployment.
func (c *AuthConfig) ControlPlaneEnabled() bool {
	return c != nil && c.ControlPlane != nil && c.ControlPlane.Enabled
}

// OperatorTenantEnabled is a deprecated compatibility shim for pre-#312 call
// sites that still compile against the operator-tenant helper names.
func (c *AuthConfig) OperatorTenantEnabled() bool {
	return c.ControlPlaneEnabled()
}

// EffectiveOperatorTenantSlug is a deprecated compatibility shim for pre-#312
// call sites. Deprecated operator-tenant config keys are still rejected by Load.
func (c *AuthConfig) EffectiveOperatorTenantSlug() string {
	return "operator"
}

// EffectiveOperatorTenantAdminRoles is a deprecated compatibility shim for
// pre-#312 call sites. New admin authorization uses live openrails:admin grants.
func (c *AuthConfig) EffectiveOperatorTenantAdminRoles() []string {
	return []string{"owner", "admin"}
}

// UserRegistrationOpen reports whether public native-user self-registration is
// enabled (default false = restricted to admin/bootstrap).
func (cp *ControlPlaneConfig) UserRegistrationOpen() bool {
	return cp != nil && cp.PublicUserRegistration
}

// TenantRegistrationOpen reports whether public tenant onboarding/management is
// enabled (default false = restricted to admin/bootstrap).
func (cp *ControlPlaneConfig) TenantRegistrationOpen() bool {
	return cp != nil && cp.PublicTenantRegistration
}

// SelfHostedPosture reports whether to mount only the intentional AuthKit route
// groups (self-hosted) rather than the full hosted-SaaS DefaultAPI surface. True
// unless BOTH registration axes are public (the fully-open hosted-SaaS posture).
func (cp *ControlPlaneConfig) SelfHostedPosture() bool {
	if cp == nil {
		return true
	}
	return !(cp.PublicUserRegistration && cp.PublicTenantRegistration)
}

type SolanaConfig struct {
	// RPCEndpoint is a custom RPC endpoint override. If set, it bypasses the fallback chain entirely.
	// Leave empty to use the automatic fallback chain: Helius (if configured) → Solana public.
	RPCEndpoint string `koanf:"rpc_endpoint"`

	// HeliusAPIKey enables Helius as the primary RPC provider (recommended for production).
	// Get a free API key at https://helius.dev (100k requests/day on free tier).
	// If not set, falls back to Solana public endpoints.
	HeliusAPIKey string `koanf:"helius_api_key"`

	Network         string `koanf:"-"` // derived from the operating mode: devnet (test) or mainnet
	RecipientWallet string `koanf:"recipient_wallet"`

	Tokens map[string]TokenConfig `koanf:"tokens,omitempty"`
	// SolanaPayRecurringSubscriptions controls the public runtime capability flag
	// for recurring Solana Pay transaction-request checkout.
	SolanaPayRecurringSubscriptions bool `koanf:"solana_pay_recurring_subscriptions"`
}

type PythConfig struct {
	HermesURL        string            `koanf:"hermes_url"`
	MaxPriceAge      string            `koanf:"max_price_age"`
	MaxConfidenceBPS int               `koanf:"max_confidence_bps"`
	PriceFeeds       map[string]string `koanf:"price_feeds"`
}

// TokenConfig defines configuration for a specific Solana token
type TokenConfig struct {
	Mint     string `json:"mint" koanf:"mint"`         // Token mint address accepted on the configured Solana network.
	Name     string `json:"name" koanf:"name"`         // Token name.
	Decimals int    `json:"decimals" koanf:"decimals"` // Token decimal places.
}

// RateLimitsConfig is a map of endpoint identifier -> rate limit config
type RateLimitsConfig map[string]*RateLimit

// DunningMode constants define the dunning behavior modes
const (
	// DunningModeOn is the default mode - normal dunning with retry charges, grace period, and recovery workflow
	DunningModeOn = "on"
	// DunningModeDryRunOnly runs the dunning workflow but does not attempt charges - for debugging charge logic bugs
	DunningModeDryRunOnly = "dry_run_only"
	// DunningModeOff disables dunning entirely - rebill failures result in immediate cancellation with no recovery
	DunningModeOff = "off"
)

// DefaultDunningWindowDays bounds how long past a missed rebill dunning may
// still attempt charges (see FeatureFlags.DunningWindowDays).
const DefaultDunningWindowDays = 15

// Operating modes (#346) — see Config.Mode.
const (
	ModeTest       = "test"
	ModeProduction = "production"
	ModeLimited    = "limited"
	ModeReadOnly   = "readonly"
)

// ValidModes contains all valid operating-mode values ("" = legacy/unset).
var ValidModes = map[string]bool{
	"":             true,
	ModeTest:       true,
	ModeProduction: true,
	ModeLimited:    true,
	ModeReadOnly:   true,
}

// ValidDunningModes contains all valid dunning mode values
var ValidDunningModes = map[string]bool{
	DunningModeOn:         true,
	DunningModeDryRunOnly: true,
	DunningModeOff:        true,
}

// FeatureFlags holds feature flag configuration for controlling system behavior.
// These flags are primarily used for safety - disabling destructive operations
// when bugs are suspected, without requiring a code deployment.
type FeatureFlags struct {
	// DunningMode controls dunning (retry charging) behavior for failed subscription rebills.
	// Values:
	//   - "on" (default): Normal dunning - retry charges, grace period, recovery workflow
	//   - "dry_run_only": Workflow runs but no charges attempted - for debugging
	//   - "off": No dunning - immediate cancellation on rebill failure, no recovery
	DunningMode string `koanf:"dunning_mode"`

	// DunningWindowDays bounds how long past a missed rebill (the subscription's
	// current period end) dunning may still attempt charges. Once the window has
	// elapsed the subscription is cancelled and entitlements revoked WITHOUT
	// charging — a card that failed months ago must never be surprise-charged by
	// a catch-up dunning run (e.g. after importing stale legacy subscriptions).
	// 0 or negative = DefaultDunningWindowDays.
	DunningWindowDays int `koanf:"dunning_window_days"`

	// DisableProcessorSubscriptionDeletions, when true, blocks every outbound
	// processor-side delete_subscription call (NMI/Mobius recurring deletes)
	// while local cancellation and entitlement changes proceed normally. The
	// remote subscription is left alive for later reconciliation. Safety switch
	// for migration cutovers — prevents bulk remote deletions while local state
	// is still being converged. STRICTER than LimitedMode: blocks even the
	// deletes that finalize user-asked cancellations.
	DisableProcessorSubscriptionDeletions bool `koanf:"disable_processor_subscription_deletions"`


	// DisableEntitlementExpiration stops all entitlement/credit expiration when true.
	// Affects: CreditExpiryWorker, HoldExpiryWorker, entitlement revocation in FailMembership.
	// Users keep premium access even after subscription ends.
	// Default: false (normal expiration behavior)
	DisableEntitlementExpiration bool `koanf:"disable_entitlement_expiration"`

}

// GetDunningMode returns the effective dunning mode, defaulting to "on" if not set or invalid.
func (f *FeatureFlags) GetDunningMode() string {
	if f == nil || f.DunningMode == "" {
		return DunningModeOn
	}
	mode := strings.ToLower(strings.TrimSpace(f.DunningMode))
	if !ValidDunningModes[mode] {
		return DunningModeOn
	}
	return mode
}

// IsDunningEnabled returns true if dunning charges should be attempted.
// Returns false for "off" and "dry_run_only" modes.
func (f *FeatureFlags) IsDunningEnabled() bool {
	return f.GetDunningMode() == DunningModeOn
}

// IsDunningDryRun returns true if dunning is in dry-run mode (workflow runs, no charges).
func (f *FeatureFlags) IsDunningDryRun() bool {
	return f.GetDunningMode() == DunningModeDryRunOnly
}

// IsDunningOff returns true if dunning is completely disabled (immediate cancel on failure).
func (f *FeatureFlags) IsDunningOff() bool {
	return f.GetDunningMode() == DunningModeOff
}

// GetDunningWindow returns how long past the missed rebill (current period end)
// dunning may still attempt charges, defaulting to DefaultDunningWindowDays.
func (f *FeatureFlags) GetDunningWindow() time.Duration {
	days := DefaultDunningWindowDays
	if f != nil && f.DunningWindowDays > 0 {
		days = f.DunningWindowDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// IsProcessorSubscriptionDeletionDisabled returns true if outbound
// processor-side subscription deletions are blocked.
func (f *FeatureFlags) IsProcessorSubscriptionDeletionDisabled() bool {
	return f != nil && f.DisableProcessorSubscriptionDeletions
}

// SendGridConfig holds SendGrid email configuration.
// Sender info (from_email, from_name) comes from StoreConfig.
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
type CaptchaConfig struct {
	Enabled           bool     `koanf:"enabled"`
	Provider          string   `koanf:"provider"`
	SiteKey           string   `koanf:"site_key"`
	SecretKey         string   `koanf:"secret_key"`
	Action            string   `koanf:"action"`
	VerifyURL         string   `koanf:"verify_url"`
	ScriptURL         string   `koanf:"script_url"`
	MinScore          float64  `koanf:"min_score"`
	ChallengeTTL      string   `koanf:"challenge_ttl"`
	ExtremeMultiplier int      `koanf:"extreme_multiplier"`
	ChallengeBuckets  []string `koanf:"challenge_buckets"`
}

func (c *CaptchaConfig) EffectiveProvider() string {
	if c == nil || strings.TrimSpace(c.Provider) == "" {
		return CaptchaProviderTurnstile
	}
	return strings.ToLower(strings.TrimSpace(c.Provider))
}

func (c *CaptchaConfig) EffectiveVerifyURL() string {
	if c == nil || strings.TrimSpace(c.VerifyURL) == "" {
		switch c.EffectiveProvider() {
		case CaptchaProviderRecaptchaV3:
			return "https://www.google.com/recaptcha/api/siteverify"
		case CaptchaProviderHCaptcha:
			return "https://hcaptcha.com/siteverify"
		default:
			return "https://challenges.cloudflare.com/turnstile/v0/siteverify"
		}
	}
	return strings.TrimSpace(c.VerifyURL)
}

func (c *CaptchaConfig) EffectiveScriptURL() string {
	if c == nil || strings.TrimSpace(c.ScriptURL) == "" {
		switch c.EffectiveProvider() {
		case CaptchaProviderRecaptchaV3:
			siteKey := strings.TrimSpace(c.SiteKey)
			if siteKey == "" {
				return "https://www.google.com/recaptcha/api.js?render="
			}
			return "https://www.google.com/recaptcha/api.js?render=" + url.QueryEscape(siteKey)
		case CaptchaProviderHCaptcha:
			return "https://js.hcaptcha.com/1/api.js?render=explicit"
		default:
			return "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit"
		}
	}
	return strings.TrimSpace(c.ScriptURL)
}

func (c *CaptchaConfig) EffectiveChallengeTTL() time.Duration {
	return parseDurationDefault(c.durationValue(func(c *CaptchaConfig) string { return c.ChallengeTTL }), 15*time.Minute)
}

func (c *CaptchaConfig) EffectiveExtremeMultiplier() int {
	if c == nil || c.ExtremeMultiplier <= 0 {
		return 3
	}
	return c.ExtremeMultiplier
}

func (c *CaptchaConfig) EffectiveMinScore() float64 {
	if c == nil || c.MinScore <= 0 {
		return 0.5
	}
	return c.MinScore
}

func (c *CaptchaConfig) EffectiveAction() string {
	if c == nil || strings.TrimSpace(c.Action) == "" {
		return "billing_challenge"
	}
	return strings.TrimSpace(c.Action)
}

func (c *CaptchaConfig) EffectiveChallengeBuckets() []string {
	if c == nil || len(c.ChallengeBuckets) == 0 {
		return []string{"checkout", "payment-methods", "subscriptions"}
	}
	out := make([]string, 0, len(c.ChallengeBuckets))
	for _, bucket := range c.ChallengeBuckets {
		bucket = strings.ToLower(strings.TrimSpace(bucket))
		if bucket != "" {
			out = append(out, bucket)
		}
	}
	return out
}

func (c *CaptchaConfig) durationValue(get func(*CaptchaConfig) string) string {
	if c == nil {
		return ""
	}
	return get(c)
}

func parseDurationDefault(raw string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// Validate validates the billing configuration
func Validate(cfg *Config) error {
	// Skip strict validation in development environments
	// Operating mode must be a known value — a typo'd mode (e.g. "redaonly")
	// must never silently boot with full behavior (#346).
	if !ValidModes[strings.ToLower(strings.TrimSpace(cfg.Mode))] {
		return fmt.Errorf("invalid mode %q: must be one of test, production, limited, readonly", cfg.Mode)
	}

	// Port range (#349): UnmarshalText validates string-typed values, but an
	// integer yaml value decodes straight into the field and must be checked
	// here. 0 = unset (the default port applies).
	if cfg.Port != 0 && (cfg.Port < 1 || cfg.Port > 65535) {
		return fmt.Errorf("invalid port %d: must be 1-65535", cfg.Port)
	}

	isDev := cfg.Env == "development" || cfg.Env == "dev" || cfg.Env == ""
	if !isDev {
		// Outside development the operating mode must be declared explicitly —
		// sandbox money is dev-only, and "I forgot to set it" must never
		// silently pick live or test behavior.
		switch cfg.GetMode() {
		case ModeProduction, ModeLimited, ModeReadOnly:
		case ModeTest:
			return fmt.Errorf("mode=test is not allowed outside development")
		default:
			return fmt.Errorf("mode is required outside development: set mode (or env MODE) to one of production, limited, readonly")
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
		if cfg.Auth == nil || len(cfg.Auth.Issuers) == 0 {
			return fmt.Errorf("auth issuers must be configured outside development")
		}
		if strings.TrimSpace(cfg.Auth.ExpectedAudience) == "" {
			return fmt.Errorf("auth expected_audience must be configured outside development")
		}
		for _, issuer := range cfg.Auth.Issuers {
			issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
			if issuer == "" || issuer == "http://localhost:8080" || issuer == "http://api:2052" || issuer == "http://issuer:8080" {
				return fmt.Errorf("default auth issuer is not allowed outside development")
			}
		}
		if len(cfg.CorsOrigins) == 0 {
			return fmt.Errorf("cors_origins must be configured outside development")
		}
	}
	// Validate Processors map
	if len(cfg.Processors) > 0 {
		if err := validateProcessors(cfg, isDev); err != nil {
			return fmt.Errorf("processors validation failed: %w", err)
		}
	}

	// Validate Stripe key prefix matches test_mode
	// This runs after processor validation to check the key we'll actually use
	if err := validateStripeKeyForTestMode(cfg); err != nil {
		return err
	}
	if err := validateCaptcha(cfg.Captcha); err != nil {
		return fmt.Errorf("captcha config validation failed: %w", err)
	}

	// Always validate database configuration
	if err := validateDatabase(cfg.DB); err != nil {
		return fmt.Errorf("database config validation failed: %w", err)
	}

	// Billing hot-path fail policy must be an explicit, known value (issue #248).
	if err := validateBillingHotPath(cfg.BillingHotPath); err != nil {
		return fmt.Errorf("billing_hot_path config validation failed: %w", err)
	}

	return nil
}

// validateBillingHotPath rejects an unknown fail policy (issue #248): only
// fail_closed / fail_open are permitted. An empty value is allowed and resolves
// to the fail_closed default (see BillingHotPathConfig.EffectiveFailPolicy).
func validateBillingHotPath(cfg *BillingHotPathConfig) error {
	if cfg == nil {
		return nil
	}
	switch p := strings.ToLower(strings.TrimSpace(cfg.FailPolicy)); p {
	case "", BillingFailClosed, BillingFailOpen:
		return nil
	default:
		return fmt.Errorf("fail_policy must be %q or %q, got %q", BillingFailClosed, BillingFailOpen, cfg.FailPolicy)
	}
}

func validateCaptcha(cfg *CaptchaConfig) error {
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	switch cfg.EffectiveProvider() {
	case CaptchaProviderTurnstile, CaptchaProviderRecaptchaV3, CaptchaProviderHCaptcha:
	default:
		return fmt.Errorf("unsupported provider %q", cfg.Provider)
	}

	if strings.TrimSpace(cfg.SiteKey) == "" {
		return fmt.Errorf("site_key is required when captcha is enabled")
	}
	if strings.TrimSpace(cfg.SecretKey) == "" {
		return fmt.Errorf("secret_key is required when captcha is enabled")
	}
	if cfg.EffectiveProvider() == CaptchaProviderRecaptchaV3 && strings.TrimSpace(cfg.EffectiveAction()) == "" {
		return fmt.Errorf("action is required when recaptcha-v3 is enabled")
	}
	if rawURL := strings.TrimSpace(cfg.VerifyURL); rawURL != "" {
		if err := validateHTTPSURL("verify_url", rawURL); err != nil {
			return err
		}
	}
	if rawURL := strings.TrimSpace(cfg.ScriptURL); rawURL != "" {
		if err := validateHTTPSURL("script_url", rawURL); err != nil {
			return err
		}
	}
	if err := validateOptionalDuration("challenge_ttl", cfg.ChallengeTTL); err != nil {
		return err
	}
	if cfg.MinScore < 0 || cfg.MinScore > 1 {
		return fmt.Errorf("min_score must be between 0 and 1")
	}
	if cfg.ExtremeMultiplier < 0 {
		return fmt.Errorf("extreme_multiplier cannot be negative")
	}
	if len(cfg.ChallengeBuckets) > 0 && len(cfg.EffectiveChallengeBuckets()) == 0 {
		return fmt.Errorf("challenge_buckets must include at least one non-empty bucket")
	}
	return nil
}

func validateHTTPSURL(name, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid %s: %w", name, err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%s must use https", name)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s must include a host", name)
	}
	return nil
}

func validateOptionalDuration(name, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid %s: %w", name, err)
	}
	if d <= 0 {
		return fmt.Errorf("%s must be positive", name)
	}
	return nil
}

// validateStripeKeyForTestMode checks if the Stripe API key prefix matches the operating mode.
// If there's a mismatch, it logs a warning and clears the key to disable Stripe.
// This prevents accidentally processing real charges in test mode or test charges in production.
func validateStripeKeyForTestMode(cfg *Config) error {
	stripeProc, ok := cfg.Processors["stripe"]
	if !ok || stripeProc == nil {
		return nil // No Stripe configured
	}

	secretKey := strings.TrimSpace(stripeProc.SecretKey)
	if secretKey == "" {
		return nil // No key configured, nothing to validate
	}

	// Both standard secret keys (sk_*) and restricted keys (rk_*) carry the
	// live/test mode in their prefix, so classify either form.
	isLiveKey := strings.HasPrefix(secretKey, "sk_live_") || strings.HasPrefix(secretKey, "rk_live_")
	isTestKey := strings.HasPrefix(secretKey, "sk_test_") || strings.HasPrefix(secretKey, "rk_test_")

	if cfg.IsTestMode() && isLiveKey {
		// Hard guarantee (#347): sandbox-money mode must never hold a live key —
		// a mistakenly-test-moded production system would otherwise carry a
		// credential that can move real money.
		return fmt.Errorf("stripe live key (sk_live_/rk_live_) is not allowed in test/sandbox mode; use a test key or switch to a live mode")
	}
	if !cfg.IsTestMode() && isTestKey {
		log.Warn("⚠️  Stripe test key provided but test_mode is disabled (production) - disabling Stripe")
		log.Warn("   Use a live-mode key (sk_live_/rk_live_) in live modes, or set mode=test for testing")
		stripeProc.SecretKey = ""
	}
	return nil
}


// validateProcessors validates all processors in the new Processors map
func validateProcessors(cfg *Config, isDev bool) error {
	for name, proc := range cfg.Processors {
		if proc == nil {
			continue
		}

		effectiveType := proc.GetEffectiveType(name)
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
			if err := validateSolanaProcessor(cfg, name, proc, isDev); err != nil {
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
		log.Warnf("processor '%s' (nmi): webhook_secret not configured; signature verification disabled", name)
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
		log.Warnf("processor '%s' (stripe): webhook_secret not configured; signature verification disabled", name)
	}

	return nil
}

// validateSolanaProcessor validates a Solana-type processor
func validateSolanaProcessor(cfg *Config, name string, proc *ProcessorConfig, isDev bool) error {
	if strings.TrimSpace(proc.RecipientWallet) == "" {
		log.Warnf("processor '%s' (solana): recipient_wallet not configured; Solana payments disabled", name)
	}
	if cfg == nil || cfg.Pyth == nil {
		return fmt.Errorf("processor '%s' (solana): pyth configuration is required", name)
	}
	if strings.TrimSpace(cfg.Pyth.HermesURL) == "" {
		return fmt.Errorf("processor '%s' (solana): pyth.hermes_url is required", name)
	}
	if _, err := time.ParseDuration(strings.TrimSpace(cfg.Pyth.MaxPriceAge)); err != nil {
		return fmt.Errorf("processor '%s' (solana): invalid pyth.max_price_age: %w", name, err)
	}
	for symbol := range proc.Tokens {
		normalized := strings.ToUpper(strings.TrimSpace(symbol))
		if normalized == "" {
			return fmt.Errorf("processor '%s' (solana): token symbol cannot be empty", name)
		}
		if strings.TrimSpace(cfg.Pyth.PriceFeeds[normalized]) == "" {
			return fmt.Errorf("processor '%s' (solana): missing pyth.price_feeds.%s", name, normalized)
		}
	}

	return nil
}

// GetNMIProcessors returns all NMI-backed processor configs from the Processors map.
func (cfg *Config) GetNMIProcessors() map[string]*ProcessorConfig {
	result := make(map[string]*ProcessorConfig)
	if cfg == nil || cfg.Processors == nil {
		return result
	}

	for name, proc := range cfg.Processors {
		if proc != nil && proc.IsNMI(name) {
			result[strings.ToLower(name)] = proc
		}
	}

	return result
}

// GetCCBillProcessor returns the CCBill processor config from the Processors map.
func (cfg *Config) GetCCBillProcessor() *ProcessorConfig {
	if cfg == nil || cfg.Processors == nil {
		return nil
	}
	if proc, ok := cfg.Processors["ccbill"]; ok && proc != nil {
		return proc
	}
	return nil
}

// GetStripeProcessor returns the Stripe processor config from the Processors map.
func (cfg *Config) GetStripeProcessor() *ProcessorConfig {
	if cfg == nil || cfg.Processors == nil {
		return nil
	}
	if proc, ok := cfg.Processors["stripe"]; ok && proc != nil {
		return proc
	}
	return nil
}

// GetSolanaProcessor returns the Solana processor config from the Processors map.
func (cfg *Config) GetSolanaProcessor() *ProcessorConfig {
	if cfg == nil || cfg.Processors == nil {
		return nil
	}
	if proc, ok := cfg.Processors["solana"]; ok && proc != nil {
		return proc
	}
	return nil
}

// GetProcessor returns a processor config by name from the Processors map.
func (cfg *Config) GetProcessor(name string) *ProcessorConfig {
	if cfg == nil || cfg.Processors == nil {
		return nil
	}
	normalizedName := strings.ToLower(strings.TrimSpace(name))
	if proc, ok := cfg.Processors[normalizedName]; ok && proc != nil {
		return proc
	}
	return nil
}

// GetProcessorType returns the type of a processor by name.
// Returns empty string if processor not found.
func (cfg *Config) GetProcessorType(name string) string {
	proc := cfg.GetProcessor(name)
	if proc == nil {
		return ""
	}
	return proc.GetEffectiveType(name)
}

// IsNMIProcessor returns true if the named processor is NMI-backed.
func (cfg *Config) IsNMIProcessor(name string) bool {
	return cfg.GetProcessorType(name) == ProcessorTypeNMI
}

// GetMode returns the normalized operating mode ("" when unset/legacy).
// Unknown values are rejected by Validate; here they normalize to "" so a
// pre-validation caller never misreads a typo as a real mode.
func (cfg *Config) GetMode() string {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if !ValidModes[mode] {
		return ""
	}
	return mode
}

// IsTestMode returns true if payment processors should use sandbox/test
// environments. Mode decides: test → sandbox; production/limited/readonly →
// live. Unset mode means sandbox — that only ever serves development, because
// Validate requires an explicit mode outside it.
func (cfg *Config) IsTestMode() bool {
	switch cfg.GetMode() {
	case ModeTest:
		return true
	case ModeProduction, ModeLimited, ModeReadOnly:
		return false
	}
	return true
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

// GetFeatureFlags returns the feature flags config, or a default config if not set.
func (cfg *Config) GetFeatureFlags() *FeatureFlags {
	if cfg.FeatureFlags == nil {
		return &FeatureFlags{
			DunningMode:                  DunningModeOn,
			DisableEntitlementExpiration: false,
		}
	}
	return cfg.FeatureFlags
}

// GetDunningMode returns the current dunning mode from feature flags.
func (cfg *Config) GetDunningMode() string {
	return cfg.GetFeatureFlags().GetDunningMode()
}

// IsDunningEnabled returns true if normal dunning charges should be attempted.
func (cfg *Config) IsDunningEnabled() bool {
	return cfg.GetFeatureFlags().IsDunningEnabled()
}

// IsDunningDryRun returns true if dunning is in dry-run mode.
func (cfg *Config) IsDunningDryRun() bool {
	return cfg.GetFeatureFlags().IsDunningDryRun()
}

// IsDunningOff returns true if dunning is completely disabled.
func (cfg *Config) IsDunningOff() bool {
	return cfg.GetFeatureFlags().IsDunningOff()
}

// GetDunningWindow returns how long past the missed rebill dunning may still
// attempt charges.
func (cfg *Config) GetDunningWindow() time.Duration {
	return cfg.GetFeatureFlags().GetDunningWindow()
}

// IsProcessorSubscriptionDeletionDisabled returns true if outbound
// processor-side subscription deletions are blocked — by the feature flag, or
// implicitly by readonly mode (zero provider writes).
func (cfg *Config) IsProcessorSubscriptionDeletionDisabled() bool {
	return cfg.GetFeatureFlags().IsProcessorSubscriptionDeletionDisabled() || cfg.IsProviderReadOnly()
}

// IsEntitlementExpirationDisabled returns true if entitlement/credit expiration is disabled.
func (cfg *Config) IsEntitlementExpirationDisabled() bool {
	return cfg.GetFeatureFlags().DisableEntitlementExpiration
}

// assembleDBURL builds the database URL from atomic parameters if not explicitly set
func assembleDBURL(cfg *Config) {
	if cfg.DB == nil {
		return
	}

	// If URL is already explicitly set, nothing to do
	if cfg.DB.URL != "" {
		log.Debug("Using explicitly configured DB_URL")
		return
	}

	connStr := cfg.DB.GetConnectionString()
	if connStr == "" {
		return
	}

	cfg.DB.URL = connStr

	// Log warnings for critical default values being used
	warnings := []string{}
	if cfg.DB.Host == "localhost" {
		warnings = append(warnings, "DB host")
	}
	if cfg.DB.Username == "admin" {
		warnings = append(warnings, "DB username")
	}
	if cfg.DB.Password == "admin_password" {
		warnings = append(warnings, "DB password")
	}
	if cfg.DB.Database == "openrails_db" {
		warnings = append(warnings, "DB database name")
	}

	if len(warnings) > 0 {
		log.Warnf("Using default values for: %s. Assembled DB URL configured", strings.Join(warnings, ", "))
	} else {
		log.Debug("Assembled DB URL from configured parameters")
	}
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
		Env:  "development",
		Host: "0.0.0.0",
		Port: 2053,
		DB: &DBConfig{
			Host:     "localhost",
			Port:     "5432",
			Database: "openrails_db",
			Username: "admin",
			Password: "admin_password",
			SSLMode:  "disable",
			Schema:   DefaultSchema,
		},
		Redis: &RedisConfig{
			// Match docker-compose Garnet (service: garnet)
			Addr:     "garnet:6379",
			Password: "",
			DB:       0,
		},
		Auth: &AuthConfig{
			Issuers:          []string{"http://localhost:8080"},
			ExpectedAudience: "openrails-app",
		},
		ClickHouse: &ClickHouseConfig{
			HTTPAddr:   "http://localhost:8123",
			ClientAddr: "localhost:9000",
			Database:   "analytics",
			Username:   "analytics_user",     // Match docker-compose CLICKHOUSE_USER
			Password:   "analytics_password", // Match docker-compose CLICKHOUSE_PASSWORD
			Cluster:    "openrails",          // Match docker-compose cluster name
		},
		Logger: &LoggerConfig{
			Level: "info", // Default to info level (options: debug, info, warn, error, fatal, panic)
		},
		Pyth: &PythConfig{
			HermesURL:        DefaultPythHermesURL,
			MaxPriceAge:      DefaultPythMaxPriceAge,
			MaxConfidenceBPS: DefaultPythMaxConfidenceBPS,
			PriceFeeds:       DefaultPythPriceFeeds(),
		},
		RateLimits: &RateLimitsConfig{
			"subscribe": &RateLimit{
				RequestsPerMinute: 10, // Very restrictive for payment endpoints
			},
			"checkout": &RateLimit{
				RequestsPerMinute: 5, // Heavy rate limiting for checkout - prevents abuse
			},
			"webhook": &RateLimit{
				RequestsPerMinute: 100, // Higher for webhooks
			},
			"payment": &RateLimit{
				RequestsPerMinute: 20,
			},
			"default": &RateLimit{
				RequestsPerMinute: 60,
			},
		},
		Captcha: &CaptchaConfig{
			Enabled:           false,
			Provider:          CaptchaProviderTurnstile,
			Action:            "billing_challenge",
			ChallengeTTL:      "15m",
			ExtremeMultiplier: 3,
			MinScore:          0.5,
			ChallengeBuckets:  []string{"checkout", "payment-methods", "subscriptions"},
		},
		FeatureFlags: &FeatureFlags{
			DunningMode:                  DunningModeOn,
			DisableEntitlementExpiration: false,
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
		if _, err := os.Stat(candidate); err == nil {
			if err := k.Load(file.Provider(candidate), yaml.Parser()); err != nil {
				return fmt.Errorf("loading config file %s: %w", candidate, err)
			}
			return nil
		}
	}
	return nil
}

func applyPythDefaults(cfg *Config) {
	if cfg.Pyth == nil {
		cfg.Pyth = &PythConfig{}
	}
	if strings.TrimSpace(cfg.Pyth.HermesURL) == "" {
		cfg.Pyth.HermesURL = DefaultPythHermesURL
	}
	if strings.TrimSpace(cfg.Pyth.MaxPriceAge) == "" {
		cfg.Pyth.MaxPriceAge = DefaultPythMaxPriceAge
	}
	if cfg.Pyth.MaxConfidenceBPS <= 0 {
		cfg.Pyth.MaxConfidenceBPS = DefaultPythMaxConfidenceBPS
	}

	defaults := DefaultPythPriceFeeds()
	if cfg.Pyth.PriceFeeds == nil {
		cfg.Pyth.PriceFeeds = defaults
		return
	}
	for symbol, feedID := range cfg.Pyth.PriceFeeds {
		normalized := strings.ToUpper(strings.TrimSpace(symbol))
		if normalized == "" {
			delete(cfg.Pyth.PriceFeeds, symbol)
			continue
		}
		trimmedFeedID := strings.TrimSpace(feedID)
		if normalized != symbol {
			delete(cfg.Pyth.PriceFeeds, symbol)
		}
		cfg.Pyth.PriceFeeds[normalized] = trimmedFeedID
	}
	for symbol, feedID := range defaults {
		if strings.TrimSpace(cfg.Pyth.PriceFeeds[symbol]) == "" {
			cfg.Pyth.PriceFeeds[symbol] = feedID
		}
	}
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
	// - STORE_FROM_EMAIL -> store.from_email
	envKeyToConfigKey := func(s string) string {
		s = strings.ToLower(s)

		// Special case: API_URL -> api_url (top-level, not nested api.url)
		if s == "api_url" {
			return "api_url"
		}

		// Canonical Vault env vars: VAULT_ADDR is HashiCorp's standard name for
		// the server URL, so map it to vault.address (the default first-underscore
		// split would yield vault.addr). VAULT_TOKEN already splits correctly.
		if s == "vault_addr" {
			return "vault.address"
		}

		if s == "auth_issuers" {
			return "auth.issuers"
		}
		if s == "auth_expected_audience" {
			return "auth.expected_audience"
		}
		if s == "openrails_billing_hot_path_fail_policy" || s == "billing_hot_path_fail_policy" {
			return "billing_hot_path.fail_policy"
		}

		// Special case: CORS_ORIGINS -> cors_origins (top-level, not cors.origins)
		if s == "cors_origins" {
			return "cors_origins"
		}

		// FEATURE_FLAGS_* -> feature_flags.*
		if strings.HasPrefix(s, "feature_flags_") {
			return "feature_flags." + strings.TrimPrefix(s, "feature_flags_")
		}

		// USDC_FUNDING_PROVIDERS_<NAME>_<FIELD> -> usdc_funding.providers.<name>.<field>
		if strings.HasPrefix(s, "usdc_funding_providers_") {
			rest := strings.TrimPrefix(s, "usdc_funding_providers_")
			parts := strings.SplitN(rest, "_", 2)
			if len(parts) == 2 {
				return fmt.Sprintf("usdc_funding.providers.%s.%s", parts[0], parts[1])
			}
		}

		// PROCESSORS_<NAME>_<FIELD> -> processors.<name>.<field>
		// Example: PROCESSORS_MOBIUS_SECURITY_KEY -> processors.mobius.security_key
		if strings.HasPrefix(s, "processors_") {
			parts := strings.SplitN(s, "_", 3)
			if len(parts) == 3 {
				return fmt.Sprintf("processors.%s.%s", parts[1], parts[2])
			}
		}

		// Replace only the first underscore for other nested config keys.
		if !strings.Contains(s, "_") {
			return s
		}
		return strings.Replace(s, "_", ".", 1)
	}

	envCallbackWithValue := func(key string, value string) (string, interface{}) {
		mapped := envKeyToConfigKey(key)
		if mapped == "" {
			return "", nil
		}

		v := strings.TrimSpace(value)

		// Allow JSON for arrays/objects (common in docker-compose: AUTH_ISSUERS='["..."]').
		if len(v) >= 2 {
			if (v[0] == '[' && v[len(v)-1] == ']') || (v[0] == '{' && v[len(v)-1] == '}') {
				var decoded interface{}
				if err := json.Unmarshal([]byte(v), &decoded); err == nil {
					return mapped, decoded
				}
			}
		}

		// Convenience: allow comma-separated lists for common slice env vars.
		if mapped == "cors_origins" && strings.Contains(v, ",") {
			parts := strings.Split(v, ",")
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					out = append(out, p)
				}
			}
			return mapped, out
		}
		if mapped == "auth.issuers" {
			parts := strings.Split(v, ",")
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					out = append(out, p)
				}
			}
			return mapped, out
		}
		return mapped, v
	}

	if err := k.Load(env.ProviderWithValue("", ".", envCallbackWithValue), nil); err != nil {
		return nil, fmt.Errorf("loading environment variables: %w", err)
	}

	// Unmarshal into config struct (overlay onto defaults)
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}

	// Store defaults (used across the system for branding)
	if cfg.Store == nil {
		cfg.Store = &StoreConfig{}
	}
	if cfg.Store.Name == "" {
		cfg.Store.Name = "My Store"
	}
	if cfg.Store.LogoURL == "" {
		cfg.Store.LogoURL = DefaultLogoURL
	}
	applyPythDefaults(cfg)

	// Initialize and normalize Processors map
	if cfg.Processors == nil {
		cfg.Processors = make(map[string]*ProcessorConfig)
	}

	if len(cfg.Processors) > 0 {
		normalized := make(map[string]*ProcessorConfig, len(cfg.Processors))
		for name, proc := range cfg.Processors {
			key := strings.TrimSpace(strings.ToLower(name))
			if key == "" {
				log.Warnf("ignoring processor with empty name (original key: %q)", name)
				continue
			}
			if proc == nil {
				log.Warnf("ignoring processor '%s' with nil config", key)
				continue
			}

			if existing, exists := normalized[key]; exists && existing != nil {
				log.Warnf("duplicate processor configuration detected for key '%s'; overriding previous value", key)
			}

			// Non-reserved processor names must declare an explicit type.
			effectiveType := proc.GetEffectiveType(key)
			if effectiveType == "" {
				return nil, fmt.Errorf("processor '%s' must declare a type", key)
			}

			// Warn if reserved name has conflicting type
			if impliedType, isReserved := ReservedProcessorNames[key]; isReserved && proc.Type != "" && proc.Type != impliedType {
				log.Warnf("processor '%s' has type '%s' but '%s' is a reserved name implying type '%s'; using implied type",
					key, proc.Type, key, impliedType)
				proc.Type = impliedType
			}

			normalized[key] = proc
		}
		cfg.Processors = normalized
	}

	// Assemble DB URL from pieces if not explicitly set
	assembleDBURL(cfg)

	// Normalize the OpenRails Postgres schema to its canonical form (#165) so the
	// stored config value matches what SchemaName() resolves to. Validation of the
	// identifier happens in Validate(). Defaults to `billing`.
	if cfg.DB != nil {
		cfg.DB.Schema = normalizeSchema(cfg.DB.Schema)
	}

	// Log test mode status clearly at startup
	logTestModeStatus(cfg)

	// Log feature flags status at startup
	logFeatureFlagsStatus(cfg)

	// Validate the loaded configuration
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}

// logFeatureFlagsStatus logs the operating mode + feature flags at startup.
// This helps operators understand any non-default behavior.
func logFeatureFlagsStatus(cfg *Config) {
	flags := cfg.GetFeatureFlags()

	// Operating-mode banner (#346). The mode dial is the headline; the flag
	// logs below cover the fine-grained dials.
	switch cfg.GetMode() {
	case ModeReadOnly:
		log.Warn("⚠️  MODE=readonly - ZERO payment-provider writes; even user-initiated charges fail loudly")
		log.Info("   Reconciliation/forensics posture: provider reads + local serving only")
		log.Info("   Set mode=limited or mode=production to allow writes")
	case ModeLimited:
		log.Warn("⚠️  MODE=limited - No proactive payment-provider operations will be performed")
		log.Info("   Dunning charges/cancellations, auto-top-ups, arrears collection, Solana pulls and catalog provider writes are paused")
		log.Info("   Reactive operations (checkout, vault saves, user/admin cancels, webhooks) work normally")
		log.Info("   Set mode=production to resume proactive operations")
	case ModeProduction:
		log.Info("Mode: production (live processors, full behavior)")
	case ModeTest:
		log.Info("Mode: test (sandbox processors, full behavior)")
	}

	// Log dunning mode if not default
	dunningMode := flags.GetDunningMode()
	switch dunningMode {
	case DunningModeDryRunOnly:
		log.Warn("⚠️  DUNNING DRY-RUN MODE - Dunning workflow runs but no charges will be attempted")
		log.Info("   Subscriptions will stay in past_due state, retry counts preserved")
		log.Info("   Set feature_flags.dunning_mode=on to enable charges")
	case DunningModeOff:
		log.Warn("⚠️  DUNNING DISABLED - Failed rebills will result in immediate cancellation")
		log.Info("   No grace period, no retry attempts, no recovery workflow")
		log.Info("   Set feature_flags.dunning_mode=on to enable normal dunning")
	}

	// Log processor subscription deletions if disabled
	if flags.IsProcessorSubscriptionDeletionDisabled() {
		log.Warn("⚠️  PROCESSOR SUBSCRIPTION DELETIONS DISABLED - delete_subscription calls to NMI will be skipped")
		log.Info("   Local cancellations and downgrades proceed; remote subscriptions stay alive for reconciliation")
		log.Info("   Set feature_flags.disable_processor_subscription_deletions=false to resume deletions")
	}

	// Log entitlement expiration if disabled
	if flags.DisableEntitlementExpiration {
		log.Warn("⚠️  ENTITLEMENT EXPIRATION DISABLED - Credits and entitlements will not expire")
		log.Info("   CreditExpiryWorker, HoldExpiryWorker, and entitlement revocation are paused")
		log.Info("   Users keep premium access even after subscription ends")
		log.Info("   Set feature_flags.disable_entitlement_expiration=false to resume expiration")
	}
}

// logTestModeStatus logs the payment processing mode at startup.
// This helps operators confirm whether they're in test or production mode.
func logTestModeStatus(cfg *Config) {
	if cfg.IsTestMode() {
		log.Warn("⚠️  TEST MODE ENABLED - No real charges will be processed")
		log.Info("   Payment providers will use sandbox/test environments:")
		log.Info("   - NMI: secure.networkmerchants.com with test-mode transactions")
		log.Info("   - CCBill: sandbox-api.ccbill.com")
		log.Info("   - Stripe: requires sk_test_* key")
		log.Info("   - Solana: devnet")
	} else {
		log.Warn("🔴 PRODUCTION MODE - Real charges enabled")
		log.Info("   Payment providers will use production environments")

		// Warn if running real charges in dev environment (unusual)
		if cfg.IsDev() {
			log.Warn("⚠️  Real payment processing enabled in dev environment - this is unusual")
			log.Warn("   Set mode=test (env MODE=test) to use sandbox environments")
		}
	}
}
