package billing

import (
	"cmp"
	"reflect"
	"strings"
)

// Config is the engine's settings: plain data, as openrails.New takes them
// and as the standalone server's file spells them (the yaml keys).
// Everything that reaches outside the process is in openrails.Deps. Whether
// money is real is each PSP's declared environment, never the deployment's;
// how much OpenRails may write to providers, and whether the charges it
// initiates wait, are each merchant's controls.
// A field left zero takes DefaultConfig's value (WithDefaults).
type Config struct {
	// Database names the Postgres schemas New creates or upgrades.
	Database DatabaseConfig `yaml:"database"`
	// ClickHouse names the database of the event store, Deps.ClickHouse. The
	// standalone server takes it from clickhouse.url's path. Set in code.
	ClickHouse ClickHouseConfig `yaml:"-"`
	// Merchants declares the merchants an embedded engine provisions on
	// start; none leaves that to run time (a standalone server's are
	// provisioned through its API). A slug declared twice provisions neither
	// entry. Without a Vault KV mount a declaration is the merchant's
	// configuration, read-only; with one Vault serves it, and a declaration
	// naming configuration beside its slug is ignored and reported. A request
	// acts on the merchant its credential is bound to: see
	// MerchantDeclaration.Issuer. Set in code.
	Merchants []MerchantDeclaration `yaml:"-"`

	// PublicURL is where browsers and PSPs reach billing: the external billing
	// mount, excluding /v1. Provider callbacks and customer billing links are
	// built on it.
	PublicURL string `yaml:"public_url"`
	// TrustedProxies are the CIDRs whose X-Forwarded-For is trusted when
	// resolving a client's address. Empty trusts none: the socket peer is the
	// client.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// CheckoutOrigin is needed only to serve OpenRails's own hosted pay page:
	// the separate origin (https://pay.example.com) where /pay/<order> is
	// served from Deps.CheckoutAssets; leave it empty when you use the
	// embedded checkout or your own card form, and pay URLs then answer
	// pay_url_unavailable. It is an origin of its own, apart from
	// PublicURL and every declared Issuer; one that is not is
	// refused and reported.
	CheckoutOrigin string `yaml:"checkout_origin"`

	// RateLimits are the per-bucket request limits.
	RateLimits RateLimitsConfig `yaml:"rate_limits"`
	// Captcha challenges a client that keeps hitting a rate limit; it is on
	// when both its keys are set.
	Captcha CaptchaAccount `yaml:"captcha"`

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
	// ReadCache overrides how long the engine's handles serve catalog and
	// configuration reads from memory. Set in code: the standalone server
	// reads through no handle.
	ReadCache ReadCache `yaml:"-"`
}

// DatabaseConfig names the Postgres schemas OpenRails keeps its tables in.
// The role of the pool New runs with owns every object in them.
type DatabaseConfig struct {
	// Schema holds OpenRails' tables: a plain identifier.
	Schema string `yaml:"schema"`
	// RiverSchema holds the River tables OpenRails' jobs run in; empty is
	// Schema plus "_river", apart from AuthKit's and the host's River. Start
	// runs OpenRails' own River client there, and a host fleet passed to
	// Start with WithRiverClient must use the same schema. Set in code.
	RiverSchema string `yaml:"-"`
}

// ClickHouseConfig names the ClickHouse database the event store keeps its
// tables in. New creates or upgrades it, as openrails.MigrateClickHouse does.
type ClickHouseConfig struct {
	// Database holds the event store's tables.
	Database string `yaml:"-"`
}

// RateLimitsConfig is each bucket's request limit. A bucket left nil takes
// DefaultConfig's; RequestsPerMinute 0 does not limit it, so a host that
// fronts billing with its own limiter sets every bucket to 0. Other routes
// are never limited (a per-address ceiling belongs to the proxy in front of
// OpenRails). Counting is per one-minute window, bucket and subject (the
// client address, plus the user when authenticated); any subject tripping
// refuses the request with 429. In-process Client calls count nothing. The
// client address is the socket peer unless TrustedProxies lists the proxy.
type RateLimitsConfig struct {
	// Purchase limits buying: checkout and paying an order, kept tight to
	// deter card testing.
	Purchase *RateLimit `yaml:"purchase"`
	// Payment limits a customer's payment-method routes, where cards are
	// added.
	Payment *RateLimit `yaml:"payment"`
	// Subscribe limits a customer's subscription changes.
	Subscribe *RateLimit `yaml:"subscribe"`
	// Webhook limits provider callbacks, which share their rail's source
	// addresses: a DoS floor that must absorb rebill runs.
	Webhook *RateLimit `yaml:"webhook"`
}

// RateLimit is one bucket's limit over a fixed one-minute window; 0 does not
// limit it.
type RateLimit struct {
	RequestsPerMinute int `yaml:"requests_per_minute"`
}

// CaptchaAccount is the deployment's captcha account. Setting both keys
// enables the challenge, which is asked only of a client that keeps hitting a
// rate limit on purchase, payment methods or subscriptions. CaptchaConfig is
// what a browser is told about it.
type CaptchaAccount struct {
	// Provider is "turnstile", "recaptcha-v3" or "hcaptcha".
	Provider string `yaml:"provider"`
	// SiteKey is the account's public key, which the browser's widget loads.
	SiteKey string `yaml:"site_key"`
	// SecretKey is the account's secret, which verifies a solved challenge.
	SecretKey string `yaml:"secret_key"`
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
// posture, a Basis Theory tenant: merchants with settings.card_vault platform
// store new cards in it, each in its own partition. Its keys are in Vault, at
// <kv_mount>/<scope_prefix>/platform_vault/<tenant_id>.
type CardVaultConfig struct {
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
//
// Both are https, or http on a loopback IP literal: a fixture, refused
// beside a live PSP.
type HyperSwitchConfig struct {
	// APIBaseURL is the deployment's HyperSwitch API.
	APIBaseURL string `yaml:"api_base_url"`
	// SDKURL is the HyperSwitch browser SDK the payment page loads.
	SDKURL string `yaml:"sdk_url"`
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

// DefaultConfig is every engine setting's default: the value a setting left
// zero takes. A default inside an optional section does not turn it on.
func DefaultConfig() Config {
	return Config{
		Database:   DatabaseConfig{Schema: "billing"},
		ClickHouse: ClickHouseConfig{Database: "openrails"},
		RateLimits: RateLimitsConfig{
			Purchase: &RateLimit{RequestsPerMinute: 10},
			// The per-user limit is the real fraud control.
			Subscribe: &RateLimit{RequestsPerMinute: 20},
			Payment:   &RateLimit{RequestsPerMinute: 40},
			Webhook:   &RateLimit{RequestsPerMinute: 1200},
		},
		Captcha: CaptchaAccount{Provider: "turnstile"},
		Vault:   VaultConfig{ScopePrefix: "openrails", TransitMount: "transit"},
	}
}

// WithDefaults is c with every zero setting, field by field, at
// DefaultConfig's value, and an empty Database.RiverSchema at the schema plus
// "_river".
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
