// Package catalog implements a terraform-style declarative "catalog-as-code"
// apply for the OpenRails billing catalog (issue #162).
//
// A manifest is a YAML file describing the desired catalog: a tree of
// tier_groups > products > prices. Applying it converges OpenRails (and, via
// the existing declarative-provider dispatch in issue #208, every configured
// payment processor) onto that desired state. The pipeline is the proven
// cozy-art shape — load → validate → plan → print → apply — with two
// identity rules:
//
//   - A product's identity is its slug.
//   - A price's identity is its FINANCIAL SUBSTANCE (currency, unit_amount,
//     interval, interval_count). There is no price slug. Prices are a SET:
//     declared prices are ensured active; an active OpenRails price whose
//     financial identity is not declared is archived.
//
// The schema is kept drop-in compatible with cozy-art's billing_catalog.yaml,
// extended with a per-product / per-price `providers:` list (issue #208) and
// optional `provider_links` so a single apply fans out across Stripe, NMI,
// CCBill and Solana.
package catalog

// Manifest is the root of a catalog-as-code document.
//
// Every price declares its own `currency` explicitly — there is no catalog-wide
// currency default. The `default_providers` key lets a manifest set a
// catalog-wide provider list that individual products/prices inherit when they
// don't specify their own.
type Manifest struct {
	Version          int         `json:"version" yaml:"version"`
	DefaultProviders []string    `json:"default_providers,omitempty" yaml:"default_providers,omitempty"`
	TierGroups       []TierGroup `json:"tier_groups" yaml:"tier_groups"`
}

// TierGroup is a named grouping of products (e.g. a subscription plan family).
// Mirrors cozy-art's tier_groups nesting for drop-in compatibility.
type TierGroup struct {
	Slug        string    `json:"slug" yaml:"slug"`
	DisplayName string    `json:"display_name" yaml:"display_name"`
	Products    []Product `json:"products" yaml:"products"`
}

// Product is a declared product. Identity is its slug.
type Product struct {
	Slug        string `json:"slug" yaml:"slug"`
	DisplayName string `json:"display_name" yaml:"display_name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	TierRank    int    `json:"tier_rank" yaml:"tier_rank"`
	// Status maps to the OpenRails CatalogStatus enum (draft|active|archived,
	// issue #210). Empty defaults to active.
	Status       string   `json:"status,omitempty" yaml:"status,omitempty"`
	Entitlements []string `json:"entitlements,omitempty" yaml:"entitlements,omitempty"`

	// Providers is the per-product default provider list inherited by this
	// product's prices when a price does not declare its own (issue #208).
	// Empty falls back to the manifest's default_providers.
	Providers []string `json:"providers,omitempty" yaml:"providers,omitempty"`

	Prices []Price `json:"prices" yaml:"prices"`
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

	// StripePriceID is a cozy-art-compatible shorthand for
	// provider_links.stripe.price_id. When set it is folded into ProviderLinks
	// during load. This keeps cozy-art's billing_catalog.yaml loadable as-is.
	StripePriceID string `json:"stripe_price_id,omitempty" yaml:"stripe_price_id,omitempty"`

	// LegacyImport, when true, declares a historical price that already has
	// subscribers but is no longer purchasable: it is created/converged to
	// status=archived rather than active (no purchasable gap, no double-charge).
	LegacyImport bool `json:"legacy_import,omitempty" yaml:"legacy_import,omitempty"`
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
