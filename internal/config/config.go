package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// configContextKey is a distinct type so this key cannot collide with another
// package storing "config" in the same context.
type configContextKey string

// ConfigContextKey is the context key the CLI stores the loaded *Config under.
const ConfigContextKey configContextKey = "config"

// Config is the engine's configuration: plain data. Everything that reaches
// outside the process is in Deps. ProviderWriteMode is required. Whether money
// is real is each PSP's declared environment, never the deployment's.
type Config struct {
	// ProviderWriteMode is how much OpenRails may do against payment
	// providers: "full" (normal operation), "limited" (no system-initiated
	// provider writes) or "readonly" (no provider writes: it never charges
	// anyone). Required.
	ProviderWriteMode string
	// EngineAdmissionHold pauses new renewal obligations, never receipt recovery.
	EngineAdmissionHold bool
	// UsageBillingEnabled opts this host into automatic usage invoices and collection.
	UsageBillingEnabled bool

	// Database names the Postgres schemas New creates or upgrades.
	Database DatabaseConfig
	// Merchants declares the merchants an embedded engine provisions on
	// start; none leaves that to run time (a standalone server's are
	// provisioned through its API). A slug declared twice provisions neither
	// entry. Without a Vault KV mount a declaration is the merchant's
	// configuration, read-only; with one it seeds Vault once, when Vault
	// holds nothing for the merchant. A request acts on the merchant its
	// credential is bound to: see MerchantDeclaration.Issuer.
	Merchants []billing.MerchantDeclaration

	// PublicBillingBaseURL is the external billing mount, excluding /v1. It is
	// used only to build provider callbacks and customer billing links.
	PublicBillingBaseURL string
	// ReturnOrigins are the exact origins (scheme://host[:port]) that purchase
	// and billing-portal return URLs may name. Empty allows only the origin of
	// PublicBillingBaseURL.
	ReturnOrigins []string
	// TrustedProxies are the CIDRs whose X-Forwarded-For is trusted when
	// resolving a client's address. Empty trusts none: the socket peer is the
	// client.
	TrustedProxies []string
	// CheckoutOrigin is the origin (https://pay.example.com) of the payment
	// page, which Engine.CheckoutPage serves from Deps.CheckoutAssets. It is
	// an origin of its own, apart from PublicBillingBaseURL and every
	// declared Issuer; one that is not is refused and reported. Empty serves
	// none.
	CheckoutOrigin string

	// RateLimits are the per-bucket request limits: nil takes the built-in
	// limits, an empty map limits nothing (a host that fronts billing with its
	// own limiter).
	RateLimits RateLimitsConfig
	// Captcha challenges a client that keeps hitting a rate limit.
	Captcha *CaptchaConfig
	// UserActionRateLimit bounds customer card deletes and soft cancellations
	// across this merchant's users, actors and processes. Always enabled,
	// including when the host owns ordinary HTTP rate limits.
	UserActionRateLimit UserActionRateLimitConfig
	// DestructiveActionLimits are the staff limits on refunds, revokes,
	// cancels and the other held destructive actions. Zero fields keep the
	// defaults.
	DestructiveActionLimits DestructiveActionLimitsConfig

	// Vault names the mounts of Deps.Vault: a KV mount makes Vault hold
	// merchant configuration.
	Vault *VaultConfig
	// CardVault is the operator's card vault; nil refuses
	// settings.card_vault platform.
	CardVault *CardVaultConfig
	// HyperSwitch is the host's trusted HyperSwitch deployment.
	HyperSwitch *HyperSwitchConfig
	// Solana is the deployment's Solana configuration: its crankers.
	Solana *SolanaConfig
	// LLM is the model behind the console's catalog questions.
	// Without an API key those features are off.
	LLM *LLMConfig

	// ProviderSandbox points non-live PSPs' provider clients at loopback
	// gateways, to qualify a process against fake providers, and accepts
	// extra CCBill webhook sources for sandbox accounts. A gateway is refused
	// beside a live PSP and for anything but a literal loopback address.
	ProviderSandbox *ProviderSandboxConfig

	// EventRetention is the retained event-log lifetime, as a Go duration.
	// Empty is 90 days. Reader progress never extends retention.
	EventRetention string
	// WebhookSecretOverlap is how long a rotated-out webhook signing secret
	// keeps verifying: a Go duration between 1m and 168h. Empty is 24h.
	WebhookSecretOverlap string
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
	// CCBillWebhookSources are extra source CIDRs accepted as a sandbox
	// CCBill account's webhook origins. CCBill signs nothing, so the source
	// address is the authentication: a live account accepts CCBill's own
	// ranges alone.
	CCBillWebhookSources []string
}

// ErrProviderSandboxGateway is the coded refusal for a provider_sandbox
// gateway declaration whose destination is not a literal loopback address.
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

// SandboxCCBillWebhookSources are the extra CCBill webhook source CIDRs
// declared for sandbox accounts.
func SandboxCCBillWebhookSources(cfg *Config) []string {
	if cfg == nil || cfg.ProviderSandbox == nil {
		return nil
	}
	return cfg.ProviderSandbox.CCBillWebhookSources
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

// MerchantConfigKVMount is the KV v2 mount that holds merchant
// configuration; "" when none is named and a file holds it.
func MerchantConfigKVMount(cfg *Config) string {
	if cfg == nil || cfg.Vault == nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(cfg.Vault.KVMount), "/")
}

// VaultConfig names the mounts OpenRails uses on Deps.Vault. A KV mount
// makes Vault hold merchant configuration; CardVault.KVMount can read card
// credentials separately.
type VaultConfig struct {
	// KVMount is the KV v2 mount merchant configuration lives in, under
	// ScopePrefix. Empty: Config.Merchants holds it.
	KVMount string
	// ScopePrefix is the path under KVMount; empty is "openrails".
	ScopePrefix string
	// TransitMount is the Transit mount Solana signing keys live under; empty
	// is "transit".
	TransitMount string
}

var mountPathRe = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)

// ValidateMountPath accepts one or more slash-led segments of RFC 3986
// unreserved characters, none of them "." or "..": the value lands in route
// patterns and the console's <base href>. name says which setting it is.
func ValidateMountPath(name, p string) error {
	bad := !mountPathRe.MatchString(p)
	for _, segment := range strings.Split(p, "/") {
		bad = bad || segment == "." || segment == ".."
	}
	if bad {
		return fmt.Errorf("invalid %s %q: want an absolute path like /billing — no trailing slash, no . or .. segments, only letters, digits and . _ ~ -", name, p)
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
// Catalog tool definitions and corrective validation keep requests constrained.
const (
	LLMDefaultModelAnthropic = "claude-haiku-4-5-20251001"
	// gpt-5.4-nano is the cheapest current-generation tool-capable OpenAI
	// model ($0.20/M in, $1.25/M out as of 2026-07). gpt-4.1-nano is nominally
	// cheaper but the 4.1 family is mid-retirement — a default must not 404.
	LLMDefaultModelOpenAI = "gpt-5.4-nano"
)

// LLMConfig configures the model behind the console's catalog
// questions.
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
	// CatalogCopilotEnabled turns on read-only questions about the catalog,
	// which send aggregate catalog and subscriber counts to the provider.
	CatalogCopilotEnabled bool
	// CatalogDraftingEnabled lets the catalog copilot draft price and catalog
	// changes. Drafts are proposals; nothing is applied.
	CatalogDraftingEnabled bool
}

// LLMConfigured reports whether a model backend is configured (fail-closed on a
// missing key).
func LLMConfigured(c *LLMConfig) bool { return c != nil && strings.TrimSpace(c.APIKey) != "" }

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

// DatabaseConfig names the Postgres schemas OpenRails keeps its tables in.
// The role of the pool New runs with owns every object in them.
type DatabaseConfig struct {
	// Schema holds OpenRails' tables. Empty is "billing".
	Schema string
	// RiverSchema holds the River tables OpenRails' jobs run in; empty is
	// Schema plus "_river" ("billing_river"), apart from AuthKit's and the
	// host's River. Start runs OpenRails' own River client there, and a host
	// fleet passed to Start with WithRiverClient must use the same schema.
	RiverSchema string
	// SkipMigrations makes New validate existing schemas without applying
	// migrations or preparing startup partitions: a migration that is missing,
	// or was applied with other content, is billing.ErrMigrationsRequired. Call
	// openrails.Migrate first.
	// Normal request-time and scheduled partition maintenance is unchanged.
	SkipMigrations bool
	// LeaseConns caps the separate pool, copied from Deps.Postgres's
	// configuration, that renews idempotency leases. Zero is four
	// connections.
	LeaseConns int
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

// RiverSchemaName is the schema of the River client Engine.Start runs:
// Database.RiverSchema, or the schema plus "_river".
func RiverSchemaName(c *Config) string {
	if c != nil {
		if s := strings.ToLower(strings.TrimSpace(c.Database.RiverSchema)); s != "" {
			return s
		}
	}
	return SchemaName(c) + "_river"
}

// schemaIdentRe restricts the OpenRails schema to a safe SQL identifier: it must
// start with a letter or underscore and contain only letters, digits, and
// underscores. This forbids quotes, spaces, and dots, so the value can be used to
// build search_path / River schema names without quoting hazards.
var schemaIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SchemaName returns the effective OpenRails Postgres schema (#165, #471):
// Database.Schema trimmed and lower-cased, default billing. Code that needs
// the schema reads it here, never Database.Schema directly.
func SchemaName(c *Config) string {
	if c == nil {
		return DefaultSchema
	}
	return normalizeSchema(c.Database.Schema)
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
	// Environment is the PSP's environment, one of its rail's.
	Environment string
	// Rail is the gateway this account is on: nmi, ccbill, stripe, solana.
	// Required unless the account name is itself a reserved gateway name.
	Rail models.Rail
	// AccountID is this account's rail-native identity (#641/#655): NMI
	// gateway-id, Stripe acct_…, CCBill clientAccnum-clientSubacc (dash-joined,
	// e.g. 999999-0000, #697), or a Solana PSP's treasury.
	// REQUIRED — ValidatePSPAccount rejects an empty one.
	AccountID string
	// Archived keeps an account addressable for existing obligations and inbound
	// provider events, but excludes it from new purchase/subscription work.
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

// ResolvedCustodian (#795 / or#879 / or#880) is the RESOLVED runtime shape of a
// declared custodian: who holds the card, under what tenant identity, and the
// credentials to detokenize it into a gateway. It is resolved from ONE
// custodians row and may be shared by every PSP that references it — the
// gateway credentials themselves stay on the PSP, one source of truth.
type ResolvedCustodian struct {
	Environment string
	// Container and CollectionApplicationID bind platform card collection to a merchant.
	Container               string
	CollectionApplicationID string
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
	// PublicAPIKey is payment-page config (not a secret).
	PublicAPIKey string
	// ProfileID is the merchant-owned HyperSwitch business profile.
	ProfileID string
	// NetworkTokens arms NT provisioning on instrument creation (never
	// load-bearing; charge routing stays pan_proxy on NMI gateways).
	NetworkTokens bool
	// AccountUpdater arms the real-time Account Updater, the priced add-on
	// that refreshes the card number charged through NMI.
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
// (#641). ValidatePSPAccount requires account_id — there is NO fallback to the
// config map name (an account is never indexed by a name we made up).
func (p *ResolvedPSP) EffectiveAccountID() string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.AccountID)
}

// ValidateRailAccountID refuses a malformed account: a CCBill part that is
// not CCBill's number, a treasury that is not a Solana address. An empty one
// is the callers'.
func ValidateRailAccountID(rail models.Rail, accountID string) error {
	switch rail {
	case models.RailCCBill:
		parts := rails.AccountParts(rail, accountID)
		for _, name := range rails.AccountFields(rail) {
			if part := parts[name]; part == "" || strings.Trim(part, "0123456789") != "" {
				return fmt.Errorf("%s must be CCBill's number, digits only (got %q)", name, part)
			}
		}
	case models.RailSolana:
		return ValidateSolanaTreasury(accountID)
	}
	return nil
}

// ValidatePSPAccount refuses a PSP missing an account field of its rail, or
// with a malformed account.
func ValidatePSPAccount(psp, rail, account string) error {
	r := models.Rail(strings.ToLower(strings.TrimSpace(rail)))
	account = strings.TrimSpace(account)
	parts := rails.AccountParts(r, account)
	for _, name := range rails.AccountFields(r) {
		if strings.TrimSpace(parts[name]) == "" {
			return fmt.Errorf("psps.%s.%s is required", psp, name)
		}
	}
	if err := ValidateRailAccountID(r, account); err != nil {
		return fmt.Errorf("psps.%s: %w", psp, err)
	}
	return nil
}

// IsNMI returns true if this rail config is for an NMI-backed rail.
func (p *ResolvedPSP) IsNMI(name string) bool {
	return p.EffectiveRail(name) == models.RailNMI
}

// ToNMIProviderSettings converts the rail config to NMI client settings.
// Only valid for NMI-type rails.
func (p *ResolvedPSP) ToNMIProviderSettings() *NMIProviderSettings {
	s := &NMIProviderSettings{Environment: p.Environment}
	if p.NMI != nil {
		s.SecurityKey = p.NMI.SecurityKey
		s.WebhookSecret = p.NMI.WebhookSigningSecret
	}
	return s
}

// SplitCCBillAccountID splits a CCBill account into its client_accnum and
// client_subacc.
func SplitCCBillAccountID(accountID string) (accNum, subAcc string, err error) {
	accountID = strings.TrimSpace(accountID)
	if err := ValidateRailAccountID(models.RailCCBill, accountID); err != nil {
		return "", "", err
	}
	parts := rails.AccountParts(models.RailCCBill, accountID)
	return parts["client_accnum"], parts["client_subacc"], nil
}

// ToCCBillConfig converts the rail config to the CCBill client config. Only
// valid for CCBill-type rails. A malformed account leaves the pair empty and
// is rejected by ValidatePSPAccount.
func (p *ResolvedPSP) ToCCBillConfig() *CCBillConfig {
	c := &CCBillConfig{Sandbox: p.Environment == EnvironmentSandbox}
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
// credential pair and the PSP's environment, which selects the gateway.
type NMIProviderSettings struct {
	Environment   string
	SecurityKey   string
	WebhookSecret string
}

// CCBillConfig is the derived CCBill client config: the PSP's
// client_accnum and client_subacc plus credentials.
type CCBillConfig struct {
	Salt         string
	ClientSubAcc string
	ClientAccNum string
	// Sandbox selects CCBill's sandbox (a sandbox PSP).
	Sandbox bool

	DataLinkUsername string
	DataLinkPassword string
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

// UserActionRateLimitConfig bounds distinct customers affected by manual card
// removals and cancellations in rolling hour/day windows. Zero fields select defaults.
type UserActionRateLimitConfig struct {
	MinimumCustomersPerHour        int64
	PaidCustomersPerHourlyCustomer int64
	MinimumCustomersPerDay         int64
	PaidCustomersPerDailyCustomer  int64
}

func NormalizeUserActionRateLimit(c UserActionRateLimitConfig) (UserActionRateLimitConfig, error) {
	if c.MinimumCustomersPerHour == 0 {
		c.MinimumCustomersPerHour = 5
	}
	if c.PaidCustomersPerHourlyCustomer == 0 {
		c.PaidCustomersPerHourlyCustomer = 400
	}
	if c.MinimumCustomersPerDay == 0 {
		c.MinimumCustomersPerDay = 10
	}
	if c.PaidCustomersPerDailyCustomer == 0 {
		c.PaidCustomersPerDailyCustomer = 100
	}
	if c.MinimumCustomersPerHour < 1 || c.PaidCustomersPerHourlyCustomer < 1 || c.MinimumCustomersPerDay < 1 || c.PaidCustomersPerDailyCustomer < 1 {
		return c, fmt.Errorf("user_action_rate_limit fields must be positive")
	}
	return c, nil
}

// DestructiveActionLimitsConfig is how many destructive actions staff may
// take in a rolling hour and day before a hold stops them: each actor's, each
// merchant's and the deployment's. The merchant and deployment limits are
// floors that rise with active customers. Zero fields select the defaults
// (actor 5/15, merchant 15/25, deployment 50/100).
type DestructiveActionLimitsConfig struct {
	ActorPerHour      int64
	ActorPerDay       int64
	MerchantPerHour   int64
	MerchantPerDay    int64
	DeploymentPerHour int64
	DeploymentPerDay  int64
}

// NormalizeDestructiveActionLimits fills the defaults and refuses limits that
// could never apply: a negative one, an hour above its day, an actor above
// its merchant, or a merchant above the deployment.
func NormalizeDestructiveActionLimits(c DestructiveActionLimitsConfig) (DestructiveActionLimitsConfig, error) {
	for _, f := range []struct {
		v   *int64
		def int64
	}{{&c.ActorPerHour, 5}, {&c.ActorPerDay, 15}, {&c.MerchantPerHour, 15}, {&c.MerchantPerDay, 25}, {&c.DeploymentPerHour, 50}, {&c.DeploymentPerDay, 100}} {
		if *f.v == 0 {
			*f.v = f.def
		}
		if *f.v < 0 {
			return c, fmt.Errorf("destructive_action_limits fields must be positive")
		}
	}
	switch {
	case c.ActorPerHour > c.ActorPerDay || c.MerchantPerHour > c.MerchantPerDay || c.DeploymentPerHour > c.DeploymentPerDay:
		return c, fmt.Errorf("destructive_action_limits: an hourly limit cannot exceed its daily limit")
	case c.ActorPerHour > c.MerchantPerHour || c.ActorPerDay > c.MerchantPerDay:
		return c, fmt.Errorf("destructive_action_limits: an actor limit cannot exceed the merchant limit")
	case c.MerchantPerHour > c.DeploymentPerHour || c.MerchantPerDay > c.DeploymentPerDay:
		return c, fmt.Errorf("destructive_action_limits: a merchant limit cannot exceed the deployment limit")
	}
	return c, nil
}

// RateLimitsConfig maps a feature bucket to its request limit.
// A bucket left out is not limited; other routes never are (a
// per-address ceiling belongs to the proxy in front of OpenRails).
type RateLimitsConfig map[string]*RateLimit

// RateLimitBuckets are the buckets RateLimitsConfig may name.
var RateLimitBuckets = []string{"purchase", "payment", "subscribe", "webhook", "catalog-ask"}

// Provider write modes (#346, #355) — see Config.ProviderWriteMode.
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
// on purchase, payment methods or subscriptions.
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
	return []string{"purchase", "payment-methods", "subscriptions"}
}

// Validate refuses what the engine cannot serve without: an unknown provider
// write mode (#346), impossible limits and the database schema. Every
// optional setting is Degrade's: it is turned off with a reason, never
// refused.
func Validate(cfg *Config) error {
	if _, err := NormalizeUserActionRateLimit(cfg.UserActionRateLimit); err != nil {
		return err
	}
	if _, err := NormalizeDestructiveActionLimits(cfg.DestructiveActionLimits); err != nil {
		return err
	}

	// A typo (e.g. "redaonly") must never silently boot with full behavior.
	providerWriteMode := cfg.normalizedProviderWriteMode()
	if !ValidProviderWriteModes[providerWriteMode] {
		return fmt.Errorf("invalid provider_write_mode %q: must be one of full, limited, readonly", providerWriteMode)
	}
	if err := ValidateDatabase(cfg.Database); err != nil {
		return fmt.Errorf("database config validation failed: %w", err)
	}
	return nil
}

// validateLLM refuses an unknown provider (#741/#761: one vendor's dialect
// pointed at another's key) and a model endpoint over plaintext HTTP.
func validateLLM(llm *LLMConfig) error {
	if llm == nil {
		return nil
	}
	switch LLMProvider(llm) {
	case LLMProviderAnthropic, LLMProviderOpenAI:
	default:
		return fmt.Errorf("invalid llm.provider %q: must be %q or %q", llm.Provider, LLMProviderAnthropic, LLMProviderOpenAI)
	}
	if base := strings.TrimSpace(llm.BaseURL); base != "" {
		u, err := url.Parse(base)
		if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("invalid llm.base_url %q: must be an absolute http(s) URL", base)
		}
		if u.Scheme != "https" {
			return fmt.Errorf("llm.base_url %q must use https", base)
		}
	}
	return nil
}

// validateSourceCIDR refuses a malformed CIDR (#746) and, deliberately, a /0
// prefix. The lists it guards feed transport-level authentication (SEC-19):
// trusting 0.0.0.0/0 as a proxy makes every X-Forwarded-For authoritative,
// and allowlisting ::/0 as a CCBill source accepts a forged callback from
// anywhere (CCBill has no HMAC). Degrade drops such an entry, so the list
// only ever trusts less.
func validateSourceCIDR(raw string) error {
	_, ipNet, err := net.ParseCIDR(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid CIDR %q (expected e.g. \"10.0.0.0/8\"): %w", raw, err)
	}
	if ones, _ := ipNet.Mask.Size(); ones == 0 {
		return fmt.Errorf("CIDR %q matches every address; enumerate the real ranges instead", raw)
	}
	return nil
}

// ValidateVault refuses a mount path that is not a plain Vault path: Vault
// is then unusable (vaultconn.Open), never a boot failure.
func ValidateVault(vc *VaultConfig) error {
	if vc == nil {
		return nil
	}
	for name, raw := range map[string]string{"vault.kv_mount": vc.KVMount, "vault.scope_prefix": vc.ScopePrefix, "vault.transit_mount": vc.TransitMount} {
		p := strings.Trim(strings.TrimSpace(raw), "/")
		if p != "" && (path.Clean(p) != p || strings.Contains(p, "..")) {
			return fmt.Errorf("%s %q must be a plain Vault path", name, raw)
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

// ValidateStripeSecretKeyEnvironment refuses a Stripe key that disagrees with
// its PSP's declared environment: a live key moves real money, a test key
// never does. Stripe's key prefix is the provider's own answer.
func ValidateStripeSecretKeyEnvironment(environment, secretKey string) error {
	secretKey = strings.TrimSpace(secretKey)
	isLiveKey := strings.HasPrefix(secretKey, "sk_live_") || strings.HasPrefix(secretKey, "rk_live_")
	isTestKey := strings.HasPrefix(secretKey, "sk_test_") || strings.HasPrefix(secretKey, "rk_test_")
	if secretKey == "" {
		return nil
	}
	if (!isLiveKey && !isTestKey) || len(secretKey) <= len("sk_test_") {
		return fmt.Errorf("invalid Stripe secret key format: expected sk_live_, rk_live_, sk_test_, or rk_test_ with a nonempty value")
	}
	if isLiveKey && !LiveEnvironment("stripe", environment) {
		return fmt.Errorf("a live key (sk_live_/rk_live_) moves real money; the PSP declares environment %q", environment)
	}
	if isTestKey && LiveEnvironment("stripe", environment) {
		return fmt.Errorf("a test key (sk_test_/rk_test_) moves no money; the PSP declares environment live")
	}
	return nil
}

// ValidateStripePublishableKeyEnvironment checks a declared browser key the
// same way: pk_live_ only on a live PSP, pk_test_ only on a test one.
func ValidateStripePublishableKeyEnvironment(environment, key string) error {
	key = strings.TrimSpace(key)
	live, test := strings.HasPrefix(key, "pk_live_"), strings.HasPrefix(key, "pk_test_")
	if (!live && !test) || len(key) <= len("pk_test_") {
		return fmt.Errorf("invalid Stripe publishable key format: expected pk_live_ or pk_test_ with a nonempty value")
	}
	if live != LiveEnvironment("stripe", environment) {
		return fmt.Errorf("publishable key %s… disagrees with the PSP's environment %q", key[:len("pk_test_")], environment)
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

// IsLimitedMode returns true if proactive payment-provider operations
// (dunning charges/cancellations, usage order collection, Solana
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

// ValidatePublicURL permits HTTP only for explicitly authorized loopback hosts.
func ValidatePublicURL(raw string, allowLoopback, originOnly bool) error {
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

// ValidateDatabase refuses a schema that is not a safe identifier and a
// negative lease pool.
func ValidateDatabase(d DatabaseConfig) error {
	if d.LeaseConns < 0 {
		return fmt.Errorf("database.lease_conns %d is negative", d.LeaseConns)
	}
	// The schema is interpolated into SQL and must be a safe identifier (#165).
	return validateSchema(d.Schema)
}

// DefaultRateLimits are the built-in per-bucket limits, applied whenever
// Config.RateLimits is nil.
func DefaultRateLimits() RateLimitsConfig {
	return RateLimitsConfig{
		// Mutation endpoint; the per-user limit is the real fraud control.
		"subscribe": &RateLimit{RequestsPerMinute: 20},
		// Kept tight to deter card testing.
		"purchase": &RateLimit{RequestsPerMinute: 10},
		// Per source IP. All webhooks from a rail share one bucket (fixed rail
		// IPs), so this must absorb rebill runs without refusing legitimate
		// payment events. Webhooks are already authenticated; this is a DoS
		// floor, not the primary control.
		"webhook":     &RateLimit{RequestsPerMinute: 1200},
		"payment":     &RateLimit{RequestsPerMinute: 40},
		"catalog-ask": &RateLimit{RequestsPerMinute: 10},
	}
}

// DefaultCaptcha is the captcha configuration applied when Config.Captcha is
// nil: no account, so no challenge.
func DefaultCaptcha() *CaptchaConfig { return &CaptchaConfig{Provider: CaptchaProviderTurnstile} }

// LogStartupStatus writes the operator-facing posture banners for long-running
// OpenRails processes. Keep this out of Load so one-off CLIs do not look like
// they started the payment server.
func LogStartupStatus(logger *slog.Logger, cfg *Config) {
	logOperatingModeStatus(logger, cfg)
}

func logOperatingModeStatus(logger *slog.Logger, cfg *Config) {
	switch GetProviderWriteMode(cfg) {
	case ProviderWriteModeReadOnly:
		logger.Warn("⚠️  PROVIDER_WRITE_MODE=readonly - ZERO payment-provider writes; even user-initiated charges fail loudly")
		logger.Info("   Reconciliation/forensics posture: provider reads + local serving only")
		logger.Info("   Set provider_write_mode=limited or provider_write_mode=full to allow writes")
	case ProviderWriteModeLimited:
		logger.Warn("⚠️  PROVIDER_WRITE_MODE=limited - No proactive payment-provider operations will be performed")
		logger.Info("   Dunning charges/cancellations, usage order collection, Solana pulls and catalog provider writes are paused")
		logger.Info("   Reactive operations (purchase, vault saves, user/admin cancels, webhooks) work normally")
		logger.Info("   Set provider_write_mode=full to resume proactive operations")
	case ProviderWriteModeFull:
		logger.Info("Provider write mode: full (complete behavior - charges, dunning, deletes all run)")
	}
}
