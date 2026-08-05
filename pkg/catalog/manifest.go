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
//     access duration, renewal flag, and trial terms). There is no price slug. Prices are a SET:
//     declared prices are ensured active; an active OpenRails price whose
//     financial identity is not declared is archived.
//
// Each price declares its own `providers:` list and optional `provider_links`
// so apply can fan out explicitly across Stripe, NMI, CCBill and Solana.
package catalog

import (
	"bytes"
	"encoding/json"
)

// Manifest is the root of a catalog-as-code document.
//
// Every price declares its own `currency` and `providers` explicitly — there
// are no catalog/product-level defaults.
type Manifest struct {
	Version        int             `json:"version" yaml:"version"`
	Products       []Product       `json:"products,omitempty" yaml:"products,omitempty"`
	Meters         []Meter         `json:"meters,omitempty" yaml:"meters,omitempty"`
	CreditBalances []CreditBalance `json:"credit_balances,omitempty" yaml:"credit_balances,omitempty"`
	UsageLimits    []UsageLimit    `json:"usage_limits,omitempty" yaml:"usage_limits,omitempty"`

	TierGroups []TierGroup `json:"-" yaml:"-"`
}

// Meter is a billed usage stream: {key, event_type, value_property,
// aggregation, group_by, unit} (OpenMeter-style, #638). aggregation is required
// and is one of sum/count/max/min/unique_count/latest. The #599 `kind:
// counter|gauge` shape was retired by or#893 — a rate card is the only pricing
// input.
type Meter struct {
	Key string `json:"key" yaml:"key"`

	EventType     string            `json:"event_type,omitempty" yaml:"event_type,omitempty"`
	ValueProperty string            `json:"value_property,omitempty" yaml:"value_property,omitempty"`
	Aggregation   string            `json:"aggregation,omitempty" yaml:"aggregation,omitempty"`
	Unit          string            `json:"unit,omitempty" yaml:"unit,omitempty"`
	GroupBy       map[string]string `json:"group_by,omitempty" yaml:"group_by,omitempty"`
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
	// Archived maps to status=archived. Omitted/false = active (matches the
	// merchant-manifest provider-account `archived` key).
	Archived     bool          `json:"archived,omitempty" yaml:"archived,omitempty"`
	Entitlements []string      `json:"entitlements,omitempty" yaml:"entitlements,omitempty"`
	Credits      []CreditGrant `json:"credits,omitempty" yaml:"credits,omitempty"`
	UsageLimits  []string      `json:"usage_limits,omitempty" yaml:"usage_limits,omitempty"`
	Includes     []string      `json:"includes,omitempty" yaml:"includes,omitempty"`

	Prices []Price `json:"prices,omitempty" yaml:"prices,omitempty"`

	// Rate-card model (#638): metered usage / flat-fee rate cards and a variable
	// credit top-up (#639/#640). A usage product declares no billing cadence — its
	// cap/allowance window is the invoice period (calendar-month via the merchant
	// invoice boundary), so collection cadence is a billing-policy concern (#643),
	// not catalog (#642).
	RateCards []RateCard `json:"rate_cards,omitempty" yaml:"rate_cards,omitempty"`
}

type CreditBalance struct {
	Key  string `json:"key" yaml:"key"`
	Unit string `json:"unit" yaml:"unit"`
	// ExpiresDefault is the DECLARED expiry every grant into this balance
	// inherits unless it declares its own `expires` (e.g. "365d"). Omitting it
	// means balances in this bucket never expire — OpenRails supplies no
	// implicit clock of its own (#857).
	ExpiresDefault string `json:"expires_default,omitempty" yaml:"expires_default,omitempty"`
}

// CreditGrant is one product-bundled deposit into a declared credit balance.
// Expiry comes from `expires` here, else the balance's `expires_default`, else
// nowhere — and "nowhere" means the granted balance never expires (#857).
type CreditGrant struct {
	Key string `json:"key" yaml:"key"`
	// Unit is resolved from the referenced CreditBalance during validation.
	Unit string `json:"-" yaml:"-"`
	// Currency is internal state; catalog v1 does not declare it on grants.
	Currency string `json:"-" yaml:"-"`
	Amount   *int64 `json:"amount,omitempty" yaml:"amount,omitempty"`
	// ExpiryHours is resolved from Expires or the balance default.
	ExpiryHours *int   `json:"-" yaml:"-"`
	Expires     string `json:"expires,omitempty" yaml:"expires,omitempty"`
	Cadence     string `json:"cadence,omitempty" yaml:"cadence,omitempty"`
}

// UnmarshalJSON keeps the HTTP catalog manifest as strict as the YAML parser.
func (g *CreditGrant) UnmarshalJSON(raw []byte) error {
	type manifestCreditGrant CreditGrant

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var declared manifestCreditGrant
	if err := decoder.Decode(&declared); err != nil {
		return err
	}
	*g = CreditGrant(declared)
	return nil
}

func (p Product) tierRank() int {
	if p.TierRank == nil {
		return 0
	}
	return *p.TierRank
}

// Price is a declared price. Its ROW IDENTITY is still its financial
// substance (currency, unit_amount, duration, auto_renew) — there is no price
// slug for THAT. Key (#774) is a separate, orthogonal concept: a durable,
// merchant-unique movable-pointer handle naming this price's substance-
// version chain, independent of row identity.
type Price struct {
	Currency   string `json:"currency,omitempty" yaml:"currency,omitempty"`
	UnitAmount int64  `json:"unit_amount" yaml:"unit_amount"` // micros (millionths of a major unit)

	// Key (#774) is optional; when omitted it auto-defaults to
	// "<product-key>-<interval>" (see billingservice.PriceIntervalLabel) as
	// long as the product declares only ONE price at that interval. Two or
	// more prices at the same interval (e.g. a promo alongside the standard
	// price) must each carry an explicit key — apply refuses the ambiguity
	// loudly rather than silently colliding both onto the same default.
	Key string `json:"key,omitempty" yaml:"key,omitempty"`

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

	// Archived maps to status=archived. Omitted/false = active.
	Archived bool `json:"archived,omitempty" yaml:"archived,omitempty"`

	// PSPs is the explicit payment-service-provider list for this price: the
	// merchant's PSP keys (e.g. mobius) — reserved gateways (stripe, ccbill,
	// solana) are their own PSP names. Omitted or empty means OpenRails-native
	// only: no external provider sync.
	PSPs []string `json:"psps,omitempty" yaml:"psps,omitempty"`

	// PSPLinks pre-supplies PSP-specific link ids, mapping
	// PSP key -> key/value pairs. Maps straight onto
	// service.CreatePriceRequest.PSPLinks. A supplied link is VALIDATED
	// against the provider (object exists + matches the price's money terms)
	// before it is accepted; a mismatch fails the apply loudly. An existing
	// object is never duplicated. A MISSING object is created where the linked id
	// is client-creatable (NMI plan_id, Stripe lookup_key) and errors where it is
	// provider-generated (Stripe price_id, Solana plan_pda). Canonical keys:
	//   psp_links:
	//     stripe: {lookup_key: premium}                        # recommended: find-or-create at a chosen key ...
	//     stripe: {price_id: price_xxx, product_id: prod_xxx}  # ... or pin an exact existing Price (require-exists)
	//     mobius: {plan_id: premium}                           # NMI recurring plan on the "mobius" PSP; find-or-create
	//     solana: {plan_pda: 7Xy...PdA}                        # existing on-chain plan account
	//     ccbill: {form_name: premium, flex_id: abc-123}       # operator-owned, unvalidated
	PSPLinks map[string]map[string]string `json:"psp_links,omitempty" yaml:"psp_links,omitempty"`

	// Trial is an optional FIRST phase that differs from the recurring terms above
	// (#622). unit_amount=0 is a free trial. Requires auto_renew. Omit for a flat
	// price.
	//   trial: {unit_amount: 19950000, duration: 30d}  # $19.95 (micros) first 30d, then UnitAmount recurring
	//   trial: {unit_amount: 0, duration: 7d}           # free 7-day trial, then UnitAmount recurring
	Trial *PriceTrial `json:"trial,omitempty" yaml:"trial,omitempty"`

	// Variable credit top-up offer fields. Fixed catalog prices keep using the
	// UnitAmount/Duration shape above.
	InputMin int64         `json:"input_min,omitempty" yaml:"input_min,omitempty"`
	InputMax int64         `json:"input_max,omitempty" yaml:"input_max,omitempty"`
	Round    string        `json:"round,omitempty" yaml:"round,omitempty"`
	Model    string        `json:"model,omitempty" yaml:"model,omitempty"`
	Flat     *FlatPrice    `json:"flat,omitempty" yaml:"flat,omitempty"`
	PerUnit  *PerUnitPrice `json:"per_unit,omitempty" yaml:"per_unit,omitempty"`
	Tiered   *TieredPrice  `json:"tiered,omitempty" yaml:"tiered,omitempty"`
	Package  *PackagePrice `json:"package,omitempty" yaml:"package,omitempty"`
}

// PriceTrial is the trial first phase for a Price (#622): a first phase at its
// own price/length, then the Price's recurring terms. Requires the price's
// auto_renew.
type PriceTrial struct {
	UnitAmount int64  `json:"unit_amount" yaml:"unit_amount"` // first-phase price in micros (0 = free trial)
	Duration   string `json:"duration" yaml:"duration"`       // first-phase length
}

func (p Price) ratePrice() RatePrice {
	return RatePrice{
		Model:    p.Model,
		Currency: p.Currency,
		Flat:     p.Flat,
		PerUnit:  p.PerUnit,
		Tiered:   p.Tiered,
		Package:  p.Package,
	}
}

func (p Price) withRatePrice(rp RatePrice) Price {
	p.Model = rp.Model
	p.Currency = rp.Currency
	p.Flat = rp.Flat
	p.PerUnit = rp.PerUnit
	p.Tiered = rp.Tiered
	p.Package = rp.Package
	return p
}
