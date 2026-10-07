package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	log "github.com/sirupsen/logrus"
)

// configContextKey is a distinct type so this key cannot collide with another
// package storing "config" in the same context.
type configContextKey string

// ConfigContextKey is the context key the CLI stores the loaded *Config under.
const ConfigContextKey configContextKey = "config"

// CredentialPosture says which provider credentials are accepted: sandbox or
// live. The zero value is unset, which New refuses.
type CredentialPosture string

const (
	CredentialPostureSandbox CredentialPosture = "sandbox"
	CredentialPostureLive    CredentialPosture = "live"
)

// ParseCredentialPosture reads a posture from text; empty is unset.
func ParseCredentialPosture(text string) (CredentialPosture, error) {
	switch p := CredentialPosture(strings.ToLower(strings.TrimSpace(text))); p {
	case "", CredentialPostureSandbox, CredentialPostureLive:
		return p, nil
	default:
		return "", fmt.Errorf("invalid test_mode %q: must be %q or %q", p, CredentialPostureSandbox, CredentialPostureLive)
	}
}

// Config is the engine's configuration: plain data. Everything that reaches
// outside the process is in Deps. TestMode and ProviderWriteMode are required.
type Config struct {
	// ProviderWriteMode is how much OpenRails may do against payment
	// providers: "full" (normal operation), "limited" (no system-initiated
	// provider writes) or "readonly" (no provider writes: it never charges
	// anyone). Required. Independent of TestMode.
	ProviderWriteMode string

	// TestMode selects sandbox or live provider credentials. Required. It
	// never relaxes authentication, transport security or credential
	// encryption.
	TestMode CredentialPosture

	// Schema is the Postgres schema OpenRails' tables live in. Empty is
	// "billing".
	Schema string
	// SchemaOwner is the role Migrate hands the schema and everything in it
	// to, when the logins OpenRails runs as inherit a shared owner rather than
	// being the migrating role. The role must exist. Empty: the migrating role
	// owns what it creates.
	SchemaOwner string
	// River selects who runs the job fleet. Zero is RiverManaged.
	River RiverOwnership
	// RiverSchema is the managed fleet's schema (default public). A host-owned
	// fleet keeps River's tables where the host's client puts them.
	RiverSchema string
	// Merchant declares the one merchant an embedded engine serves; zero
	// leaves the engine unbound (callers select a merchant per operation).
	Merchant MerchantDeclaration
	// Catalog declares Merchant's catalog (catalog.ParseApplicationYAML
	// of the host's catalog.yaml). New applies it before returning: unchanged
	// it replays, edited it converges. While it is set, writes to the
	// merchant's catalog are refused (billing.ErrCatalogDeclared); creator
	// catalogs and negotiated payer rates stay writable. Nil leaves the
	// catalog to the API.
	Catalog *catalog.Application
	// HTTP selects the route groups Client.Routes publishes; nil publishes none.
	HTTP *HTTPConfig
	// ControlPlane attaches the OpenRails-owned AuthKit control plane (the
	// standalone server and hosted products); nil for hosts with their own auth.
	ControlPlane *ControlPlaneConfig

	// PublicBillingBaseURL is the external billing mount, excluding /v1. It is
	// used only to build provider callbacks and customer billing links.
	PublicBillingBaseURL string
	// DashboardBaseURL is where administrative links point.
	DashboardBaseURL string

	// DB opens OpenRails' own pool when Deps.Postgres is nil.
	DB *DBConfig
	// Redis opens OpenRails' own client when Deps.Redis is nil.
	Redis *RedisConfig
	// Logger sets the log level.
	Logger *LoggerConfig
	// SendGrid selects the built-in SendGrid sender for billing and
	// control-plane email when Deps.EmailSender is nil; with neither, OpenRails
	// sends no email.
	SendGrid *SendGridConfig
	// RateLimits are the per-bucket request limits; nil takes the built-in
	// defaults.
	RateLimits *RateLimitsConfig
	// RateLimitsDisabled turns off the built-in rate limits and captcha
	// escalation, for a host that fronts billing with its own limiter.
	RateLimitsDisabled bool
	// Captcha challenges a client that keeps hitting a rate limit.
	Captcha *CaptchaConfig
	// Encryption holds the master key for credentials stored in the database.
	Encryption *EncryptionConfig
	// Vault connects to HashiCorp Vault when Deps.Vault is nil.
	Vault *VaultConfig

	// AdminConsole serves the merchant admin console. Off by default; enabled
	// without a console build (Deps.ConsoleAssets) New refuses.
	AdminConsole *AdminConsoleConfig

	// LLM is the model behind the console's natural-language widgets and
	// questions. Without an API key those features are off.
	LLM *LLMConfig

	// SecretBackend selects credential custody: SecretBackendSnapshot (the
	// default: supplied by the host, never persisted), SecretBackendVault or
	// SecretBackendDB (always encrypted). Vault never falls back to the
	// database.
	SecretBackend string
	// CredentialSnapshotID is a stable host-owned UUID identifying snapshot
	// custody. Required to move custody from managed to snapshot.
	CredentialSnapshotID string
	// CredentialReadOnly refuses managed credential writes even when the
	// backend would permit them.
	CredentialReadOnly bool
	// AlertSecretBackend selects vault or db custody for outbound webhook
	// credentials, independently of SecretBackend.
	AlertSecretBackend string
	// AllowCatalogUpdates enables the product, price, catalog and metering
	// mutation routes. An in-process Client applies its own catalog without
	// it; a declared Catalog still refuses the merchant's own catalog.
	AllowCatalogUpdates bool

	// CatalogReconciliationInterval schedules the alert-only catalog
	// reconciliation: a Go duration ("30m"). Empty is 1h; "0" disables it.
	CatalogReconciliationInterval string

	// ProviderBillingQuiescenceInterval is the minimum separation between two
	// equal provider billing observations before an operation authorization
	// may settle: a positive Go duration in whole seconds. Empty is 24h.
	ProviderBillingQuiescenceInterval string

	// WebhookSecretOverlap is how long a rotated-out webhook signing secret
	// keeps verifying: a Go duration between 1m and 168h. Empty is 24h.
	WebhookSecretOverlap string

	// ReturnOrigins are the exact origins (scheme://host[:port]) that checkout
	// and billing-portal return URLs may name. Empty allows only the origin of
	// PublicBillingBaseURL.
	ReturnOrigins []string

	// TrustedProxies are the CIDRs whose X-Forwarded-For is trusted when
	// resolving a client's address. Empty trusts none: the socket peer is the
	// client.
	TrustedProxies []string

	// CloudflareProxies are Cloudflare's egress CIDRs, set only where
	// Cloudflare fronts this origin. They are trusted like TrustedProxies, and
	// they alone may assert CF-Connecting-IP.
	CloudflareProxies []string

	// CCBillWebhookIPAllowlist are extra source CIDRs accepted as CCBill
	// webhook origins. CCBill signs nothing, so the source address is the
	// authentication: the list is honored only with TestMode sandbox and no
	// live CCBill PSP. Empty accepts CCBill's own ranges alone.
	CCBillWebhookIPAllowlist []string

	// ProviderSandbox points sandbox provider clients at loopback gateways, to
	// qualify a process against fake providers. Refused with TestMode live
	// and for anything but a literal loopback address.
	ProviderSandbox *ProviderSandboxConfig
	// HyperSwitch is the host's trusted HyperSwitch deployment.
	HyperSwitch *HyperSwitchConfig

	// EngineAdmissionHold pauses new renewal obligations, never receipt recovery.
	EngineAdmissionHold bool
}

// ProviderSandboxConfig names loopback provider gateways for sandbox runs.
// Each is an absolute http(s) URL whose host is a loopback IP literal, with no
// userinfo; hostnames, even "localhost", are refused.
type ProviderSandboxConfig struct {
	// NMIGatewayURL replaces the NMI sandbox endpoints.
	NMIGatewayURL string
	// StripeAPIURL replaces the Stripe API root. The readonly guard and the
	// pinned API version still apply.
	StripeAPIURL string
	// SolanaRPCURL replaces every merchant's Solana RPC endpoint.
	SolanaRPCURL string
	// CCBillDataLinkURL replaces the CCBill DataLink endpoint.
	CCBillDataLinkURL string
}

// ErrProviderSandboxGateway is the coded refusal for a provider_sandbox
// gateway declaration: a live posture, or a destination that is not a literal
// loopback address.
var ErrProviderSandboxGateway = errors.New("provider_sandbox gateway refused")

// SandboxNMIGatewayURL is the loopback NMI gateway declared for this sandbox
// run, or "" for the real sandbox endpoints.
func SandboxNMIGatewayURL(cfg *Config) string {
	if cfg == nil || cfg.ProviderSandbox == nil {
		return ""
	}
	return strings.TrimSpace(cfg.ProviderSandbox.NMIGatewayURL)
}

// SandboxStripeAPIURL is the configured loopback API, or empty for Stripe.
func SandboxStripeAPIURL(cfg *Config) string {
	if cfg == nil || cfg.ProviderSandbox == nil {
		return ""
	}
	return strings.TrimSpace(cfg.ProviderSandbox.StripeAPIURL)
}

// SandboxSolanaRPCURL is the configured loopback Solana RPC, or empty.
func SandboxSolanaRPCURL(cfg *Config) string {
	if cfg == nil || cfg.ProviderSandbox == nil {
		return ""
	}
	return strings.TrimSpace(cfg.ProviderSandbox.SolanaRPCURL)
}

// SandboxCCBillDataLinkURL is the configured loopback DataLink, or empty.
func SandboxCCBillDataLinkURL(cfg *Config) string {
	if cfg == nil || cfg.ProviderSandbox == nil {
		return ""
	}
	return strings.TrimSpace(cfg.ProviderSandbox.CCBillDataLinkURL)
}

// ValidateLoopbackGatewayURL accepts only a literal loopback destination: an
// absolute http(s) URL, no userinfo, whose host is an IP literal that
// net.IP.IsLoopback classifies. A hostname is never enough (no DNS).
func ValidateLoopbackGatewayURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%w: %q is not an absolute http(s) URL", ErrProviderSandboxGateway, raw)
	}
	if u.User != nil {
		return fmt.Errorf("%w: %q carries userinfo", ErrProviderSandboxGateway, raw)
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil {
		return fmt.Errorf("%w: host %q must be a loopback IP literal, not a hostname", ErrProviderSandboxGateway, u.Hostname())
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("%w: host %q is not a loopback address", ErrProviderSandboxGateway, u.Hostname())
	}
	return nil
}

func validateProviderSandbox(cfg *Config) error {
	for key, gateway := range map[string]string{"nmi_gateway_url": SandboxNMIGatewayURL(cfg), "stripe_api_url": SandboxStripeAPIURL(cfg), "solana_rpc_url": SandboxSolanaRPCURL(cfg), "ccbill_datalink_url": SandboxCCBillDataLinkURL(cfg)} {
		if gateway == "" {
			continue
		}
		if cfg.TestMode == CredentialPostureLive {
			return fmt.Errorf("%w: %s with test_mode=live never talks to a fake provider", ErrProviderSandboxGateway, key)
		}
		if err := ValidateLoopbackGatewayURL(gateway); err != nil {
			return fmt.Errorf("provider_sandbox.%s: %w", key, err)
		}
	}
	return nil
}

// CatalogReconciliationSchedule resolves the catalog reconciliation loop
// schedule: empty → 1h default, <=0 → disabled. A malformed value is an error
// — a typo must never silently pick a schedule (#712).
func CatalogReconciliationSchedule(cfg *Config) (interval time.Duration, enabled bool, err error) {
	raw := ""
	if cfg != nil {
		raw = strings.TrimSpace(cfg.CatalogReconciliationInterval)
	}
	if raw == "" {
		return time.Hour, true, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("catalog_reconciliation_interval %q is not a Go duration (e.g. 30m, 2h; 0 disables): %w", raw, err)
	}
	if d <= 0 {
		return 0, false, nil
	}
	return d, true, nil
}

// ProviderBillingQuiescence returns the policy used for new qualifications.
// The chosen duration is persisted per operation, so later config changes do
// not retroactively weaken an existing reservation's evidence requirement.
func ProviderBillingQuiescence(cfg *Config) (time.Duration, error) {
	raw := ""
	if cfg != nil {
		raw = strings.TrimSpace(cfg.ProviderBillingQuiescenceInterval)
	}
	if raw == "" {
		return 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("provider_billing_quiescence_interval %q is not a Go duration (e.g. 24h): %w", raw, err)
	}
	if d < time.Second {
		return 0, fmt.Errorf("provider_billing_quiescence_interval must be at least one second")
	}
	if d%time.Second != 0 {
		return 0, fmt.Errorf("provider_billing_quiescence_interval must use whole seconds")
	}
	return d, nil
}

const (
	SecretBackendSnapshot = "snapshot"
	SecretBackendDB       = "db"
	SecretBackendVault    = "vault"
)

// SecretStoreBackend returns the declared credential custody. Empty chooses an
// immutable host snapshot; unknown inputs remain invalid rather than selecting DB.
func SecretStoreBackend(cfg *Config) string {
	if cfg == nil || strings.TrimSpace(cfg.SecretBackend) == "" {
		return SecretBackendSnapshot
	}
	return strings.ToLower(strings.TrimSpace(cfg.SecretBackend))
}

// EncryptionConfig configures encryption at rest. The master key wraps each
// merchant's data key, which encrypts the credentials OpenRails stores in the
// database. SecretBackendDB requires it; host-owned snapshot credentials stay
// in memory and need none.
type EncryptionConfig struct {
	// MasterKey is the base64 of a 32-byte AES-256 key. Empty disables
	// encryption at rest.
	MasterKey string
}

// VaultConfig connects to HashiCorp Vault for merchant secrets and Solana
// Transit signing. The connection selects neither: SecretBackend selects
// secret storage, and each Solana PSP selects its signer.
type VaultConfig struct {
	Namespace   string
	ScopePrefix string
	Enabled     bool
	// Address is the server URL; empty uses the Vault client's default.
	Address string
	// AuthMethod is "token", "approle" or "kubernetes". Empty with a Token is
	// token.
	AuthMethod string
	// Token is a pre-issued Vault token.
	Token    string
	RoleID   string
	SecretID string
	K8sRole  string
	// KVMount is the KV-v2 mount merchant secrets live under; empty is
	// "secret".
	KVMount string
	// TransitMount is the Transit mount Solana signing keys live under; empty
	// is "transit".
	TransitMount string
}

// AdminConsoleConfig serves the merchant admin console at Path, with a
// Path/config.json document the console reads to find its auth and API bases.
type AdminConsoleConfig struct {
	Enabled bool
	// Path is where the console is served: an absolute URL path without a
	// trailing slash, such as "/billing/admin". Empty is "/admin". An embedded
	// host mounts Client.AdminConsole at exactly this path.
	Path string
	// AuthBaseURL is the base of the AuthKit HTTP API the console signs in
	// through. Empty is the control plane's, "/auth/v1". Embedded hosts set
	// their AuthKit API, "/api/v1" by default; it may be absolute.
	AuthBaseURL string
	// APIBaseURL is the base of the merchant API. Empty is "/v1"; embedded
	// hosts typically use "/billing/v1".
	APIBaseURL string
}

// AdminConsoleEnabled reports whether the admin console SPA should be served.
func AdminConsoleEnabled(c *AdminConsoleConfig) bool { return c != nil && c.Enabled }

// DefaultAdminConsolePath is where the console is served when Path is unset.
const DefaultAdminConsolePath = "/admin"

// AdminConsoleMountPath is Path, defaulted to DefaultAdminConsolePath.
func AdminConsoleMountPath(c *AdminConsoleConfig) string {
	if c == nil || c.Path == "" {
		return DefaultAdminConsolePath
	}
	return c.Path
}

var adminConsolePathRe = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)

// validateAdminConsolePath accepts one or more slash-led segments of RFC 3986
// unreserved characters, none of them "." or "..": the value lands in route
// patterns and the console's <base href>.
func validateAdminConsolePath(p string) error {
	bad := !adminConsolePathRe.MatchString(p)
	for _, segment := range strings.Split(p, "/") {
		bad = bad || segment == "." || segment == ".."
	}
	if bad {
		return fmt.Errorf("invalid admin_console.path %q: want an absolute path like /billing/admin — no trailing slash, no . or .. segments, only letters, digits and . _ ~ -", p)
	}
	return nil
}

// LLM provider names (#741/#761). The provider selects the API dialect;
// unknown values refuse to boot.
const (
	LLMProviderAnthropic = "anthropic"
	LLMProviderOpenAI    = "openai"
)

// Per-provider default models when llm.model is unset.
// Cheapest-capable-first (Paul): query generation is designed to need no model
// cleverness — /schema + corrective errors carry the intelligence. Configure a
// bigger model only if generation quality measurably demands it.
const (
	LLMDefaultModelAnthropic = "claude-haiku-4-5-20251001"
	// gpt-5.4-nano is the cheapest current-generation tool-capable OpenAI
	// model ($0.20/M in, $1.25/M out as of 2026-07). gpt-4.1-nano is nominally
	// cheaper but the 4.1 family is mid-retirement — a default must not 404.
	LLMDefaultModelOpenAI = "gpt-5.4-nano"
)

// LLMConfig configures the model behind the console's natural-language
// widgets and questions.
type LLMConfig struct {
	// Provider selects the API dialect: "anthropic" (the default) or
	// "openai". Any other value is refused.
	Provider string
	// Model is the provider's model ID; empty is the provider's default.
	Model string
	// BaseURL replaces the provider's public endpoint: an absolute https URL.
	// With openai it includes the version segment
	// (https://host/v1); with anthropic it is the origin.
	BaseURL string
	// APIKey is the provider credential. Empty turns the features off.
	APIKey string
	// AskEnabled turns on questions about metrics, which send aggregate query
	// results to the provider (widget generation sends only the schema).
	AskEnabled bool
	// CatalogCopilotEnabled turns on read-only questions about the catalog,
	// which send aggregate catalog and subscriber counts to the provider.
	CatalogCopilotEnabled bool
	// CatalogDraftingEnabled lets the catalog copilot draft price and catalog
	// changes. Drafts are proposals; nothing is applied.
	CatalogDraftingEnabled bool
}

// LLMConfigured reports whether NL widget generation can run (fail-closed on a
// missing key).
func LLMConfigured(c *LLMConfig) bool { return c != nil && strings.TrimSpace(c.APIKey) != "" }

// LLMAskConfigured reports whether metrics Q&A can run: a key AND the explicit
// ask_enabled consent (results flow to the provider — never implied by the key).
func LLMAskConfigured(c *LLMConfig) bool { return LLMConfigured(c) && c.AskEnabled }

// LLMCatalogCopilotConfigured reports whether catalog Q&A (#779 Phase 1) can
// run: a key AND the explicit catalog_copilot_enabled consent.
func LLMCatalogCopilotConfigured(c *LLMConfig) bool {
	return LLMConfigured(c) && c.CatalogCopilotEnabled
}

// LLMCatalogDraftingConfigured reports whether the #779 Phase 2 draft_* tools
// are armed: catalog Q&A configured and explicit catalog_drafting_enabled
// consent.
func LLMCatalogDraftingConfigured(c *LLMConfig) bool {
	return LLMCatalogCopilotConfigured(c) && c.CatalogDraftingEnabled
}

// LLMProvider returns the effective provider name.
func LLMProvider(c *LLMConfig) string {
	if c == nil || strings.TrimSpace(c.Provider) == "" {
		return LLMProviderAnthropic
	}
	return strings.ToLower(strings.TrimSpace(c.Provider))
}

// LLMModel returns the effective model id (per-provider default).
func LLMModel(c *LLMConfig) string {
	if c != nil && strings.TrimSpace(c.Model) != "" {
		return strings.TrimSpace(c.Model)
	}
	if LLMProvider(c) == LLMProviderOpenAI {
		return LLMDefaultModelOpenAI
	}
	return LLMDefaultModelAnthropic
}

// DBConfig is a Postgres connection. URL takes precedence; otherwise the
// connection string is built from the parts.
type DBConfig struct {
	URL string

	Host     string
	Port     string
	Database string
	Username string
	Password string
	// SSLMode defaults to "require".
	SSLMode string

	// SQLTrace logs every query at debug level on pools OpenRails opens.
	SQLTrace bool
}

// DBConnectionString returns the database connection string.
// Priority order:
// 1. If URL is set, use it directly
// 2. If all atomic parameters are present, build connection string from them
// 3. Return empty string (caller should use defaults or error based on environment)
func DBConnectionString(c *DBConfig) string {
	// 1. If URL is provided, use it
	if c.URL != "" {
		return c.URL
	}

	// 2. Build connection string from atomic parameters if all required fields are present
	if c.Host != "" && c.Port != "" && c.Database != "" && c.Username != "" {
		// Format: postgresql://username:password@host:port/database?sslmode=...
		//
		// Credentials and the database name are percent-encoded via net/url
		// rather than interpolated raw. A password containing reserved URL
		// characters (most dangerously '@' or '/') would otherwise corrupt the
		// DSN — e.g. a password "p@ss/0" makes the parser read a different host,
		// which at best fails to connect and at worst silently redirects the
		// connection to an attacker-influenced endpoint. url.UserPassword +
		// url.URL.String() escape every component correctly.
		sslMode := c.SSLMode
		if sslMode == "" {
			// Default to TLS when sslmode is omitted. Local compose sets disable explicitly.
			sslMode = "require"
		}
		u := url.URL{
			Scheme:   "postgresql",
			User:     url.UserPassword(c.Username, c.Password),
			Host:     net.JoinHostPort(c.Host, c.Port),
			Path:     "/" + c.Database,
			RawQuery: url.Values{"sslmode": {sslMode}}.Encode(),
		}
		return u.String()
	}

	// 3. No URL and incomplete atomic parameters - return empty (caller handles defaults)
	return ""
}

// DefaultSchema is the Postgres schema used when none is configured. All SQL
// is authored in it; any other schema is reached by internal/sqlschema.
const DefaultSchema = "billing"

// MigratekitApp is the migratekit app/tracking key written to
// public.migrations.app for OpenRails' own (non-River, non-AuthKit) migrations.
// It is the "app name" #471 standardized to
// "openrails" (from the historical "billing"). It is deliberately independent of
// the (configurable) schema name — the embedded engine's boot validation greps
// for this value, so hosts must keep it in lockstep.
const MigratekitApp = "openrails"

// DefaultRiverSchema is the default namespace for managed River tables.
const DefaultRiverSchema = "public"

// schemaIdentRe restricts the OpenRails schema to a safe SQL identifier: it must
// start with a letter or underscore and contain only letters, digits, and
// underscores. This forbids quotes, spaces, and dots, so the value can be used to
// build search_path / River schema names without quoting hazards.
var schemaIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SchemaName returns the effective OpenRails Postgres schema (#165, #471):
// Schema trimmed and lower-cased, default billing. Code that needs the schema
// reads it here, never Schema directly.
func SchemaName(c *Config) string {
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
		return fmt.Errorf("schema %q is not a valid Postgres identifier (letters, digits, underscore only; must start with a letter or underscore)", raw)
	}
	return nil
}

// PSP environments (#641): the credentials' nature. A deployment is
// all-test OR all-live; test_mode is the switch.
const (
	ProviderEnvironmentTest = "test"
	ProviderEnvironmentLive = "live"
)

// ExpectedProviderEnvironment is the environment every PSP runs in
// for the given test_mode: test under sandbox, live in production. It is DERIVED
// (#882) — a PSP never declares it.
func ExpectedProviderEnvironment(testMode bool) string {
	if testMode {
		return ProviderEnvironmentTest
	}
	return ProviderEnvironmentLive
}

// ReservedPSPRails maps a PSP name to the rail it implies, so a
// config entry named after a self-contained gateway need not restate its rail.
var ReservedPSPRails = map[string]models.Rail{
	"ccbill": models.RailCCBill,
	"stripe": models.RailStripe,
	"solana": models.RailSolana,
}

// ResolvedPSP is one configured PSP: the rail (gateway) it
// is on plus that rail's credentials. The map key in a PSPSet is the
// operator-chosen account NAME (e.g. "mobius", "paykings" on rail nmi).
//
// For an account named after a self-contained gateway (ccbill, stripe, solana) the
// rail is inferred from the name; other names (e.g. "mobius") must set Rail.
//
// PROGRAMMATIC-ONLY (#521/#711): no yaml/env loader parses these structs.
// Embedded hosts build them in code (a PSP declaration); standalone
// declares rail accounts in the merchant config manifest instead.
type ResolvedPSP struct {
	// ID is the immutable psps row id. Resolution OUTPUT only; zero for static sets.
	ID uuid.UUID
	// Key is the merchant's PSP key for this account (psps.key / the manifest
	// `psps.<key>` map name, e.g. "mobius"). Resolution OUTPUT only — set by
	// railresolve sources, never declared inside the entry itself.
	Key string
	// Rail is the gateway this account is on: nmi, ccbill, stripe, solana.
	// Required unless the account name is itself a reserved gateway name.
	Rail models.Rail
	// AccountID is this account's rail-native identity (#641/#655): NMI
	// gateway-id, Stripe acct_…, CCBill clientAccnum-clientSubacc (dash-joined,
	// e.g. 999999-0000, #697), or Solana wallet (#592: operator-declared).
	// REQUIRED — ValidateRailSet rejects an empty one.
	AccountID string
	// Archived keeps an account addressable for existing obligations and inbound
	// provider events, but excludes it from new checkout/subscription work.
	Archived bool

	// Exactly one provider block is set, matching Type. Credentials live ONLY in
	// the typed block — there is no flat fallback.
	NMI    *NMIRailConfig
	CCBill *CCBillRailConfig
	Stripe *StripeRailConfig
	Solana *SolanaRailConfig

	// Custodian names the declared custodian (custodians.<key>) holding the
	// instruments charged through this PSP. "" = the PSP holds its own.
	// DECLARATION input; Custody below is what it resolves to.
	Custodian string

	// Custody (or#879/or#880) is the axis orthogonal to the rail: nil means
	// the PSP holds its own instruments. A third-party custodian holds the
	// card and proxies it into THIS PSP's gateway.
	Custody *ResolvedCustodian
}

// NMIRailConfig — programmatic-only (see PSPConfig). Field
// names match the store/manifest canonical secret keys (#711).
type NMIRailConfig struct {
	EndpointDeployment   string
	SecurityKey          string
	WebhookSigningSecret string
}

// CCBillRailConfig — programmatic-only. The clientAccnum/clientSubacc pair is
// NOT declared here: it is derived from the account's dash-joined AccountID
// (#697/#711), exactly like the store plane, so identity is declared once.
type CCBillRailConfig struct {
	Salt             string
	DataLinkUsername string
	DataLinkPassword string
}

// StripeRailConfig — programmatic-only. Field names match the store/manifest
// canonical secret keys (#711).
type StripeRailConfig struct {
	SecretKey            string
	WebhookSigningSecret string
	// WebhookSigningSecretThin is the signing secret of a Stripe "thin" Event
	// Destination; when set, webhooks verify against either secret.
	WebhookSigningSecretThin string
}

// SolanaRailConfig — programmatic-only boot plane. Standalone declares the same
// knobs per merchant in the manifest rail-account `settings` block (#711, see
// SolanaAccountSettings); store settings win over this boot plane (#699).
type SolanaRailConfig struct {
	// RPCProvider selects the preferred Solana RPC provider. Empty defaults to
	// "helius"; without rpc_api_key the client uses public RPC fallback.
	RPCProvider string
	// RPCAPIKey is the API key for the selected RPC provider (currently helius).
	RPCAPIKey string
	Tokens    map[string]TokenConfig
	// Network is DERIVED from test_mode at startup (devnet under test_mode,
	// mainnet otherwise) — not configurable (#349).
	Network string
}

// ResolvedCustodian (#795 / or#879 / or#880) is the RESOLVED runtime shape of a
// declared custodian: who holds the card, under what tenant identity, and the
// credentials to detokenize it into a gateway. It is resolved from ONE
// custodians row and may be shared by every PSP that references it — the
// gateway credentials themselves stay on the PSP, one source of truth.
type ResolvedCustodian struct {
	// Key is the merchant's name for this custodian (custodians.key).
	Key string
	// Custodian is the vendor KIND (models.CustodianBasisTheory) — the value
	// stamped on payment_methods.custodian.
	Custodian string
	// AccountID is the custodian-native tenant id (operator-declared).
	AccountID string
	// APIKey is the custodian's PRIVATE application key — the only custodial
	// secret; it authorizes detokenization, never a charge on its own.
	APIKey string
	// PublicAPIKey is checkout-page config (not a secret).
	PublicAPIKey string
	// ProfileID is the merchant-owned HyperSwitch business profile.
	ProfileID string
	// NetworkTokens arms NT provisioning on instrument creation (never
	// load-bearing; charge routing stays pan_proxy on NMI gateways).
	NetworkTokens bool
	// AccountUpdater arms the batch account-updater cycle (or#795 — the
	// contracted add-on that refreshes the FPAN we actually charge).
	AccountUpdater bool
	// AccountUpdaterLookaheadDays is how far ahead of a renewal an instrument
	// is refreshed, and the same window it then stays fresh for.
	AccountUpdaterLookaheadDays int
	// WebhookKeyURL overrides the custodian's CDN webhook public-key URL (tests only).
	WebhookKeyURL string
	// APIBaseURL overrides the custodian API base URL (tests only; "" = production).
	APIBaseURL string
}

// PSPSet is an in-memory set of payment-provider credential entries. It
// is not part of config.yaml/.env; private standalone installs seed provider
// credentials through merchant bootstrap/Vault state, and embedded hosts may
// pass a set programmatically during construction.
type PSPSet map[string]*ResolvedPSP

// EffectiveRail returns the account's rail (gateway), inferring it from a reserved
// account name when Rail is unset.
func (p *ResolvedPSP) EffectiveRail(name string) models.Rail {
	if p.Rail != "" {
		return models.Rail(strings.ToLower(string(p.Rail)))
	}
	normalizedName := strings.ToLower(strings.TrimSpace(name))
	if rail, ok := ReservedPSPRails[normalizedName]; ok {
		return rail
	}
	return ""
}

// EffectiveAccountID is the account's operator-declared rail-native identity
// (#641). ValidateRailSet requires account_id — there is NO fallback to the
// config map name (an account is never indexed by a name we made up).
func (p *ResolvedPSP) EffectiveAccountID() string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.AccountID)
}

// ValidateRailAccountID rejects rail-specific malformed account_id values.
// CCBill composite identity is dash-joined (#697): clientAccnum-clientSubacc,
// matching CCBill's own convention — a slash would also re-embed the
// merchant-secret path delimiter inside the id. Format-only; empty ids are
// handled by the callers' requiredness rules.
func ValidateRailAccountID(rail models.Rail, accountID string) error {
	if rail == models.RailCCBill && strings.Contains(accountID, "/") {
		return fmt.Errorf("CCBill account_id uses a dash: clientAccnum-clientSubacc, e.g. 999999-0000 (got %q)", accountID)
	}
	return nil
}

func (p *ResolvedPSP) normalizeTypedBlock(name string) error {
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
func (p *ResolvedPSP) IsNMI(name string) bool {
	return p.EffectiveRail(name) == models.RailNMI
}

// IsCCBill returns true if this rail config is for CCBill.
func (p *ResolvedPSP) IsCCBill(name string) bool {
	return p.EffectiveRail(name) == models.RailCCBill
}

// IsStripe returns true if this rail config is for Stripe.
func (p *ResolvedPSP) IsStripe(name string) bool {
	return p.EffectiveRail(name) == models.RailStripe
}

// IsSolana returns true if this rail config is for Solana.
func (p *ResolvedPSP) IsSolana(name string) bool {
	return p.EffectiveRail(name) == models.RailSolana
}

// HasThirdPartyCustody reports whether a custodian other than the PSP holds
// the instruments charged through this PSP (or#879).
func (p *ResolvedPSP) HasThirdPartyCustody() bool {
	return p != nil && p.Custody != nil && p.Custody.Custodian != "" && p.Custody.Custodian != models.CustodianPSP
}

// CustodianKey is the declared custodian reference for this PSP, normalized.
func (p *ResolvedPSP) CustodianKey() string {
	if p == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(p.Custodian))
}

// ToNMIProviderSettings converts the rail config to NMI client settings.
// Only valid for NMI-type rails.
func (p *ResolvedPSP) ToNMIProviderSettings() *NMIProviderSettings {
	s := &NMIProviderSettings{}
	if p.NMI != nil {
		s.SecurityKey = p.NMI.SecurityKey
		s.WebhookSecret = p.NMI.WebhookSigningSecret
		s.EndpointDeployment = p.NMI.EndpointDeployment
	}
	return s
}

// SplitCCBillAccountID splits the dash-joined CCBill composite identity
// (clientAccnum-clientSubacc, #697) at the FIRST dash — both parts are
// numeric in CCBill's own convention, so the first dash is the separator.
func SplitCCBillAccountID(accountID string) (accNum, subAcc string, err error) {
	acc, sub, ok := strings.Cut(strings.TrimSpace(accountID), "-")
	acc, sub = strings.TrimSpace(acc), strings.TrimSpace(sub)
	if !ok || acc == "" || sub == "" {
		return "", "", fmt.Errorf("CCBill account_id uses a dash: clientAccnum-clientSubacc, e.g. 999999-0000 (got %q)", accountID)
	}
	return acc, sub, nil
}

// ToCCBillConfig converts the rail config to the CCBill client config. Only
// valid for CCBill-type rails. The clientAccnum/clientSubacc pair is derived
// from the declared AccountID (#711 — identity is declared once); a malformed
// AccountID leaves the pair empty and is rejected by validateCCBillRail.
func (p *ResolvedPSP) ToCCBillConfig() *CCBillConfig {
	c := &CCBillConfig{} // TestMode set by caller based on global test_mode
	if acc, sub, err := SplitCCBillAccountID(p.EffectiveAccountID()); err == nil {
		c.ClientAccNum = acc
		c.ClientSubAcc = sub
	}
	if p.CCBill != nil {
		c.Salt = p.CCBill.Salt
		c.DataLinkUsername = p.CCBill.DataLinkUsername
		c.DataLinkPassword = p.CCBill.DataLinkPassword
	}
	return c
}

// NMIProviderSettings is what the NMI client actually reads (#710): the
// credential pair. Sandbox posture is nmi.NewClient's testMode argument.
type NMIProviderSettings struct {
	EndpointDeployment string
	SecurityKey        string
	WebhookSecret      string
}

// CCBillConfig is the derived CCBill CLIENT config (programmatic-only): the
// account pair split out of the dash-joined account_id plus credentials.
type CCBillConfig struct {
	Salt         string
	ClientSubAcc string
	ClientAccNum string
	TestMode     bool

	DataLinkUsername string
	DataLinkPassword string
}

// RedisConfig is a Redis connection.
type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

// TokenConfig defines configuration for a specific Solana token.
//
// There is deliberately NO `decimals` field (#817): an SPL token's base-unit
// precision is a property of the MINT on-chain, immutable since InitializeMint,
// and the chain is the source of truth. A merchant-settable copy is a field
// they can get wrong, and a wrong value misprices every charge by a power of
// ten. Decimals are read from the mint (internal/integrations/solana.
// ReadMintDecimals) and cached, never declared.
type TokenConfig struct {
	Mint string `json:"mint"` // Token mint address accepted on the configured Solana network.
	Name string `json:"name"` // Token name.
}

// Payable base-unit precision bounds for an SPL mint. Applied to the value READ
// FROM THE MINT, not to configuration. SPL mints top out at 9 in practice; the
// slack accommodates exotic mints while still refusing an absurd 10^n rescale.
// 0 is rejected: a whole-unit token cannot represent a sub-unit charge, so it is
// not usable as a payment token here.
const (
	MinTokenDecimals = 1
	MaxTokenDecimals = 18
)

// ValidateTokenDecimals rejects an on-chain precision unusable for payments.
func ValidateTokenDecimals(mintOrSymbol string, decimals int) error {
	if decimals < MinTokenDecimals || decimals > MaxTokenDecimals {
		return fmt.Errorf("solana token %s: on-chain mint decimals %d are outside the payable range %d..%d",
			mintOrSymbol, decimals, MinTokenDecimals, MaxTokenDecimals)
	}
	return nil
}

// RateLimitsConfig maps a feature bucket to its request limit.
// A bucket left out is not limited; other routes never are (a
// per-address ceiling belongs to the proxy in front of OpenRails).
type RateLimitsConfig map[string]*RateLimit

// RateLimitBuckets are the buckets RateLimitsConfig may name.
var RateLimitBuckets = []string{"checkout", "payment", "subscribe", "webhook", "metrics-ask", "catalog-ask", "dashboard-generate"}

// Provider write modes (#346, #355) — see Config.ProviderWriteMode. The former
// mode=test is gone (sandbox is the orthogonal test_mode axis) and "production"
// is renamed "full".
const (
	ProviderWriteModeFull     = "full"
	ProviderWriteModeLimited  = "limited"
	ProviderWriteModeReadOnly = "readonly"
)

// ValidProviderWriteModes contains all valid provider write values ("" = unset;
// dev only).
var ValidProviderWriteModes = map[string]bool{
	"":                        true,
	ProviderWriteModeFull:     true,
	ProviderWriteModeLimited:  true,
	ProviderWriteModeReadOnly: true,
}

// SendGridConfig is the SendGrid account OpenRails' email is sent through.
// Billing email is sent from the merchant's profile from_email when it has
// one; From is the deployment's own address, for everything else.
type SendGridConfig struct {
	APIKey string
	From   EmailAddress
}

// LoggerConfig sets the log level: debug, info, warn or error.
type LoggerConfig struct {
	Level string
}

// RateLimit is one bucket's limit over a fixed one-minute window.
type RateLimit struct {
	RequestsPerMinute int
}

const (
	CaptchaProviderTurnstile   = "turnstile"
	CaptchaProviderRecaptchaV3 = "recaptcha-v3"
	CaptchaProviderHCaptcha    = "hcaptcha"
)

// CaptchaConfig is the captcha account. Setting both keys enables the
// challenge, which is asked only of a client that keeps hitting a rate limit
// on checkout, payment methods or subscriptions.
type CaptchaConfig struct {
	// Provider is "turnstile" (the default), "recaptcha-v3" or "hcaptcha".
	Provider  string
	SiteKey   string
	SecretKey string
}

// CaptchaEnabled reports whether captcha challenges are active. There is no
// enabled knob (#353): configuring the credentials IS the enablement signal —
// the system already applies captcha selectively (only after extreme
// rate-limit escalation, only on the challenged buckets).
func CaptchaEnabled(c *CaptchaConfig) bool {
	return c != nil && strings.TrimSpace(c.SiteKey) != "" && strings.TrimSpace(c.SecretKey) != ""
}

func CaptchaProvider(c *CaptchaConfig) string {
	if c == nil || strings.TrimSpace(c.Provider) == "" {
		return CaptchaProviderTurnstile
	}
	return strings.ToLower(strings.TrimSpace(c.Provider))
}

func CaptchaVerifyURL(c *CaptchaConfig) string {
	switch CaptchaProvider(c) {
	case CaptchaProviderRecaptchaV3:
		return "https://www.google.com/recaptcha/api/siteverify"
	case CaptchaProviderHCaptcha:
		return "https://hcaptcha.com/siteverify"
	default:
		return "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	}
}

func CaptchaScriptURL(c *CaptchaConfig) string {
	switch CaptchaProvider(c) {
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

// Captcha policy is fixed: protocol constants, not deployment choices.
const (
	CaptchaChallengeTTL      = 15 * time.Minute
	CaptchaExtremeMultiplier = 3
	CaptchaMinScore          = 0.5
	CaptchaAction            = "billing_challenge"
)

// CaptchaChallengeBuckets are the rate-limit buckets a challenge guards.
func CaptchaChallengeBuckets() []string {
	return []string{"checkout", "payment-methods", "subscriptions"}
}

// Validate validates the billing configuration
func Validate(cfg *Config) error {
	// Credential posture never relaxes security validation.
	// Provider write mode must be a known value — a typo (e.g. "redaonly") must
	// never silently boot with full behavior (#346).
	providerWriteMode := cfg.normalizedProviderWriteMode()
	if !ValidProviderWriteModes[providerWriteMode] {
		return fmt.Errorf("invalid provider_write_mode %q: must be one of full, limited, readonly", providerWriteMode)
	}

	// Malformed catalog_reconciliation_interval refuses to boot (#712): the old
	// env knob silently fell back to 1h on a typo.
	if _, _, err := CatalogReconciliationSchedule(cfg); err != nil {
		return err
	}
	if _, err := ProviderBillingQuiescence(cfg); err != nil {
		return err
	}

	// A typo never silently takes an unrecognized branch.
	switch cfg.TestMode {
	case CredentialPostureSandbox, CredentialPostureLive:
	case "":
		return fmt.Errorf("test_mode is required: choose sandbox or live")
	default:
		return fmt.Errorf("invalid test_mode %q: must be %q or %q", cfg.TestMode, CredentialPostureSandbox, CredentialPostureLive)
	}

	if cfg.RateLimits == nil && !cfg.RateLimitsDisabled {
		return fmt.Errorf("rate_limits is required unless rate_limits_disabled is explicitly set for a host-owned limiter")
	}
	if cfg.RateLimits != nil {
		for bucket := range *cfg.RateLimits {
			if !slices.Contains(RateLimitBuckets, bucket) {
				return fmt.Errorf("rate_limits.%s is not a bucket (%s); a per-address ceiling belongs to the proxy in front of OpenRails", bucket, strings.Join(RateLimitBuckets, ", "))
			}
		}
	}
	if err := validateCaptcha(cfg.Captcha); err != nil {
		return fmt.Errorf("captcha config validation failed: %w", err)
	}
	if cfg.AdminConsole != nil && cfg.AdminConsole.Path != "" {
		if err := validateAdminConsolePath(cfg.AdminConsole.Path); err != nil {
			return err
		}
	}

	// #741/#761: an unknown llm.provider must never silently boot with one
	// vendor's dialect pointed at another vendor's key.
	if cfg.LLM != nil {
		switch LLMProvider(cfg.LLM) {
		case LLMProviderAnthropic, LLMProviderOpenAI:
		default:
			return fmt.Errorf("invalid llm.provider %q: must be %q or %q", cfg.LLM.Provider, LLMProviderAnthropic, LLMProviderOpenAI)
		}
		// Same root-of-trust posture as the auth issuer: plaintext HTTP to
		// the model endpoint (prompts + aggregate results in flight) is
		// prohibited.
		if base := strings.TrimSpace(cfg.LLM.BaseURL); base != "" {
			u, err := url.Parse(base)
			if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("invalid llm.base_url %q: must be an absolute http(s) URL", base)
			}
			if u.Scheme != "https" {
				return fmt.Errorf("llm.base_url %q must use https", base)
			}
		}
	}

	// Always validate database configuration
	if err := ValidateDatabase(cfg); err != nil {
		return fmt.Errorf("database config validation failed: %w", err)
	}

	if err := validateEncryption(cfg.Encryption); err != nil {
		return fmt.Errorf("encryption config validation failed: %w", err)
	}

	if err := validateHyperSwitch(cfg); err != nil {
		return err
	}
	if err := validateProviderSandbox(cfg); err != nil {
		return err
	}
	if err := validateSecretBackend(cfg); err != nil {
		return fmt.Errorf("secret_backend config validation failed: %w", err)
	}
	if err := validateSecurityPolicy(cfg); err != nil {
		return err
	}

	if err := validateSourceCIDRs(cfg.TrustedProxies); err != nil {
		return fmt.Errorf("trusted_proxies config validation failed: %w", err)
	}

	if err := validateSourceCIDRs(cfg.CloudflareProxies); err != nil {
		return fmt.Errorf("cloudflare_proxies config validation failed: %w", err)
	}

	if err := validateSourceCIDRs(cfg.CCBillWebhookIPAllowlist); err != nil {
		return fmt.Errorf("ccbill_webhook_ip_allowlist config validation failed: %w", err)
	}

	return nil
}

// validateSourceCIDRs rejects a malformed CIDR at boot — a typo must never
// silently no-op into an unintended trust posture (#746) — and, deliberately,
// a /0 prefix. Both lists it guards feed transport-level authentication
// (SEC-19): trusting 0.0.0.0/0 as a proxy makes every X-Forwarded-For
// authoritative, and allowlisting ::/0 as a CCBill source accepts a forged
// callback from anywhere — CCBill has no HMAC, so the source IP is its only
// authentication. An operator who wants "everything" must say so one honest
// CIDR at a time.
func validateSourceCIDRs(cidrs []string) error {
	for _, raw := range cidrs {
		trimmed := strings.TrimSpace(raw)
		_, ipNet, err := net.ParseCIDR(trimmed)
		if err != nil {
			return fmt.Errorf("invalid CIDR %q (expected e.g. \"10.0.0.0/8\"): %w", raw, err)
		}
		if ones, _ := ipNet.Mask.Size(); ones == 0 {
			return fmt.Errorf("CIDR %q matches every address; enumerate the real ranges instead", raw)
		}
	}
	return nil
}

// validateSecretBackend checks declared custody. A live Vault connection may be
// supplied by an embedded host; backend construction verifies its actual access.
func validateSecretBackend(cfg *Config) error {
	if cfg.CredentialSnapshotID != "" {
		id, err := uuid.Parse(cfg.CredentialSnapshotID)
		if err != nil || id == uuid.Nil || id.String() != cfg.CredentialSnapshotID {
			return fmt.Errorf("credential_snapshot_id must be a canonical nonzero UUID")
		}
	}
	switch strings.ToLower(strings.TrimSpace(cfg.SecretBackend)) {
	case "", SecretBackendSnapshot, SecretBackendDB, SecretBackendVault:
	default:
		return fmt.Errorf("secret_backend must be snapshot, db or vault")
	}
	switch cfg.AlertSecretBackend {
	case "", SecretBackendDB, SecretBackendVault:
	default:
		return fmt.Errorf("alert_secret_backend must be db or vault when configured")
	}
	if SecretStoreBackend(cfg) == SecretBackendDB || cfg.AlertSecretBackend == SecretBackendDB {
		if cfg.Encryption == nil || strings.TrimSpace(cfg.Encryption.MasterKey) == "" {
			return fmt.Errorf("DB credential storage requires encryption.master_key")
		}
	}
	return nil
}

func validateCaptcha(cfg *CaptchaConfig) error {
	if cfg == nil {
		return nil
	}

	switch CaptchaProvider(cfg) {
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
// An empty key is legitimate for host-owned provider credentials or Vault.
// The managed DB store enforces and reports its actual encryption posture;
// syntax validation cannot infer that any secret will be persisted.
func validateEncryption(cfg *EncryptionConfig) error {
	if cfg == nil || strings.TrimSpace(cfg.MasterKey) == "" {
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

// ValidateRailSet checks every declared rail and its credential posture.
func ValidateRailSet(cfg *Config, rails PSPSet) error {
	if len(rails) == 0 {
		return nil
	}
	if err := validateRails(cfg, rails); err != nil {
		return fmt.Errorf("rails validation failed: %w", err)
	}
	return validateStripeKeyForTestMode(cfg, rails)
}

// validateStripeKeyForTestMode enforces the two credential/test_mode
// mismatches symmetrically: a live key can never run under sandbox posture,
// and a test key can never run under live posture. A mismatch must fail boot
// rather than silently disable a configured rail.
func validateStripeKeyForTestMode(cfg *Config, rails PSPSet) error {
	if cfg == nil {
		cfg = &Config{}
	}
	for name, stripeProc := range rails {
		if stripeProc == nil || stripeProc.EffectiveRail(name) != models.RailStripe || stripeProc.Stripe == nil {
			continue
		}

		if err := ValidateStripeCredentialPosture(cfg, stripeProc.Stripe.SecretKey); err != nil {
			return fmt.Errorf("stripe rail %q: %w", strings.ToLower(strings.TrimSpace(name)), err)
		}
	}
	return nil
}

// ValidateStripeCredentialPosture checks a single supplied Stripe key against
// the runtime posture without requiring a complete account or probing Stripe.
func ValidateStripeCredentialPosture(cfg *Config, secretKey string) error {
	secretKey = strings.TrimSpace(secretKey)
	isLiveKey := strings.HasPrefix(secretKey, "sk_live_") || strings.HasPrefix(secretKey, "rk_live_")
	isTestKey := strings.HasPrefix(secretKey, "sk_test_") || strings.HasPrefix(secretKey, "rk_test_")
	if secretKey != "" && ((!isLiveKey && !isTestKey) || len(secretKey) <= len("sk_test_")) {
		return fmt.Errorf("invalid Stripe secret key format: expected sk_live_, rk_live_, sk_test_, or rk_test_ with a nonempty value")
	}
	if cfg != nil && IsTestMode(cfg) {
		if isLiveKey {
			return fmt.Errorf("live key (sk_live_/rk_live_) is not allowed when test_mode=sandbox; use a test key or set test_mode=live")
		}
	} else if isTestKey {
		return fmt.Errorf("test key (sk_test_/rk_test_) is not allowed when test_mode=live; use a live key or set test_mode=sandbox")
	}
	return nil
}

// ValidateStripePublishableKeyPosture checks a declared browser key the same
// way: pk_test_ only under sandbox, pk_live_ only under live.
func ValidateStripePublishableKeyPosture(cfg *Config, key string) error {
	key = strings.TrimSpace(key)
	live, test := strings.HasPrefix(key, "pk_live_"), strings.HasPrefix(key, "pk_test_")
	if (!live && !test) || len(key) <= len("pk_test_") {
		return fmt.Errorf("invalid Stripe publishable key format: expected pk_live_ or pk_test_ with a nonempty value")
	}
	if cfg != nil && IsTestMode(cfg) {
		if live {
			return fmt.Errorf("live publishable key (pk_live_) is not allowed when test_mode=sandbox")
		}
	} else if test {
		return fmt.Errorf("test publishable key (pk_test_) is not allowed when test_mode=live")
	}
	return nil
}

// validateRails validates all rails in the new Rails map
func validateRails(cfg *Config, rails PSPSet) error {
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
			if err := validateNMIRail(name, proc); err != nil {
				return err
			}
		case models.RailCCBill:
			if err := validateCCBillRail(name, proc); err != nil {
				return err
			}
		case models.RailStripe:
			if err := validateStripeRail(name, proc); err != nil {
				return err
			}
		case models.RailSolana:
			if err := validateSolanaRail(name, proc); err != nil {
				return err
			}
		default:
			return fmt.Errorf("rail '%s' has unknown type '%s'", name, effectiveType)
		}
		if err := validateCustody(name, proc); err != nil {
			return err
		}
	}
	return nil
}

// validateNMIRail validates an NMI-type rail
func validateNMIRail(name string, proc *ResolvedPSP) error {
	nmi := proc.NMI
	if nmi == nil {
		return fmt.Errorf("rail '%s' (nmi): nmi block is required", name)
	}

	if strings.TrimSpace(nmi.SecurityKey) == "" {
		return fmt.Errorf("rail '%s' (nmi): security_key is required", name)
	}

	if strings.TrimSpace(nmi.WebhookSigningSecret) == "" {
		return fmt.Errorf("rail '%s' (nmi): webhook_signing_secret is required (signature verification cannot be disabled)", name)
	}

	return nil
}

// validateCCBillRail validates a CCBill-type rail
func validateCCBillRail(name string, proc *ResolvedPSP) error {
	// #697/#711: identity checks run even in dev — the clientAccnum/clientSubacc
	// pair is DERIVED from the dash-joined account_id, so a missing or malformed
	// account_id is a config bug, not a missing credential.
	if err := ValidateRailAccountID(models.RailCCBill, proc.EffectiveAccountID()); err != nil {
		return fmt.Errorf("rail '%s' (ccbill): %w", name, err)
	}
	if _, _, err := SplitCCBillAccountID(proc.EffectiveAccountID()); err != nil {
		return fmt.Errorf("rail '%s' (ccbill): account_id is required (the pair is derived from it): %w", name, err)
	}
	ccbill := proc.CCBill
	if ccbill == nil {
		return fmt.Errorf("rail '%s' (ccbill): ccbill block is required", name)
	}
	if strings.TrimSpace(ccbill.Salt) == "" {
		return fmt.Errorf("rail '%s' (ccbill): salt is required to sign FlexForm links", name)
	}

	hasUsername := strings.TrimSpace(ccbill.DataLinkUsername) != ""
	hasPassword := strings.TrimSpace(ccbill.DataLinkPassword) != ""
	if hasUsername != hasPassword {
		return fmt.Errorf("rail '%s' (ccbill): both datalink_username and datalink_password must be provided when configuring DataLink", name)
	}

	return nil
}

// validateStripeRail validates a Stripe-type rail
func validateStripeRail(name string, proc *ResolvedPSP) error {
	stripe := proc.Stripe
	if stripe == nil {
		return fmt.Errorf("rail '%s' (stripe): stripe block is required", name)
	}
	if strings.TrimSpace(stripe.SecretKey) == "" {
		log.Warnf("rail '%s' (stripe): secret_key not configured; checkout unavailable", name)
	}

	if strings.TrimSpace(stripe.WebhookSigningSecret) == "" {
		return fmt.Errorf("rail '%s' (stripe): webhook_signing_secret is required", name)
	}

	return nil
}

// validateSolanaRail validates only config-loading concerns. Solana token
// pricing/default policy belongs to internal/modules/solana/tokens and is
// applied at runtime by configureSolanaRail.
func validateSolanaRail(name string, proc *ResolvedPSP) error {
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

// validateCustody validates a PSP's third-party custody arrangement
// (or#879/or#880). Custody is a MODIFIER on a rail, so it is checked for every
// rail: only rails the custodian's registry entry names as proxy rails may
// reference it, and a referenced custodian must be fully armed or the checkout
// it backs is silently dead.
func validateCustody(name string, proc *ResolvedPSP) error {
	if !proc.HasThirdPartyCustody() {
		return nil
	}
	d, err := custodians.Require(proc.Custody.Custodian)
	if err != nil {
		return fmt.Errorf("rail '%s': %w", name, err)
	}
	rail := proc.EffectiveRail(name)
	if !d.SupportsRail(rail) {
		return fmt.Errorf("rail '%s' (%s): custodian %q is not supported on this rail — it can only be charged through %s, which has a detokenizing proxy path", name, rail, d.Kind, d.RailNames())
	}
	if strings.TrimSpace(proc.Custody.AccountID) == "" {
		return fmt.Errorf("rail '%s': custodian %q requires account_id (the custodian-native tenant id)", name, d.Kind)
	}
	for _, slot := range d.Secrets {
		if slot.Required && slot.Name == custodians.SecretAPIKey && strings.TrimSpace(proc.Custody.APIKey) == "" {
			return fmt.Errorf("rail '%s': custodian %q requires the %s secret (its private application key)", name, d.Kind, slot.Name)
		}
	}
	return nil
}

// RailKeysByType returns configured account names on the given rail,
// sorted for deterministic diagnostics and selection.
func (set PSPSet) RailKeysByType(rail models.Rail) []string {
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

// ActiveRailByType returns a deterministic non-archived PSP for a
// rail. Database-backed new-work selection uses created_at to pick the newest
// active account; config-only callers do not have that timestamp, so they use
// sorted config keys.
func (set PSPSet) ActiveRailByType(rail models.Rail) (string, *ResolvedPSP, error) {
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
func (set PSPSet) ActiveRailKeysByType(rail models.Rail) []string {
	var out []string
	for _, key := range set.RailKeysByType(rail) {
		if !set[key].Archived {
			out = append(out, key)
		}
	}
	return out
}

// FindByAccountID returns the configured account on a rail whose EffectiveAccountID
// matches accountID (#641), used to target a specific PSP.
func (set PSPSet) FindByAccountID(rail models.Rail, accountID string) (*ResolvedPSP, bool) {
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

// GetStripeRail returns the configured active Stripe rail.
func (set PSPSet) GetStripeRail() *ResolvedPSP {
	_, proc, _ := set.ActiveRailByType(models.RailStripe)
	return proc
}

// GetSolanaRail returns the configured active Solana rail.
func (set PSPSet) GetSolanaRail() *ResolvedPSP {
	_, proc, _ := set.ActiveRailByType(models.RailSolana)
	return proc
}

func (cfg *Config) normalizedProviderWriteMode() string {
	if cfg == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(cfg.ProviderWriteMode))
}

// GetProviderWriteMode returns the normalized provider write mode. Unset or
// invalid values normalize to READONLY — fail closed (Paul 2026-07-02): a boot
// that never declared its write policy (or typoed it before Validate runs)
// must not execute provider writes. Explicit full|limited is the only way to
// enable them.
func GetProviderWriteMode(cfg *Config) string {
	mode := cfg.normalizedProviderWriteMode()
	if mode == "" || !ValidProviderWriteModes[mode] {
		return ProviderWriteModeReadOnly
	}
	return mode
}

// IsTestMode returns true if payment rails should use sandbox/test
// environments and the sandbox-credential guarantees apply (#355): Stripe
// live-key refusal, NMI boot probe, CCBill sandbox URL, Solana devnet.
// Orthogonal to ProviderWriteMode (pure behavior). Unset (empty) reads as
// live, matching the historical bool zero value; standalone Load() always
// resolves TestMode explicitly before this is read, and embedded.New refuses
// to construct with it unset (#745), so "unset" in practice only reaches this
// accessor via a direct Config{} literal in a test. Sandbox is allowed in
// every environment (#762) — Validate no longer rejects test_mode=sandbox
// outside development.
func IsTestMode(cfg *Config) bool {
	return cfg.TestMode == CredentialPostureSandbox
}

// IsLimitedMode returns true if proactive payment-provider operations
// (dunning charges/cancellations, invoice collection, Solana
// pulls, catalog provider-object writes) are disabled, leaving only reactive,
// user-initiated operations. True in limited and readonly modes.
func IsLimitedMode(cfg *Config) bool {
	mode := GetProviderWriteMode(cfg)
	return mode == ProviderWriteModeLimited || mode == ProviderWriteModeReadOnly
}

// IsProviderReadOnly returns true if EVERY provider write — even reactive,
// user-initiated ones — must be blocked (provider_write_mode=readonly). Reads
// (query APIs, verification) stay allowed.
func IsProviderReadOnly(cfg *Config) bool {
	return GetProviderWriteMode(cfg) == ProviderWriteModeReadOnly
}

// Mode is a Config's provider write mode behind the interface the intent
// executor's gate reads (intents.ModeView). A nil Config is readonly.
type Mode struct{ Config *Config }

// IsProviderReadOnly reports IsProviderReadOnly of the Config.
func (m Mode) IsProviderReadOnly() bool { return IsProviderReadOnly(m.Config) }

// IsLimitedMode reports IsLimitedMode of the Config.
func (m Mode) IsLimitedMode() bool { return IsLimitedMode(m.Config) }

// validatePublicURL permits HTTP only for explicitly authorized loopback hosts.
func validatePublicURL(raw string, allowLoopback, originOnly bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be an absolute HTTPS URL without credentials, query or fragment")
	}
	if originOnly && u.Path != "" && u.Path != "/" {
		return fmt.Errorf("must be an origin without a path")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && allowLoopback && loopback) {
		return fmt.Errorf("must use HTTPS (HTTP requires an explicit loopback exception)")
	}
	return nil
}

// ValidateDatabase validates the database configuration and schema.
func ValidateDatabase(cfg *Config) error {
	if cfg == nil || cfg.DB == nil {
		return fmt.Errorf("database configuration is required")
	}
	if cfg.DB.URL == "" {
		return fmt.Errorf("database URL could not be determined")
	}
	// The schema is interpolated into SQL and must be a safe identifier (#165).
	return validateSchema(cfg.Schema)
}

// DefaultRateLimits are the built-in per-bucket limits, applied whenever
// Config.RateLimits is nil and limits are not disabled.
func DefaultRateLimits() *RateLimitsConfig {
	return &RateLimitsConfig{
		// Mutation endpoint; the per-user limit is the real fraud control.
		"subscribe": &RateLimit{RequestsPerMinute: 20},
		// Kept tight to deter card testing.
		"checkout": &RateLimit{RequestsPerMinute: 10},
		// Per source IP. All webhooks from a rail share one bucket (fixed rail
		// IPs), so this must absorb rebill runs without refusing legitimate
		// payment events. Webhooks are already authenticated; this is a DoS
		// floor, not the primary control.
		"webhook":            &RateLimit{RequestsPerMinute: 1200},
		"payment":            &RateLimit{RequestsPerMinute: 40},
		"metrics-ask":        &RateLimit{RequestsPerMinute: 10},
		"catalog-ask":        &RateLimit{RequestsPerMinute: 10},
		"dashboard-generate": &RateLimit{RequestsPerMinute: 10},
	}
}

// DefaultCaptcha is the captcha configuration applied with DefaultRateLimits:
// no account, so no challenge.
func DefaultCaptcha() *CaptchaConfig { return &CaptchaConfig{Provider: CaptchaProviderTurnstile} }

// LogStartupStatus writes the operator-facing posture banners for long-running
// OpenRails processes. Keep this out of Load so one-off CLIs do not look like
// they started the payment server.
func LogStartupStatus(cfg *Config) {
	logTestModeStatus(cfg)
	logOperatingModeStatus(cfg)
}

func logOperatingModeStatus(cfg *Config) {
	switch GetProviderWriteMode(cfg) {
	case ProviderWriteModeReadOnly:
		log.Warn("⚠️  PROVIDER_WRITE_MODE=readonly - ZERO payment-provider writes; even user-initiated charges fail loudly")
		log.Info("   Reconciliation/forensics posture: provider reads + local serving only")
		log.Info("   Set provider_write_mode=limited or provider_write_mode=full to allow writes")
	case ProviderWriteModeLimited:
		log.Warn("⚠️  PROVIDER_WRITE_MODE=limited - No proactive payment-provider operations will be performed")
		log.Info("   Dunning charges/cancellations, invoice collection, Solana pulls and catalog provider writes are paused")
		log.Info("   Reactive operations (checkout, vault saves, user/admin cancels, webhooks) work normally")
		log.Info("   Set provider_write_mode=full to resume proactive operations")
	case ProviderWriteModeFull:
		log.Info("Provider write mode: full (complete behavior - charges, dunning, deletes all run)")
	}
}

// logTestModeStatus logs the credential environment at startup.
// This helps operators confirm whether they're on sandbox or live credentials.
func logTestModeStatus(cfg *Config) {
	if IsTestMode(cfg) {
		log.Warn("⚠️  TEST ENV ENABLED - No real charges will be processed")
		log.Info("   Payment providers will use sandbox/test environments:")
		log.Info("   - NMI: secure.networkmerchants.com with test-mode transactions")
		log.Info("   - CCBill: sandbox-api.ccbill.com")
		log.Info("   - Stripe: requires sk_test_* key")
		log.Info("   - Solana: devnet")
	} else {
		log.Warn("🔴 LIVE CREDENTIALS - Real charges enabled")
		log.Info("   Payment providers will use production environments")

	}
}
