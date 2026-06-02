package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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

// FlexiblePort is a custom type that can unmarshal both strings and integers
type FlexiblePort int16

// UnmarshalText implements the encoding.TextUnmarshaler interface
func (p *FlexiblePort) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*p = 0
		return nil
	}

	val, err := strconv.ParseInt(s, 10, 16)
	if err != nil {
		return fmt.Errorf("invalid port value: %w", err)
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

	// Cloudflared contains Cloudflare Tunnel settings used for local/dev tooling.
	// Billing does not run cloudflared, but we keep these keys in config so that
	// config.example.yaml can document deterministic webhook setups consistently.
	Cloudflared *CloudflaredConfig `koanf:"cloudflared,omitempty"`

	// TestMode controls whether payment processors use sandbox/test environments.
	// When true: NMI uses sandbox.nmi.com, CCBill uses sandbox-api.ccbill.com,
	// Solana uses devnet, Stripe requires sk_test_* key.
	// When false: All processors use production environments (real charges).
	// Defaults to true for safety. Set to false only for production deployments.
	// Note: This is orthogonal to Env - Env controls logging/debug, TestMode controls payments.
	TestMode *bool `koanf:"test_mode,omitempty"`

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

	DB           *DBConfig         `koanf:"db,omitempty"`
	Redis        *RedisConfig      `koanf:"redis,omitempty"`
	Auth         *AuthConfig       `koanf:"auth,omitempty"`
	ClickHouse   *ClickHouseConfig `koanf:"clickhouse,omitempty"`
	Logger       *LoggerConfig     `koanf:"logger,omitempty"`
	SendGrid     *SendGridConfig   `koanf:"sendgrid,omitempty"`
	Pyth         *PythConfig       `koanf:"pyth,omitempty"`
	CorsOrigins  []string          `koanf:"cors_origins,omitempty"`
	// TenantCORS configures per-tenant browser-direct allowed origins (issue #222
	// browser tier). It is keyed by tenant slug; each entry lists the exact
	// origins a browser on that tenant's domain may use to call OpenRails
	// directly (e.g. the /v1/self/* self-service surface with a delegated access
	// token). These origins are added to the allow-list ALONGSIDE CorsOrigins —
	// they are an additive union, never a wildcard. Preflight succeeds for any
	// listed origin and is denied for any origin that is not listed in either
	// CorsOrigins or some tenant's allowed origins.
	TenantCORS   map[string]*TenantCORSConfig `koanf:"tenant_cors,omitempty"`
	RateLimits   *RateLimitsConfig `koanf:"rate_limits,omitempty"`
	Captcha      *CaptchaConfig    `koanf:"captcha,omitempty"`
	FeatureFlags *FeatureFlags     `koanf:"feature_flags,omitempty"`
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
	DirectPostURL   string `koanf:"direct_post_url"`
	QueryURL        string `koanf:"query_url"`

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
	RPCEndpoint     string                 `koanf:"rpc_endpoint"`
	HeliusAPIKey    string                 `koanf:"helius_api_key"`
	Network         string                 `koanf:"network"`
	RecipientWallet string                 `koanf:"recipient_wallet"`
	Tokens          map[string]TokenConfig `koanf:"tokens"`
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
		DirectPostURL:   p.DirectPostURL,
		QueryURL:        p.QueryURL,
		TestMode:        false, // Will be set by caller based on global test_mode
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
		TestMode:           false, // Will be set by caller based on global test_mode
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
		RPCEndpoint:     p.RPCEndpoint,
		HeliusAPIKey:    p.HeliusAPIKey,
		Network:         p.Network,
		RecipientWallet: p.RecipientWallet,
		Tokens:          p.Tokens,
	}
}

type NMIProviderSettings struct {
	Name            string
	SecurityKey     string
	TokenizationKey string
	WebhookSecret   string
	TestMode        bool
	DirectPostURL   string
	QueryURL        string
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

// AuthConfig holds JWT verification configuration for billing service.
// Billing is a JWT verifier (not issuer) - it validates tokens issued by your IdP.
// TenantCORSConfig holds the browser-direct CORS policy for a single tenant
// (issue #222 browser tier).
type TenantCORSConfig struct {
	// AllowedOrigins is the exact set of browser origins (scheme+host+port) on
	// this tenant's domain that may call OpenRails directly. Exact-match only; no
	// wildcards. Example: ["https://app.doujins.com", "https://doujins.com"].
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

	// OperatorOrgSlug, when set, switches admin auth (all /admin/* routes) from
	// the global "admin" role check to an AuthKit-org-claim check:
	// the caller must have UserContext.Org == OperatorOrgSlug AND hold one of
	// OperatorOrgAdminRoles within that org. When empty (default), the
	// global "admin" role check from profiles.user_roles is used.
	//
	// Use this for multi-org AuthKit deployments where a single org owns the
	// billing operator role. For single-org AuthKit deployments and self-hosted
	// instances with one customer, leave this unset.
	OperatorOrgSlug string `koanf:"operator_org_slug,omitempty"`

	// OperatorOrgAdminRoles is the list of OrgRoles considered admin-equivalent
	// when OperatorOrgSlug is set. Defaults to ["admin", "owner"].
	OperatorOrgAdminRoles []string `koanf:"operator_org_admin_roles,omitempty"`

	// ControlPlane configures OpenRails' OpenRails-owned AuthKit control plane
	// (issue #224). When nil/disabled, OpenRails behaves as a pure JWT verifier
	// (current default). When enabled, OpenRails builds an in-process AuthKit
	// core/service, can selectively mount AuthKit route groups, and bootstraps
	// the default tenant's operator org + roles + permission catalog + initial
	// operator OAT.
	ControlPlane *ControlPlaneConfig `koanf:"control_plane,omitempty"`
}

// ControlPlaneConfig configures the OpenRails-owned AuthKit control plane
// (issue #224). It is OPTIONAL and OFF by default: a deployment that only
// verifies externally-issued JWTs does not set this. A self-hosted, locked-down
// deployment enables it to own user/org/role/OAT operations in-process.
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

	// OrgMode is the AuthKit org mode ("single" or "multi"). Operator-org
	// bootstrap and the org-management HTTP routes require "multi". Defaults to
	// "multi" when Enabled so the operator org and its OATs exist.
	OrgMode string `koanf:"org_mode,omitempty"`

	// TokenPrefix is the brand prefix for minted OATs (e.g. "openrails" ->
	// `openrails_oat_<key_id>_<secret>`). Empty -> bare `oat_`.
	TokenPrefix string `koanf:"token_prefix,omitempty"`

	// LockedDown selects the self-hosted, locked-down posture: public user
	// registration and public org management are disabled, and only the
	// intentional AuthKit route groups are mounted (never DefaultAPI). Defaults
	// to true when Enabled — hosted-SaaS mode must explicitly opt out.
	LockedDown *bool `koanf:"locked_down,omitempty"`

	// OperatorOATName is the human-readable name of the initial operator OAT
	// minted at bootstrap. Defaults to "openrails-bootstrap-operator".
	OperatorOATName string `koanf:"operator_oat_name,omitempty"`
}

// ControlPlaneEnabled reports whether the OpenRails-owned AuthKit control plane
// is enabled for this deployment.
func (c *AuthConfig) ControlPlaneEnabled() bool {
	return c != nil && c.ControlPlane != nil && c.ControlPlane.Enabled
}

// LockedDownEnabled reports whether the control plane runs in the self-hosted,
// locked-down posture (public registration + org management disabled, selective
// route mounting). Defaults to true when the control plane is enabled.
func (cp *ControlPlaneConfig) LockedDownEnabled() bool {
	if cp == nil {
		return false
	}
	if cp.LockedDown == nil {
		return true
	}
	return *cp.LockedDown
}

// OperatorOrgEnabled reports whether OperatorOrgSlug is set (multi-org admin mode).
func (c *AuthConfig) OperatorOrgEnabled() bool {
	return c != nil && strings.TrimSpace(c.OperatorOrgSlug) != ""
}

// EffectiveOperatorOrgAdminRoles returns OperatorOrgAdminRoles or the default set.
func (c *AuthConfig) EffectiveOperatorOrgAdminRoles() []string {
	if c == nil || len(c.OperatorOrgAdminRoles) == 0 {
		return []string{"admin", "owner"}
	}
	return c.OperatorOrgAdminRoles
}

type SolanaConfig struct {
	// RPCEndpoint is a custom RPC endpoint override. If set, it bypasses the fallback chain entirely.
	// Leave empty to use the automatic fallback chain: Helius (if configured) → Solana public.
	RPCEndpoint string `koanf:"rpc_endpoint"`

	// HeliusAPIKey enables Helius as the primary RPC provider (recommended for production).
	// Get a free API key at https://helius.dev (100k requests/day on free tier).
	// If not set, falls back to Solana public endpoints.
	HeliusAPIKey string `koanf:"helius_api_key"`

	Network         string `koanf:"network"` // mainnet, devnet, testnet
	RecipientWallet string `koanf:"recipient_wallet"`

	Tokens map[string]TokenConfig `koanf:"tokens,omitempty"`
}

type PythConfig struct {
	HermesURL        string            `koanf:"hermes_url"`
	MaxPriceAge      string            `koanf:"max_price_age"`
	MaxConfidenceBPS int               `koanf:"max_confidence_bps"`
	PriceFeeds       map[string]string `koanf:"price_feeds"`
}

type CloudflaredConfig struct {
	// TunnelToken is the cloudflared "tunnel run token" (secret). Prefer setting via env.
	TunnelToken string `koanf:"tunnel_token"`
	// TunnelName is a human-friendly identifier for the tunnel (non-secret).
	TunnelName string `koanf:"tunnel_name"`
	// PublicHostname is the stable hostname (e.g., billing-webhooks-sandbox.example.com) routed to localhost.
	PublicHostname string `koanf:"public_hostname"`
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

	// DisableEntitlementExpiration stops all entitlement/credit expiration when true.
	// Affects: CreditExpiryWorker, HoldExpiryWorker, entitlement revocation in FailMembership.
	// Users keep premium access even after subscription ends.
	// Default: false (normal expiration behavior)
	DisableEntitlementExpiration bool `koanf:"disable_entitlement_expiration"`

	// VerifyProcessorMappings enables remote verification of provided processor identifiers
	// when using the catalog definition surface (e.g., checking a Stripe price_id exists).
	// Default: false (link ids are validated only for presence/shape, not existence).
	VerifyProcessorMappings bool `koanf:"verify_processor_mappings"`
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

// SendGridConfig holds SendGrid email configuration.
// Sender info (from_email, from_name) comes from StoreConfig.
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
	isDev := cfg.Env == "development" || cfg.Env == "dev" || cfg.Env == ""
	if !isDev {
		// Unset test_mode outside development means live (see IsTestMode).
		// Only an explicit test_mode=true is rejected — sandbox isn't allowed in prod.
		if cfg.TestMode != nil && *cfg.TestMode {
			return fmt.Errorf("test_mode=true is not allowed outside development")
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
	validateStripeKeyForTestMode(cfg)
	if err := validateCaptcha(cfg.Captcha); err != nil {
		return fmt.Errorf("captcha config validation failed: %w", err)
	}

	// Always validate database configuration
	if err := validateDatabase(cfg.DB); err != nil {
		return fmt.Errorf("database config validation failed: %w", err)
	}

	return nil
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

// validateStripeKeyForTestMode checks if the Stripe API key prefix matches the test_mode setting.
// If there's a mismatch, it logs a warning and clears the key to disable Stripe.
// This prevents accidentally processing real charges in test mode or test charges in production.
func validateStripeKeyForTestMode(cfg *Config) {
	stripeProc, ok := cfg.Processors["stripe"]
	if !ok || stripeProc == nil {
		return // No Stripe configured
	}

	secretKey := strings.TrimSpace(stripeProc.SecretKey)
	if secretKey == "" {
		return // No key configured, nothing to validate
	}

	// Both standard secret keys (sk_*) and restricted keys (rk_*) carry the
	// live/test mode in their prefix, so classify either form.
	isLiveKey := strings.HasPrefix(secretKey, "sk_live_") || strings.HasPrefix(secretKey, "rk_live_")
	isTestKey := strings.HasPrefix(secretKey, "sk_test_") || strings.HasPrefix(secretKey, "rk_test_")

	if cfg.IsTestMode() && isLiveKey {
		log.Warn("⚠️  Stripe live key provided but test_mode is enabled - disabling Stripe")
		log.Warn("   Use a test-mode key (sk_test_/rk_test_) when test_mode=true, or set test_mode=false for production")
		stripeProc.SecretKey = ""
	} else if !cfg.IsTestMode() && isTestKey {
		log.Warn("⚠️  Stripe test key provided but test_mode is disabled (production) - disabling Stripe")
		log.Warn("   Use a live-mode key (sk_live_/rk_live_) when test_mode=false, or set test_mode=true for testing")
		stripeProc.SecretKey = ""
	}
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

	if proc.DirectPostURL != "" {
		if _, err := url.Parse(proc.DirectPostURL); err != nil {
			return fmt.Errorf("processor '%s' (nmi): invalid direct_post_url: %w", name, err)
		}
	}

	if proc.QueryURL != "" {
		if _, err := url.Parse(proc.QueryURL); err != nil {
			return fmt.Errorf("processor '%s' (nmi): invalid query_url: %w", name, err)
		}
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

// IsTestMode returns true if payment processors should use sandbox/test environments.
// An explicit TestMode always wins. When unset it follows the environment:
// sandbox in development, live in production.
func (cfg *Config) IsTestMode() bool {
	if cfg.TestMode == nil {
		return cfg.IsDev()
	}
	return *cfg.TestMode
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
	if cfg.DB.Database == "doujins_db" {
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
			Database: "doujins_db",
			Username: "admin",
			Password: "admin_password",
			SSLMode:  "disable",
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

		// Special case: TEST_MODE/OPENRAILS_TEST_MODE -> test_mode (top-level)
		if s == "test_mode" || s == "openrails_test_mode" {
			return "test_mode"
		}

		if s == "auth_issuers" {
			return "auth.issuers"
		}
		if s == "auth_expected_audience" {
			return "auth.expected_audience"
		}

		// Special case: CORS_ORIGINS -> cors_origins (top-level, not cors.origins)
		if s == "cors_origins" {
			return "cors_origins"
		}

		// FEATURE_FLAGS_* -> feature_flags.*
		if strings.HasPrefix(s, "feature_flags_") {
			return "feature_flags." + strings.TrimPrefix(s, "feature_flags_")
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

// logFeatureFlagsStatus logs the feature flags configuration at startup.
// This helps operators understand any non-default behavior.
func logFeatureFlagsStatus(cfg *Config) {
	flags := cfg.GetFeatureFlags()

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
		log.Info("   - NMI: sandbox.nmi.com")
		log.Info("   - CCBill: sandbox-api.ccbill.com")
		log.Info("   - Stripe: requires sk_test_* key")
		log.Info("   - Solana: devnet")
	} else {
		log.Warn("🔴 PRODUCTION MODE - Real charges enabled")
		log.Info("   Payment providers will use production environments")

		// Warn if running real charges in dev environment (unusual)
		if cfg.IsDev() {
			log.Warn("⚠️  Real payment processing enabled in dev environment - this is unusual")
			log.Warn("   Set test_mode=true or TEST_MODE=true to use sandbox environments")
		}
	}
}
