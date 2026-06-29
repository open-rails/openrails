// Package catalog implements a terraform-style declarative "catalog-as-code"
// apply for the OpenRails billing catalog (issue #162).
//
// A manifest is a YAML file describing the desired catalog: products > prices.
// Applying it converges OpenRails (and, via
// the existing declarative-provider dispatch in issue #208, every configured
// payment rail) onto that desired state. The pipeline is the proven
// cozy-art shape — load → validate → plan → print → apply — with two
// identity rules:
//
//   - A product's identity is its slug.
//   - A price's identity is its FINANCIAL SUBSTANCE (currency, unit_amount,
//     interval, interval_count). There is no price slug. Prices are a SET:
//     declared prices are ensured active; an active OpenRails price whose
//     financial identity is not declared is archived.
//
// The schema includes per-product / per-price `providers:` lists and optional
// `provider_links` so a single apply fans out across Stripe, NMI, CCBill and
// Solana.
package catalog

// Manifest is the root of a catalog-as-code document.
//
// Every price declares its own `currency` explicitly — there is no catalog-wide
// currency default. The `default_providers` key lets a manifest set a
// catalog-wide provider list that individual products/prices inherit when they
// don't specify their own.
type Manifest struct {
	Version          int          `json:"version" yaml:"version"`
	DefaultProviders []string     `json:"default_providers,omitempty" yaml:"default_providers,omitempty"`
	Products         []Product    `json:"products,omitempty" yaml:"products,omitempty"`
	Meters           []Meter      `json:"meters,omitempty" yaml:"meters,omitempty"`
	UsageLimits      []UsageLimit `json:"usage_limits,omitempty" yaml:"usage_limits,omitempty"`

	TierGroups []TierGroup `json:"-" yaml:"-"`
}

type Meter struct {
	Key  string `json:"key" yaml:"key"`
	Kind string `json:"kind" yaml:"kind"`
}

type UsageLimit struct {
	Key     string             `json:"key" yaml:"key"`
	Measure string             `json:"measure" yaml:"measure"`
	Windows []UsageLimitWindow `json:"windows" yaml:"windows"`
}

type UsageLimitWindow struct {
	Window string `json:"window" yaml:"window"`
	Amount int64  `json:"amount" yaml:"amount"`
}

// TierGroup is a named grouping of products (e.g. a subscription plan family).
type TierGroup struct {
	Slug        string    `json:"slug" yaml:"slug"`
	DisplayName string    `json:"display_name" yaml:"display_name"`
	Products    []Product `json:"products" yaml:"products"`
}

// Product is a declared product. Identity is its slug.
type Product struct {
	Slug        string `json:"slug" yaml:"slug"`
	Key         string `json:"key,omitempty" yaml:"key,omitempty"`
	DisplayName string `json:"display_name" yaml:"display_name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	TierGroup   string `json:"tier_group,omitempty" yaml:"tier_group,omitempty"`
	TierRank    *int   `json:"tier_rank,omitempty" yaml:"tier_rank,omitempty"`
	// Status maps to the OpenRails CatalogStatus enum (draft|active|archived,
	// issue #210). Empty defaults to active.
	Status       string   `json:"status,omitempty" yaml:"status,omitempty"`
	Entitlements []string `json:"entitlements,omitempty" yaml:"entitlements,omitempty"`
	Credits      Credits  `json:"credits,omitempty" yaml:"credits,omitempty"`
	UsageLimits  []string `json:"usage_limits,omitempty" yaml:"usage_limits,omitempty"`
	Includes     []string `json:"includes,omitempty" yaml:"includes,omitempty"`

	// Providers is the per-product default provider list inherited by this
	// product's prices when a price does not declare its own (issue #208).
	// Empty falls back to the manifest's default_providers.
	Providers []string `json:"providers,omitempty" yaml:"providers,omitempty"`

	Prices []Price `json:"prices,omitempty" yaml:"prices,omitempty"`
}

type Credits map[string]CreditGrant

type CreditGrant struct {
	Unit        string `json:"unit,omitempty" yaml:"unit,omitempty"`
	Currency    string `json:"currency,omitempty" yaml:"currency,omitempty"`
	Amount      int64  `json:"amount" yaml:"amount"`
	ExpiresDays *int   `json:"expires_days,omitempty" yaml:"expires_days,omitempty"`
	Expires     string `json:"expires,omitempty" yaml:"expires,omitempty"`
	Cadence     string `json:"cadence,omitempty" yaml:"cadence,omitempty"`
}

func (p Product) tierRank() int {
	if p.TierRank == nil {
		return 0
	}
	return *p.TierRank
}

// Price is a declared price. It has NO slug: a price's identity is its
// financial substance (currency, unit_amount, interval, interval_count).
type Price struct {
	Currency      string `json:"currency,omitempty" yaml:"currency,omitempty"`
	UnitAmount    int64  `json:"unit_amount" yaml:"unit_amount"`
	Interval      string `json:"interval,omitempty" yaml:"interval,omitempty"`
	IntervalCount int    `json:"interval_count,omitempty" yaml:"interval_count,omitempty"`

	// Status maps to the OpenRails CatalogStatus enum. Empty defaults to active.
	Status string `json:"status,omitempty" yaml:"status,omitempty"`

	// Providers overrides the product/manifest provider list for this price.
	// nil means "inherit"; an explicit empty list ([]) means "DB-only, no
	// external providers".
	Providers []string `json:"providers,omitempty" yaml:"providers,omitempty"`

	// ProviderLinks pre-supplies provider-specific link ids, mapping
	// provider name -> key/value pairs. Maps straight onto
	// service.CreatePriceRequest.ProviderLinks. A supplied link is VALIDATED
	// against the provider (object exists + matches the price's money terms)
	// before it is accepted; a mismatch fails the apply loudly. An existing
	// object is never duplicated. A MISSING object is created where the linked id
	// is client-creatable (NMI plan_id, Stripe lookup_key) and errors where it is
	// provider-generated (Stripe price_id, Solana plan_pda). Canonical keys:
	//   provider_links:
	//     stripe: {lookup_key: premium}                        # recommended: find-or-create at a chosen key ...
	//     stripe: {price_id: price_xxx, product_id: prod_xxx}  # ... or pin an exact existing Price (require-exists)
	//     mobius: {plan_id: premium}                           # NMI recurring plan; find-or-create at this id
	//     solana: {plan_pda: 7Xy...PdA}                        # existing on-chain plan account
	//     ccbill: {form_name: premium, flex_id: abc-123}       # operator-owned, unvalidated
	ProviderLinks map[string]map[string]string `json:"provider_links,omitempty" yaml:"provider_links,omitempty"`

	// LegacyImport, when true, declares a historical price that already has
	// subscribers but is no longer purchasable: it is created/converged to
	// status=archived rather than active (no purchasable gap, no double-charge).
	LegacyImport bool `json:"legacy_import,omitempty" yaml:"legacy_import,omitempty"`

	// Intro is an optional introductory/trial FIRST period that differs from the
	// recurring terms above (#602). amount=0 is a free trial. Omit for a flat price.
	//   intro: {amount: 1995, period_days: 30}  # $19.95 first 30d, then UnitAmount recurring
	//   intro: {amount: 0, period_days: 7}       # free 7-day trial, then UnitAmount recurring
	Intro *PriceIntro `json:"intro,omitempty" yaml:"intro,omitempty"`

	Metered *MeteredPrice `json:"metered,omitempty" yaml:"metered,omitempty"`
}

type MeteredPrice struct {
	Meter    string `json:"meter" yaml:"meter"`
	Rate     int64  `json:"rate" yaml:"rate"`
	PerUnits int64  `json:"per_units,omitempty" yaml:"per_units,omitempty"`
	Per      string `json:"per,omitempty" yaml:"per,omitempty"`
}

// PriceIntro is the introductory/trial first period for a Price (#602): a first
// period at its own price/length, then the Price's recurring terms.
type PriceIntro struct {
	Amount     int64 `json:"amount" yaml:"amount"`           // first-period price (0 = free trial)
	PeriodDays int   `json:"period_days" yaml:"period_days"` // first-period length in days
}

// providersFor returns the effective provider list for a price, applying the
// inheritance chain price -> product -> manifest. A nil price-level Providers
// inherits; an explicit empty slice means "no providers".
func (m *Manifest) providersFor(product Product, price Price) []string {
	if price.Providers != nil {
		return price.Providers
	}
	if product.Providers != nil {
		return product.Providers
	}
	return m.DefaultProviders
}
