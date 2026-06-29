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
//   - A product's identity is its key.
//   - A price's identity is its FINANCIAL SUBSTANCE (currency, unit_amount,
//     interval). There is no price slug. Prices are a SET:
//     declared prices are ensured active; an active OpenRails price whose
//     financial identity is not declared is archived.
//
// Each price declares its own `providers:` list and optional `provider_links`
// so apply can fan out explicitly across Stripe, NMI, CCBill and Solana.
package catalog

// Manifest is the root of a catalog-as-code document.
//
// Every price declares its own `currency` and `providers` explicitly — there
// are no catalog/product-level defaults.
type Manifest struct {
	Version     int          `json:"version" yaml:"version"`
	Products    []Product    `json:"products,omitempty" yaml:"products,omitempty"`
	Meters      []Meter      `json:"meters,omitempty" yaml:"meters,omitempty"`
	UsageLimits []UsageLimit `json:"usage_limits,omitempty" yaml:"usage_limits,omitempty"`

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
	Key         string    `json:"key" yaml:"key"`
	DisplayName string    `json:"display_name" yaml:"display_name"`
	Products    []Product `json:"products" yaml:"products"`
}

// Product is a declared product. Identity is its key.
type Product struct {
	Key         string `json:"key" yaml:"key"`
	DisplayName string `json:"display_name" yaml:"display_name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	TierGroup   string `json:"tier_group,omitempty" yaml:"tier_group,omitempty"`
	TierRank    *int   `json:"tier_rank,omitempty" yaml:"tier_rank,omitempty"`
	// Active mirrors Stripe's product/price availability model. nil defaults to true.
	Active       *bool    `json:"active,omitempty" yaml:"active,omitempty"`
	Entitlements []string `json:"entitlements,omitempty" yaml:"entitlements,omitempty"`
	Credits      Credits  `json:"credits,omitempty" yaml:"credits,omitempty"`
	UsageLimits  []string `json:"usage_limits,omitempty" yaml:"usage_limits,omitempty"`
	Includes     []string `json:"includes,omitempty" yaml:"includes,omitempty"`

	Prices []Price `json:"prices,omitempty" yaml:"prices,omitempty"`
}

type Credits map[string]CreditGrant

type CreditGrant struct {
	Unit        string `json:"unit,omitempty" yaml:"unit,omitempty"`
	Currency    string `json:"currency,omitempty" yaml:"currency,omitempty"`
	Amount      int64  `json:"amount" yaml:"amount"`
	ExpiryHours *int   `json:"expiry_hours,omitempty" yaml:"expiry_hours,omitempty"`
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
// financial substance (currency, unit_amount, duration, auto_renew).
type Price struct {
	Currency   string `json:"currency,omitempty" yaml:"currency,omitempty"`
	UnitAmount int64  `json:"unit_amount" yaml:"unit_amount"`

	// Duration is the access window a purchase of this price grants (#622):
	// a finite value (`3d`/`30d`/`90d`/`365d`) or `indefinite`. Optional;
	// defaults to `indefinite` (durable/perpetual ownership). It is the single
	// source for the window — it drives both product-access ends_at and the
	// derived entitlement end_at.
	Duration string `json:"duration,omitempty" yaml:"duration,omitempty"`

	// AutoRenew says whether the price charges again and extends the window after
	// Duration (#622). Optional, defaults to false. A finite Duration is required
	// to renew — auto_renew with an indefinite/omitted Duration is rejected.
	AutoRenew bool `json:"auto_renew,omitempty" yaml:"auto_renew,omitempty"`

	// Active mirrors Stripe's product/price availability model. nil defaults to true.
	Active *bool `json:"active,omitempty" yaml:"active,omitempty"`

	// Providers is the explicit provider list for this price. Omitted or empty
	// means OpenRails-native only: no external provider sync.
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

	// Trial is an optional FIRST phase that differs from the recurring terms above
	// (#622). unit_amount=0 is a free trial. Requires auto_renew. Omit for a flat
	// price.
	//   trial: {unit_amount: 1995, duration: 30d}  # $19.95 first 30d, then UnitAmount recurring
	//   trial: {unit_amount: 0, duration: 7d}       # free 7-day trial, then UnitAmount recurring
	Trial *PriceTrial `json:"trial,omitempty" yaml:"trial,omitempty"`

	Metered *MeteredPrice `json:"metered,omitempty" yaml:"metered,omitempty"`
}

type MeteredPrice struct {
	Meter    string `json:"meter" yaml:"meter"`
	Rate     int64  `json:"rate" yaml:"rate"`
	PerUnits int64  `json:"per_units,omitempty" yaml:"per_units,omitempty"`
	Per      string `json:"per,omitempty" yaml:"per,omitempty"`
}

// PriceTrial is the trial first phase for a Price (#622): a first phase at its
// own price/length, then the Price's recurring terms. Requires the price's
// auto_renew.
type PriceTrial struct {
	UnitAmount int64  `json:"unit_amount" yaml:"unit_amount"` // first-phase price (0 = free trial)
	Duration   string `json:"duration" yaml:"duration"`       // first-phase length
}
