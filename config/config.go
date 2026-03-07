package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	Env         string       `koanf:"env,omitempty"`
	Port        FlexiblePort `koanf:"port,omitempty"`         // Standalone only: public HTTP port (default 2053)
	PrivatePort FlexiblePort `koanf:"private_port,omitempty"` // Standalone only: internal/service API port (default 8060)
	Host        string       `koanf:"host,omitempty"`         // Standalone only: address to bind to (default 0.0.0.0)
	APIKey      string       `koanf:"api_key,omitempty"`      // Shared secret for service-to-service auth (X-API-KEY header)

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
	Jupiter      *JupiterConfig    `koanf:"jupiter,omitempty"`
	CorsOrigins  []string          `koanf:"cors_origins,omitempty"`
	RateLimits   *RateLimitsConfig `koanf:"rate_limits,omitempty"`
	FeatureFlags *FeatureFlags     `koanf:"feature_flags,omitempty"`
}

// DBConfig holds database configuration.
// Supports both legacy connection string (URL) and atomic parameters.
// If URL is provided, it takes precedence. Otherwise, connection string
// is built from individual parameters (Host, Port, Username, etc.).
// Database is always PostgreSQL.
type DBConfig struct {
	// Legacy: Full connection string (optional)
	URL string `koanf:"url"`

	// Atomic parameters (preferred for template-based configuration)
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
	// 1. If legacy URL is provided, use it
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

		// Default to sslmode=disable if not specified
		sslMode := c.SSLMode
		if sslMode == "" {
			sslMode = "disable"
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
// Non-reserved names (e.g., "mobius", "acme") require type: nmi.
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
	// WebhookSecret is shared with NMI (same field name)

	// --- Solana fields (type: solana) ---
	RPCEndpoint     string                 `koanf:"rpc_endpoint"`
	HeliusAPIKey    string                 `koanf:"helius_api_key"`
	Network         string                 `koanf:"network"`
	RecipientWallet string                 `koanf:"recipient_wallet"`
	SupportedTokens map[string]TokenConfig `koanf:"supported_tokens"`
	EnabledTokens   []string               `koanf:"enabled_tokens"`
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
		SupportedTokens: p.SupportedTokens,
		EnabledTokens:   p.EnabledTokens,
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
type AuthConfig struct {
	Issuers          []string `koanf:"issuers"`           // List of expected token issuers (e.g., ["https://issuer.example.com"])
	ExpectedAudience string   `koanf:"expected_audience"` // Accept token only if it contains this audience (e.g., "openrails-app")
}

type SolanaConfig struct {
	// RPCEndpoint is a custom RPC endpoint override. If set, it bypasses the fallback chain entirely.
	// Leave empty to use the automatic fallback chain: Helius (if configured) → Ankr → Solana public.
	RPCEndpoint string `koanf:"rpc_endpoint"`

	// HeliusAPIKey enables Helius as the primary RPC provider (recommended for production).
	// Get a free API key at https://helius.dev (100k requests/day on free tier).
	// If not set, falls back to Ankr → Solana public endpoints.
	HeliusAPIKey string `koanf:"helius_api_key"`

	Network         string `koanf:"network"` // mainnet, devnet, testnet
	RecipientWallet string `koanf:"recipient_wallet"`

	SupportedTokens map[string]TokenConfig `koanf:"supported_tokens,omitempty"`

	// EnabledTokens is a simplified token configuration (alternative to SupportedTokens).
	// Use symbol strings for verified tokens from Jupiter Tokens V2: ["SOL", "USDC", "BONK"].
	// If not set, defaults to ["SOL", "USDC", "PYUSD"].
	EnabledTokens []string `koanf:"enabled_tokens,omitempty"`
}

type JupiterConfig struct {
	APIKey string `koanf:"api_key"`
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
	Mint        string  `json:"mint" koanf:"mint"`         // Token mint address
	Symbol      string  `json:"symbol" koanf:"symbol"`     // Token symbol (e.g., "SOL", "USDC")
	Name        string  `json:"name" koanf:"name"`         // Token name
	Decimals    int     `json:"decimals" koanf:"decimals"` // Token decimal places
	Price       float64 `json:"price"`                     // Price in USD (fetched from Jupiter at runtime, not loaded from config)
	Enabled     bool    `json:"enabled" koanf:"enabled"`   // Whether this token is enabled
	MainnetMint string  `json:"mainnet_mint,omitempty" koanf:"mainnet_mint"`
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
	// Burst is the maximum burst size (optional, defaults to RequestsPerMinute).
	// Reserved for future use with token bucket algorithms.
	Burst int `koanf:"burst"`
}

// Validate validates the billing configuration
func Validate(cfg *Config) error {
	// Skip strict validation in development environments
	isDev := cfg.Env == "development" || cfg.Env == "dev" || cfg.Env == ""

	// Validate Processors map
	if len(cfg.Processors) > 0 {
		if err := validateProcessors(cfg, isDev); err != nil {
			return fmt.Errorf("processors validation failed: %w", err)
		}
	}

	// Validate Stripe key prefix matches test_mode
	// This runs after processor validation to check the key we'll actually use
	validateStripeKeyForTestMode(cfg)

	// Always validate database configuration
	if err := validateDatabase(cfg.DB); err != nil {
		return fmt.Errorf("database config validation failed: %w", err)
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

	isLiveKey := strings.HasPrefix(secretKey, "sk_live_")
	isTestKey := strings.HasPrefix(secretKey, "sk_test_")

	if cfg.IsTestMode() && isLiveKey {
		log.Warn("⚠️  Stripe live key provided but test_mode is enabled - disabling Stripe")
		log.Warn("   Use sk_test_* key when test_mode=true, or set test_mode=false for production")
		stripeProc.SecretKey = ""
	} else if !cfg.IsTestMode() && isTestKey {
		log.Warn("⚠️  Stripe test key provided but test_mode is disabled (production) - disabling Stripe")
		log.Warn("   Use sk_live_* key when test_mode=false, or set test_mode=true for testing")
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
func validateSolanaProcessor(name string, proc *ProcessorConfig, isDev bool) error {
	if strings.TrimSpace(proc.RecipientWallet) == "" {
		log.Warnf("processor '%s' (solana): recipient_wallet not configured; Solana payments disabled", name)
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

// GetJupiterAPIKey returns the configured Jupiter API key, if any.
func (cfg *Config) GetJupiterAPIKey() string {
	if cfg == nil || cfg.Jupiter == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Jupiter.APIKey)
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
// This is a simple accessor - TestMode defaults to true for safety.
// Note: This is orthogonal to Env. Env controls logging/debug, TestMode controls payments.
func (cfg *Config) IsTestMode() bool {
	if cfg.TestMode == nil {
		return true // Default to test mode for safety
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

	// Assemble URL from atomic parameters
	// All parameters have defaults from GetDefaultBillingConfig(), so they should all be present
	connStr := fmt.Sprintf(
		"postgresql://%s:%s@%s:%s/%s?sslmode=%s",
		cfg.DB.Username,
		cfg.DB.Password,
		cfg.DB.Host,
		cfg.DB.Port,
		cfg.DB.Database,
		cfg.DB.SSLMode,
	)

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
		log.Warnf("Using default values for: %s. Assembled DB URL: %s",
			strings.Join(warnings, ", "), connStr)
	} else {
		log.Debugf("Assembled DB URL from configured parameters: %s", connStr)
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
		Env:         "development",
		Host:        "0.0.0.0",
		Port:        2053,
		PrivatePort: 8060, // Private/service API port (internal only)
		DB: &DBConfig{
			Host:     "localhost",
			Port:     "5432",
			Database: "openrails_db",
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
		RateLimits: &RateLimitsConfig{
			"subscribe": &RateLimit{
				RequestsPerMinute: 10, // Very restrictive for payment endpoints
				Burst:             3,
			},
			"checkout": &RateLimit{
				RequestsPerMinute: 5, // Heavy rate limiting for checkout - prevents abuse
				Burst:             2,
			},
			"webhook": &RateLimit{
				RequestsPerMinute: 100, // Higher for webhooks
				Burst:             20,
			},
			"payment": &RateLimit{
				RequestsPerMinute: 20,
				Burst:             5,
			},
			"default": &RateLimit{
				RequestsPerMinute: 60,
				Burst:             10,
			},
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

		// Special case: ENVIRONMENT -> env (back-compat with some .env templates)
		if s == "environment" {
			return "env"
		}

		// Special case: API_URL -> api_url (top-level, not nested api.url)
		if s == "api_url" {
			return "api_url"
		}

		// Special case: API_KEY/OPENRAILS_API_KEY/BILLING_API_KEY -> api_key (top-level, not api.key)
		// Used for private/service API auth (X-API-KEY header).
		if s == "api_key" || s == "openrails_api_key" || s == "billing_api_key" {
			return "api_key"
		}

		// Special case: TEST_MODE/OPENRAILS_TEST_MODE/BILLING_TEST_MODE -> test_mode (top-level)
		if s == "test_mode" || s == "openrails_test_mode" || s == "billing_test_mode" {
			return "test_mode"
		}

		// Special case: PRIVATE_PORT -> private_port (top-level, not private.port)
		if s == "private_port" {
			return "private_port"
		}

		// Special case: CORS_ORIGINS -> cors_origins (top-level, not cors.origins)
		if s == "cors_origins" {
			return "cors_origins"
		}

		// Legacy (avoid ad-hoc os.Getenv in integrations): MOBIUS_* -> processors.mobius.*
		// Example: MOBIUS_SECURITY_KEY -> processors.mobius.security_key
		if strings.HasPrefix(s, "mobius_") {
			return "processors.mobius." + strings.TrimPrefix(s, "mobius_")
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
		if strings.HasSuffix(mapped, ".enabled_tokens") && strings.Contains(v, ",") {
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
