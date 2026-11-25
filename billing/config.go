package billing

import (
	"cmp"
	"reflect"
	"strings"
	"time"
)

// Config is the engine's settings: plain data, as openrails.New takes them
// and as the standalone server's file spells them (the yaml keys).
// Everything that reaches outside the process is in openrails.Deps. Whether
// money is real is each PSP's declared environment, never the deployment's;
// how much OpenRails may write to providers is each merchant's write posture.
// A field left zero takes DefaultConfig's value (WithDefaults).
type Config struct {
	// EngineAdmissionHold pauses new renewal obligations, never receipt recovery.
	EngineAdmissionHold bool `yaml:"engine_admission_hold"`
	// UsageBillingEnabled opts this host into automatic usage invoices and collection.
	UsageBillingEnabled bool `yaml:"usage_billing_enabled"`

	// Database names the Postgres schemas New creates or upgrades.
	Database DatabaseConfig `yaml:"database"`
	// ClickHouse names the database of the event store, Deps.ClickHouse.
	ClickHouse ClickHouseConfig `yaml:"clickhouse"`
	// Merchants declares the merchants an embedded engine provisions on
	// start; none leaves that to run time (a standalone server's are
	// provisioned through its API). A slug declared twice provisions neither
	// entry. Without a Vault KV mount a declaration is the merchant's
	// configuration, read-only; with one it seeds Vault once, when Vault
	// holds nothing for the merchant. A request acts on the merchant its
	// credential is bound to: see MerchantDeclaration.Issuer. Set in code.
	Merchants []MerchantDeclaration `yaml:"-"`

	// PublicBillingBaseURL is the external billing mount, excluding /v1. It is
	// used only to build provider callbacks and customer billing links.
	PublicBillingBaseURL string `yaml:"public_billing_base_url"`
	// ReturnOrigins are the exact origins (scheme://host[:port]) that purchase
	// and billing-portal return URLs may name. Empty allows only the origin of
	// PublicBillingBaseURL.
	ReturnOrigins []string `yaml:"return_origins"`
	// TrustedProxies are the CIDRs whose X-Forwarded-For is trusted when
	// resolving a client's address. Empty trusts none: the socket peer is the
	// client.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// CloudflareProxies are Cloudflare's ranges where Cloudflare fronts the
	// origin: trusted like TrustedProxies, and the only proxies whose
	// CF-IPCountry header a checkout records. A standalone server sets them
	// from its cloudflare_proxies.
	CloudflareProxies []string `yaml:"-"`
	// CheckoutOrigin is the origin (https://pay.example.com) of the payment
	// page, which Engine.CheckoutPage serves from Deps.CheckoutAssets. It is
	// an origin of its own, apart from PublicBillingBaseURL and every
	// declared Issuer; one that is not is refused and reported. Empty serves
	// none.
	CheckoutOrigin string `yaml:"checkout_origin"`

	// RateLimits are the per-bucket request limits: nil takes the defaults,
	// an empty map limits nothing (a host that fronts billing with its own
	// limiter).
	RateLimits RateLimitsConfig `yaml:"rate_limits"`
	// Captcha challenges a client that keeps hitting a rate limit; it is on
	// when both its keys are set.
	Captcha CaptchaAccount `yaml:"captcha"`
	// UserActionRateLimit bounds customer card deletes and soft cancellations
	// across this merchant's users, actors and processes. Always enabled,
	// including when the host owns ordinary HTTP rate limits.
	UserActionRateLimit UserActionRateLimitConfig `yaml:"user_action_rate_limit"`
	// DestructiveActionLimits are the staff limits on refunds, revokes,
	// cancels and the other held destructive actions.
	DestructiveActionLimits DestructiveActionLimitsConfig `yaml:"destructive_action_limits"`

	// Vault names the mounts of Deps.Vault, which turns Vault on: a KV mount
	// makes Vault hold merchant configuration.
	Vault VaultConfig `yaml:"vault"`
	// CardVault is the operator's card vault, on when any field is set;
	// unset refuses settings.card_vault platform.
	CardVault CardVaultConfig `yaml:"card_vault"`
	// HyperSwitch is the host's trusted HyperSwitch deployment, on when any
	// field is set.
	HyperSwitch HyperSwitchConfig `yaml:"hyperswitch"`
	// Solana is the deployment's Solana configuration: its crankers.
	Solana SolanaConfig `yaml:"solana"`
	// LLM is the model behind the console's catalog questions.
	// Without an API key those features are off.
	LLM LLMConfig `yaml:"llm"`
	// ReadCache overrides how long the engine's handles serve catalog and
	// configuration reads from memory. Set in code: the standalone server
	// reads through no handle.
	ReadCache ReadCache `yaml:"-"`

	// ProviderSandbox points non-live PSPs' provider clients at loopback
	// gateways, to qualify a process against fake providers, and accepts
	// extra CCBill webhook sources for sandbox accounts; unset, none. A
	// gateway is refused beside a live PSP and for anything but a literal
	// loopback address.
	ProviderSandbox ProviderSandboxConfig `yaml:"provider_sandbox"`

	// EventRetention is the retained event-log lifetime, at least 1s. Reader
	// progress never extends retention.
	EventRetention time.Duration `yaml:"event_retention"`
	// WebhookSecretOverlap is how long a rotated-out webhook signing secret
	// keeps verifying: between 1m and 168h.
	WebhookSecretOverlap time.Duration `yaml:"webhook_secret_overlap"`
}

// DatabaseConfig names the Postgres schemas OpenRails keeps its tables in.
// The role of the pool New runs with owns every object in them.
type DatabaseConfig struct {
	// Schema holds OpenRails' tables: a plain identifier.
	Schema string `yaml:"schema"`
	// RiverSchema holds the River tables OpenRails' jobs run in; empty is
	// Schema plus "_river", apart from AuthKit's and the host's River. Start
	// runs OpenRails' own River client there, and a host fleet passed to
	// Start with WithRiverClient must use the same schema.
	RiverSchema string `yaml:"river_schema"`
	// SkipMigrations makes New validate existing schemas without applying
	// migrations or preparing startup partitions: a migration that is missing,
	// or was applied with other content, is ErrMigrationsRequired. Call
	// openrails.Migrate first. Normal request-time and scheduled partition
	// maintenance is unchanged.
	SkipMigrations bool `yaml:"skip_migrations"`
	// LeaseConns caps the separate pool, copied from Deps.Postgres's
	// configuration, that renews leases: idempotency claims, the billing pass
	// and merchant pulls.
	LeaseConns int `yaml:"lease_conns"`
}

// ClickHouseConfig names the ClickHouse database the event store keeps its
// tables in. New creates or upgrades it; with Database.SkipMigrations it only
// checks it, and openrails.MigrateClickHouse prepares it.
type ClickHouseConfig struct {
	// Database holds the event store's tables.
	Database string `yaml:"database"`
}

// RateLimitsConfig maps a feature bucket (purchase, payment, subscribe,
// webhook, catalog-ask) to its request limit. A bucket left out is not
// limited; other routes never are (a per-address ceiling belongs to the proxy
// in front of OpenRails). Counting is per one-minute window, bucket and
// subject (the client address, plus the user when authenticated); any
// subject tripping refuses the request with 429. An unknown bucket is
// reported and ignored. In-process Client calls count nothing. The client
// address is the socket peer unless TrustedProxies lists the proxy.
type RateLimitsConfig map[string]*RateLimit

// RateLimit is one bucket's limit over a fixed one-minute window.
type RateLimit struct {
	RequestsPerMinute int `yaml:"requests_per_minute"`
}

// CaptchaAccount is the deployment's captcha account. Setting both keys
// enables the challenge, which is asked only of a client that keeps hitting a
// rate limit on purchase, payment methods or subscriptions. CaptchaConfig is
// what a browser is told about it.
type CaptchaAccount struct {
	// Provider is "turnstile", "recaptcha-v3" or "hcaptcha".
	Provider  string `yaml:"provider"`
	SiteKey   string `yaml:"site_key"`
	SecretKey string `yaml:"secret_key"`
}

// UserActionRateLimitConfig bounds distinct customers affected by manual card
// removals and cancellations in rolling hour/day windows. The budget is
// merchant-wide across replicas and always enforced: per rolling day the
// larger of MinimumCustomersPerDay or one per PaidCustomersPerDailyCustomer
// customers who paid in the last 30 days, per rolling hour likewise. A
// customer counts once a day. A refusal answers 429, records a finding and
// opens no hold. Renewals, expiry and provider-side cancellation spend
// nothing.
type UserActionRateLimitConfig struct {
	MinimumCustomersPerHour        int64 `yaml:"minimum_customers_per_hour"`
	PaidCustomersPerHourlyCustomer int64 `yaml:"paid_customers_per_hourly_customer"`
	MinimumCustomersPerDay         int64 `yaml:"minimum_customers_per_day"`
	PaidCustomersPerDailyCustomer  int64 `yaml:"paid_customers_per_daily_customer"`
}

// DestructiveActionLimitsConfig is how many destructive actions staff may
// take in a rolling hour and day before a hold stops them: each actor's, each
// merchant's and the deployment's. The merchant and deployment limits are
// floors that rise with active customers. Contact erasures keep their own
// budget per merchant, whose floors rise with its directory; past it they
// wait rather than fail.
//
// Counted actions are refunds, cancellations, payment-method removal, access
// and credit revocation, write-offs, and changes that archive or remove an
// existing catalog object's billable, access or routing terms; creating an
// object spends nothing, nor do replays and proven no-ops. A batch counts its
// targets. The actor is the verified issuer and invoker. An in-process call
// that names no invoker is the host's own work: audited, it spends nothing
// and no hold stops it. A breach answers 429 and opens a hold that persists
// until a person clears it (openrails destructive-holds).
type DestructiveActionLimitsConfig struct {
	ActorPerHour      int64 `yaml:"actor_per_hour"`
	ActorPerDay       int64 `yaml:"actor_per_day"`
	MerchantPerHour   int64 `yaml:"merchant_per_hour"`
	MerchantPerDay    int64 `yaml:"merchant_per_day"`
	DeploymentPerHour int64 `yaml:"deployment_per_hour"`
	DeploymentPerDay  int64 `yaml:"deployment_per_day"`
	ErasurePerHour    int64 `yaml:"erasure_per_hour"`
	ErasurePerDay     int64 `yaml:"erasure_per_day"`
}

// VaultConfig names the mounts OpenRails uses on Deps.Vault. A KV mount
// makes Vault hold merchant configuration; CardVault.KVMount can read card
// credentials separately.
type VaultConfig struct {
	// KVMount is the KV v2 mount merchant configuration lives in, under
	// ScopePrefix. Empty: Config.Merchants holds it.
	KVMount string `yaml:"kv_mount"`
	// ScopePrefix is the path under KVMount.
	ScopePrefix string `yaml:"scope_prefix"`
	// TransitMount is the Transit mount Solana signing keys live under.
	TransitMount string `yaml:"transit_mount"`
}

// CardVaultConfig is the operator's card vault for this deployment's
// posture: merchants with settings.card_vault platform store new cards in it,
// each in its own partition. Its keys are in Vault, at
// <kv_mount>/<scope_prefix>/platform_vault/<tenant_id>.
type CardVaultConfig struct {
	// Kind is the vault vendor: basis_theory.
	Kind CardVaultKind `yaml:"kind"`
	// TenantID is the vendor's tenant id.
	TenantID string `yaml:"tenant_id"`
	// KVMount holds the platform credential document. Empty uses
	// Config.Vault's KVMount. Setting this does not select Vault as the
	// merchant configuration source.
	KVMount string `yaml:"kv_mount"`
}

// HyperSwitchConfig identifies the one trusted custody deployment and its
// vendor-owned browser assets. Merchant settings cannot change either target.
// The SDK is a separate server in the vendor product, hence its explicit URL.
type HyperSwitchConfig struct {
	// AllowLoopbackHTTP admits an http loopback fixture; refused beside a live PSP.
	AllowLoopbackHTTP bool   `yaml:"allow_loopback_http"`
	APIBaseURL        string `yaml:"api_base_url"`
	SDKURL            string `yaml:"sdk_url"`
}

// SolanaConfig is the deployment's Solana configuration.
type SolanaConfig struct {
	// Crankers sign Solana rebilling, in order: the first publishes new plans,
	// and each pulls the renewals of the plans it owns, whose owner is fixed
	// on-chain. A plan no listed cranker owns cannot be pulled. Empty turns
	// Solana rebilling off; one-time payments still work.
	Crankers []SolanaCrankerConfig `yaml:"crankers"`
}

// SolanaCrankerConfig is one cranker: a key that publishes and pulls plans and
// pays their fees, on whichever network each Solana PSP names (it needs SOL
// there). It never receives money: a plan pays only its merchant's treasury.
type SolanaCrankerConfig struct {
	// Mode is vault_transit (the key never leaves Vault Transit) or
	// local_keypair.
	Mode string `yaml:"mode"`
	// Key names the Vault Transit key (vault_transit).
	Key string `yaml:"key"`
	// PrivateKey is the base58 keypair (local_keypair). A secret.
	PrivateKey string `yaml:"private_key"`
}

// LLMConfig configures the model behind the console's catalog questions.
type LLMConfig struct {
	// Provider selects the API dialect: "anthropic" (when empty) or
	// "openai". Any other value is refused.
	Provider string `yaml:"provider"`
	// Model is the provider's model ID; empty is the provider's default.
	Model string `yaml:"model"`
	// BaseURL replaces the provider's public endpoint: an absolute https URL.
	// With openai it includes the version segment (https://host/v1); with
	// anthropic it is the origin.
	BaseURL string `yaml:"base_url"`
	// APIKey is the provider credential. Empty turns the features off.
	APIKey string `yaml:"api_key"`
	// CatalogCopilotEnabled turns on read-only questions about the catalog,
	// which send aggregate catalog and subscriber counts to the provider.
	CatalogCopilotEnabled bool `yaml:"catalog_copilot_enabled"`
	// CatalogDraftingEnabled lets the catalog copilot draft price and catalog
	// changes. Drafts are proposals; nothing is applied.
	CatalogDraftingEnabled bool `yaml:"catalog_drafting_enabled"`
}

// ProviderSandboxConfig names loopback provider gateways for sandbox runs.
// Each is an absolute http(s) URL whose host is a loopback IP literal, with no
// userinfo; hostnames, even "localhost", are refused.
type ProviderSandboxConfig struct {
	// NMIGatewayURL replaces the NMI sandbox endpoints.
	NMIGatewayURL string `yaml:"nmi_gateway_url"`
	// StripeAPIURL replaces the Stripe API root. The readonly guard and the
	// pinned API version still apply.
	StripeAPIURL string `yaml:"stripe_api_url"`
	// SolanaRPCURL replaces every merchant's Solana RPC endpoint.
	SolanaRPCURL string `yaml:"solana_rpc_url"`
	// CCBillDataLinkURL replaces the CCBill DataLink endpoint.
	CCBillDataLinkURL string `yaml:"ccbill_datalink_url"`
	// CCBillWebhookSources are extra source CIDRs accepted as a sandbox
	// CCBill account's webhook origins. CCBill signs nothing, so the source
	// address is the authentication: a live account accepts CCBill's own
	// ranges alone.
	CCBillWebhookSources []string `yaml:"ccbill_webhook_sources"`
}

// DefaultConfig is every engine setting's default: the value a setting left
// zero takes. A default inside an optional section does not turn it on.
func DefaultConfig() Config {
	return Config{
		Database:   DatabaseConfig{Schema: "billing", LeaseConns: 4},
		ClickHouse: ClickHouseConfig{Database: "openrails"},
		RateLimits: RateLimitsConfig{
			// Kept tight to deter card testing.
			"purchase": {RequestsPerMinute: 10},
			// Mutation endpoint; the per-user limit is the real fraud control.
			"subscribe": {RequestsPerMinute: 20},
			"payment":   {RequestsPerMinute: 40},
			// All webhooks from a rail share its source addresses: it must
			// absorb rebill runs. A DoS floor; webhooks are authenticated.
			"webhook":     {RequestsPerMinute: 1200},
			"catalog-ask": {RequestsPerMinute: 10},
		},
		UserActionRateLimit: UserActionRateLimitConfig{
			MinimumCustomersPerHour: 5, PaidCustomersPerHourlyCustomer: 400,
			MinimumCustomersPerDay: 10, PaidCustomersPerDailyCustomer: 100,
		},
		DestructiveActionLimits: DestructiveActionLimitsConfig{
			ActorPerHour: 5, ActorPerDay: 15, MerchantPerHour: 15, MerchantPerDay: 25,
			DeploymentPerHour: 50, DeploymentPerDay: 100, ErasurePerHour: 100, ErasurePerDay: 1000,
		},
		Captcha:              CaptchaAccount{Provider: "turnstile"},
		Vault:                VaultConfig{ScopePrefix: "openrails", TransitMount: "transit"},
		EventRetention:       2160 * time.Hour,
		WebhookSecretOverlap: 24 * time.Hour,
	}
}

// WithDefaults is c with every zero setting, field by field, at
// DefaultConfig's value, and an empty Database.RiverSchema at the schema plus
// "_river". A nil RateLimits takes the defaults; an empty one stays empty.
func (c Config) WithDefaults() Config {
	c.Database.Schema = strings.ToLower(strings.TrimSpace(c.Database.Schema))
	fill(reflect.ValueOf(&c).Elem(), reflect.ValueOf(DefaultConfig()))
	c.Database.RiverSchema = cmp.Or(strings.ToLower(strings.TrimSpace(c.Database.RiverSchema)), riverSchema(c.Database.Schema))
	return c
}

// fill sets each zero field of dst, a struct's fields one by one, to src's.
func fill(dst, src reflect.Value) {
	if dst.Kind() == reflect.Struct {
		for i := range dst.NumField() {
			fill(dst.Field(i), src.Field(i))
		}
		return
	}
	if dst.IsZero() {
		dst.Set(src)
	}
}

// riverSchema is the River schema of an engine in schema when none is named.
func riverSchema(schema string) string { return schema + "_river" }
