package openrails

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
)

// Config is the engine's configuration: plain data. Everything that reaches
// outside the process is in Deps. ProviderWriteMode is required. Whether money
// is real is each PSP's declared environment, never the deployment's.
type Config struct {
	// ProviderWriteMode is how much OpenRails may do against payment
	// providers: ProviderWritesFull (normal operation), ProviderWritesLimited
	// (no system-initiated provider writes) or ProviderWritesReadOnly (no
	// provider writes: it never charges anyone). Required.
	ProviderWriteMode ProviderWriteMode
	// EngineAdmissionHold pauses new renewal obligations, never receipt recovery.
	EngineAdmissionHold bool
	// UsageBillingEnabled opts this host into automatic usage invoices and collection.
	UsageBillingEnabled bool

	// Database names the Postgres schemas New creates or upgrades.
	Database DatabaseConfig
	// ClickHouse names the database of the event store, Deps.ClickHouse.
	ClickHouse ClickHouseConfig
	// Merchants declares the merchants an embedded engine provisions on
	// start; none leaves that to run time (a standalone server's are
	// provisioned through its API). A slug declared twice provisions neither
	// entry. Without a Vault KV mount a declaration is the merchant's
	// configuration, read-only; with one it seeds Vault once, when Vault
	// holds nothing for the merchant. A request acts on the merchant its
	// credential is bound to: see billing.MerchantDeclaration.Issuer.
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
	// ReadCache overrides how long the engine's handles serve catalog and
	// configuration reads from memory; nil keeps the defaults.
	ReadCache ReadCache

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

// ProviderWriteMode is how much OpenRails may do against payment providers.
type ProviderWriteMode string

const (
	// ProviderWritesFull is normal operation.
	ProviderWritesFull ProviderWriteMode = "full"
	// ProviderWritesLimited makes no system-initiated provider writes:
	// dunning, invoice collection and the other proactive operations wait,
	// while purchase and customer or staff actions run.
	ProviderWritesLimited ProviderWriteMode = "limited"
	// ProviderWritesReadOnly makes no provider writes: it never charges anyone.
	ProviderWritesReadOnly ProviderWriteMode = "readonly"
)

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
	// or was applied with other content, is billing.ErrMigrationsRequired.
	// Call Migrate first. Normal request-time and scheduled partition
	// maintenance is unchanged.
	SkipMigrations bool
	// LeaseConns caps the separate pool, copied from Deps.Postgres's
	// configuration, that renews idempotency leases. Zero is four
	// connections.
	LeaseConns int
}

// ClickHouseConfig names the ClickHouse database the event store keeps its
// tables in. New creates or upgrades it; with Database.SkipMigrations it only
// checks it, and MigrateClickHouse prepares it.
type ClickHouseConfig struct {
	// Database holds the event store's tables. Empty is "openrails".
	Database string
}

// RateLimitsConfig maps a feature bucket (purchase, payment, subscribe,
// webhook, catalog-ask) to its request limit. A bucket left out is not
// limited; other routes never are (a per-address ceiling belongs to the proxy
// in front of OpenRails).
type RateLimitsConfig map[string]*RateLimit

// RateLimit is one bucket's limit over a fixed one-minute window.
type RateLimit struct {
	RequestsPerMinute int
}

// CaptchaConfig is the captcha account. Setting both keys enables the
// challenge, which is asked only of a client that keeps hitting a rate limit
// on purchase, payment methods or subscriptions.
type CaptchaConfig struct {
	// Provider is "turnstile" (the default), "recaptcha-v3" or "hcaptcha".
	Provider  string
	SiteKey   string
	SecretKey string
}

// UserActionRateLimitConfig bounds distinct customers affected by manual card
// removals and cancellations in rolling hour/day windows. Zero fields select
// defaults.
type UserActionRateLimitConfig struct {
	MinimumCustomersPerHour        int64
	PaidCustomersPerHourlyCustomer int64
	MinimumCustomersPerDay         int64
	PaidCustomersPerDailyCustomer  int64
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

// CardVaultConfig is the operator's card vault for this deployment's
// posture: merchants with settings.card_vault platform store new cards in it,
// each in its own partition. Its keys are in Vault, at
// <kv_mount>/<scope_prefix>/platform_vault/<tenant_id>.
type CardVaultConfig struct {
	// Kind is the vault vendor: basis_theory.
	Kind billing.CardVaultKind
	// TenantID is the vendor's tenant id.
	TenantID string
	// KVMount holds the platform credential document. Empty uses
	// Config.Vault's KVMount. Setting this does not select Vault as the
	// merchant configuration source.
	KVMount string
}

// HyperSwitchConfig identifies the one trusted custody deployment and its
// vendor-owned browser assets. Merchant settings cannot change either target.
// The SDK is a separate server in the vendor product, hence its explicit URL.
type HyperSwitchConfig struct {
	// AllowLoopbackHTTP admits an http loopback fixture; refused beside a live PSP.
	AllowLoopbackHTTP bool
	APIBaseURL        string
	SDKURL            string
}

// SolanaConfig is the deployment's Solana configuration.
type SolanaConfig struct {
	// Crankers sign Solana rebilling, in order: the first publishes new plans,
	// and each pulls the renewals of the plans it owns, whose owner is fixed
	// on-chain. A plan no listed cranker owns cannot be pulled. Empty turns
	// Solana rebilling off; one-time payments still work.
	Crankers []SolanaCrankerConfig
}

// SolanaCrankerConfig is one cranker: a key that publishes and pulls plans and
// pays their fees, on whichever network each Solana PSP names (it needs SOL
// there). It never receives money: a plan pays only its merchant's treasury.
type SolanaCrankerConfig struct {
	// Mode is vault_transit (the key never leaves Vault Transit) or
	// local_keypair.
	Mode string
	// Key names the Vault Transit key (vault_transit).
	Key string
	// PrivateKey is the base58 keypair (local_keypair). A secret.
	PrivateKey string
}

// LLMConfig configures the model behind the console's catalog questions.
type LLMConfig struct {
	// Provider selects the API dialect: "anthropic" (the default) or
	// "openai". Any other value is refused.
	Provider string
	// Model is the provider's model ID; empty is the provider's default.
	Model string
	// BaseURL replaces the provider's public endpoint: an absolute https URL.
	// With openai it includes the version segment (https://host/v1); with
	// anthropic it is the origin.
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

// ParseMerchantDocument reads a merchant's document from YAML: a required
// slug and the configuration beside it, refusing unknown fields. A
// MerchantDeclaration's Issuer and Binding are set in code.
func ParseMerchantDocument(raw []byte) (billing.MerchantDocument, error) {
	return config.ParseMerchantDocument(raw)
}

// engineConfig is cfg as the engine reads it. Each settings type converts as
// a whole, so a field one side lacks fails to compile; ReadCache is the
// handles', not the engine's.
func engineConfig(cfg Config) config.Config {
	out := config.Config{
		ProviderWriteMode:       string(cfg.ProviderWriteMode),
		EngineAdmissionHold:     cfg.EngineAdmissionHold,
		UsageBillingEnabled:     cfg.UsageBillingEnabled,
		Database:                config.DatabaseConfig(cfg.Database),
		ClickHouse:              config.ClickHouseConfig(cfg.ClickHouse),
		Merchants:               cfg.Merchants,
		PublicBillingBaseURL:    cfg.PublicBillingBaseURL,
		ReturnOrigins:           cfg.ReturnOrigins,
		TrustedProxies:          cfg.TrustedProxies,
		CheckoutOrigin:          cfg.CheckoutOrigin,
		Captcha:                 convert(cfg.Captcha, func(c CaptchaConfig) config.CaptchaConfig { return config.CaptchaConfig(c) }),
		UserActionRateLimit:     config.UserActionRateLimitConfig(cfg.UserActionRateLimit),
		DestructiveActionLimits: config.DestructiveActionLimitsConfig(cfg.DestructiveActionLimits),
		Vault:                   convert(cfg.Vault, func(c VaultConfig) config.VaultConfig { return config.VaultConfig(c) }),
		CardVault:               convert(cfg.CardVault, func(c CardVaultConfig) config.CardVaultConfig { return config.CardVaultConfig(c) }),
		HyperSwitch:             convert(cfg.HyperSwitch, func(c HyperSwitchConfig) config.HyperSwitchConfig { return config.HyperSwitchConfig(c) }),
		LLM:                     convert(cfg.LLM, func(c LLMConfig) config.LLMConfig { return config.LLMConfig(c) }),
		ProviderSandbox:         convert(cfg.ProviderSandbox, func(c ProviderSandboxConfig) config.ProviderSandboxConfig { return config.ProviderSandboxConfig(c) }),
		EventRetention:          cfg.EventRetention,
		WebhookSecretOverlap:    cfg.WebhookSecretOverlap,
	}
	if cfg.RateLimits != nil {
		out.RateLimits = make(config.RateLimitsConfig, len(cfg.RateLimits))
		for bucket, limit := range cfg.RateLimits {
			out.RateLimits[bucket] = convert(limit, func(l RateLimit) config.RateLimit { return config.RateLimit(l) })
		}
	}
	out.Solana = convert(cfg.Solana, func(s SolanaConfig) config.SolanaConfig {
		crankers := make([]config.SolanaCrankerConfig, len(s.Crankers))
		for i, c := range s.Crankers {
			crankers[i] = config.SolanaCrankerConfig(c)
		}
		return config.SolanaConfig{Crankers: crankers}
	})
	return out
}

// convert is f of *p, or nil for nil.
func convert[U, T any](p *T, f func(T) U) *U {
	if p == nil {
		return nil
	}
	u := f(*p)
	return &u
}
