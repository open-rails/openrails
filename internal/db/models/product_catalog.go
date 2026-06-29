package models

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// CatalogStatus is the lifecycle state of a catalog product or price. It
// replaces the previous is_active boolean so that "draft (not yet launched)"
// and "archived (retired but grandfathered)" — both formerly is_active=false —
// are distinct, first-class states.
type CatalogStatus string

const (
	// CatalogStatusDraft: created but not launched. Not purchasable, hidden from
	// the public catalog, not created in Stripe. No subscribers yet.
	CatalogStatusDraft CatalogStatus = "draft"
	// CatalogStatusActive: live. Purchasable, shown in the catalog, billed normally.
	CatalogStatusActive CatalogStatus = "active"
	// CatalogStatusArchived: retired. Not purchasable, hidden from the
	// public catalog, but existing subscriptions are grandfathered and bill
	// indefinitely. Stripe active=false.
	CatalogStatusArchived CatalogStatus = "archived"
)

// Valid reports whether the status is one of the known lifecycle values.
func (s CatalogStatus) Valid() bool {
	switch s {
	case CatalogStatusDraft, CatalogStatusActive, CatalogStatusArchived:
		return true
	default:
		return false
	}
}

// Product represents a product offering (e.g., Premium Membership)
// This represents our product catalog concept
type Product struct {
	ID          uuid.UUID `json:"id"`
	MerchantID  uuid.UUID `json:"merchant_id"`
	Slug        string    `json:"slug"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`

	// Entitlements configuration: map entitlement name -> duration days (nil or 0 means indefinite)
	EntitlementsSpec map[string]*int `json:"entitlements_spec,omitempty"`

	// Credits configuration: bundled promo credits for subscriptions
	CreditsSpec CreditsSpec `json:"credits_spec,omitempty"`

	// Tier configuration for upgrade/downgrade relationships
	// Products in the same TierGroup are mutually exclusive - user must upgrade/downgrade between them
	// TierRank determines direction: higher rank = more premium (upgrade), lower rank = downgrade
	TierGroup *string `json:"tier_group,omitempty"`
	TierRank  int     `json:"tier_rank"`

	Status    CatalogStatus `json:"status"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`

	// Relationships
	Prices []*Price `json:"prices,omitempty"`
}

// IsPurchasable reports whether the product can be bought by a new customer
// (shown in the public catalog). Only active products are purchasable.
func (p *Product) IsPurchasable() bool { return p.Status == CatalogStatusActive }

// IsBillable reports whether existing subscriptions on this product may still
// be billed. Archived products are grandfathered, so anything that is not a
// draft remains billable.
func (p *Product) IsBillable() bool { return p.Status != CatalogStatusDraft }

// CreditsSpec defines bundled credit/currency balance grants for a product — the
// other half of "what you get" alongside entitlements (#472). Each entry deposits
// a balance on purchase / subscription event.
//
// Keys are grant labels (scope idempotency). Amount is in the minor units of Unit.
type CreditsSpec map[string]CreditGrantSpec

type CreditGrantCadence string

const (
	CreditGrantCadenceOnce       CreditGrantCadence = "once"
	CreditGrantCadencePerRenewal CreditGrantCadence = "per_renewal"
)

// DefaultCreditGrantExpiryDays is the grant expiry when expiry_days is omitted (#472).
const DefaultCreditGrantExpiryDays = 365

type CreditGrantSpec struct {
	// Unit is the currency code of the granted balance (#472). Required.
	// Unqualified = built-in currency; a future merchant/name = custom credit (#473).
	Unit   string `json:"unit,omitempty"`
	Amount int64  `json:"amount"`
	// ExpiryDays: balance expires now+N days. null/omitted => 365 default; explicit
	// 0 => never expires (#472).
	ExpiryDays *int `json:"expiry_days,omitempty"`
	// ExpiresDays is the legacy field name, still parsed for backward compat.
	ExpiresDays *int               `json:"expires_days,omitempty"`
	Cadence     CreditGrantCadence `json:"cadence,omitempty"` // once|per_renewal (default once)
}

// UnitCode returns the grant's currency/custom-credit unit code.
func (g CreditGrantSpec) UnitCode() string {
	return g.Unit
}

// EffectiveExpiryDays resolves grant expiry in days (#472): prefers expiry_days,
// then legacy expires_days, else the 365 default. The returned value is 0 only
// when explicitly set to 0 — meaning NEVER expires. null/omitted yields 365.
func (g CreditGrantSpec) EffectiveExpiryDays() int {
	if g.ExpiryDays != nil {
		return *g.ExpiryDays
	}
	if g.ExpiresDays != nil {
		return *g.ExpiresDays
	}
	return DefaultCreditGrantExpiryDays
}

func CloneEntitlementsSpec(spec map[string]*int) map[string]*int {
	if len(spec) == 0 {
		return nil
	}
	out := make(map[string]*int, len(spec))
	for key, value := range spec {
		if value == nil {
			out[key] = nil
			continue
		}
		v := *value
		out[key] = &v
	}
	return out
}

func CloneCreditsSpec(spec CreditsSpec) CreditsSpec {
	if len(spec) == 0 {
		return nil
	}
	out := make(CreditsSpec, len(spec))
	for key, value := range spec {
		cloned := value
		if value.ExpiresDays != nil {
			v := *value.ExpiresDays
			cloned.ExpiresDays = &v
		}
		if value.ExpiryDays != nil {
			v := *value.ExpiryDays
			cloned.ExpiryDays = &v
		}
		out[key] = cloned
	}
	return out
}

// Price represents a specific pricing option for a product
// This represents pricing options similar to Stripe's pricing model
type Price struct {
	ID         uuid.UUID     `json:"id"`
	MerchantID uuid.UUID     `json:"merchant_id"`
	ProductID  uuid.UUID     `json:"product_id"`
	Status     CatalogStatus `json:"status"`
	Amount     int64         `json:"amount"`
	Currency   string        `json:"currency"`

	// AccessDurationHours is the access window (#622) a purchase of this price
	// grants, in HOURS (supports sub-day windows, e.g. a 12h rental). nil =
	// indefinite/durable (perpetual ownership); a positive value = a finite window
	// (rental, one-off, or the billing period when AutoRenew). Drives BOTH
	// product_access_grants.ends_at and the derived entitlement end_at.
	AccessDurationHours *int `json:"access_duration_hours"`

	// AutoRenew (#622) reports whether the price charges again and extends the
	// window at the end of AccessDurationHours. A recurring subscription is
	// AutoRenew=true with a finite AccessDurationHours (the billing period).
	AutoRenew bool `json:"auto_renew"`

	// Trial pricing (#622): an optional FIRST phase that differs from the recurring
	// terms above. TrialUnitAmount = first-phase price (0 = free trial),
	// TrialDurationHours = first-phase length in hours. Both nil = a flat price.
	// Only valid when AutoRenew.
	TrialUnitAmount    *int64 `json:"trial_unit_amount,omitempty"`
	TrialDurationHours *int   `json:"trial_duration_hours,omitempty"`

	// Rails is a JSONB map of rail name -> rail-specific configuration
	// Keys: "mobius", "ccbill", "solana", etc.
	// Values: rail-specific data (e.g., plan_id, price_id, provider)
	// Example: {"mobius": {"plan_id": "123"}, "ccbill": {"price_id": "456"}}
	Rails map[string]map[string]string `json:"rails,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Product       *Product       `json:"-"`
	Subscriptions []Subscription `json:"-"`
	Payments      []Payment      `json:"-"`
}

// IsPurchasable reports whether the price can be bought by a new customer
// (shown in the public catalog). Only active prices are purchasable.
func (p *Price) IsPurchasable() bool { return p.Status == CatalogStatusActive }

// IsBillable reports whether existing subscriptions on this price may still be
// billed. Archived prices are grandfathered, so the renewal/rebill path keeps
// charging them indefinitely — anything that is not a draft remains billable.
func (p *Price) IsBillable() bool { return p.Status != CatalogStatusDraft }

// IsRecurring reports whether the price auto-renews (a subscription). This is
// the #622 replacement for the old "billing_cycle_days != nil" test.
func (p *Price) IsRecurring() bool { return p.AutoRenew }

// RecurringCycleDays returns the billing period in WHOLE DAYS for an
// auto-renewing price, or nil for a one-off/durable price. The window is stored
// in hours; providers (Stripe interval, NMI day-frequency, Solana period) bill
// in days, so the cadence is hours/24. A sub-day auto_renew window floors to 0
// days and is not pushed to a provider (provider adapters guard on > 0).
func (p *Price) RecurringCycleDays() *int {
	if p == nil || !p.AutoRenew || p.AccessDurationHours == nil {
		return nil
	}
	days := *p.AccessDurationHours / 24
	return &days
}

// Rail config key constants (used in the Rails JSONB map)
const (
	RailKeyPlanID         = "plan_id"
	RailKeyProvider       = "provider"
	RailKeyCCBillFormName = "form_name"
	RailKeyCCBillFlexID   = "flex_id"
	// RailKeyCCBillRecurringBillingOption is CCBill's price/product/plan identity
	// (#601) — the "Recurring Billing Option" id (a zero-padded numeric, e.g.
	// "0000042836"). It is DISTINCT from the FlexForm (form_name/flex_id, the
	// hosted purchase-flow page): legacy/archived tiers carry an RBO with no
	// FlexForm, and reconciliation keys on the RBO.
	RailKeyCCBillRecurringBillingOption = "recurring_billing_option_id"
	RailKeyStripePriceID                = "price_id"
	RailKeyStripeProductID              = "product_id"
)

// GetRailConfig returns the configuration for a specific rail, or nil if not configured
func (p *Price) GetRailConfig(rail Rail) map[string]string {
	if p.Rails == nil {
		return nil
	}
	return p.Rails[string(rail)]
}

// HasRail checks if a specific rail is configured for this price
func (p *Price) HasRail(rail Rail) bool {
	return p.GetRailConfig(rail) != nil
}

// GetNMIConfigForRail returns the NMI config for a specific rail (e.g., "mobius", "acme")
// This allows support for multiple NMI-backed rails with different plan IDs
func (p *Price) GetNMIConfigForRail(railName string) (planID string, ok bool) {
	config := p.GetRailConfig(Rail(railName))
	if config == nil {
		return "", false
	}
	planID = config[RailKeyPlanID]
	return planID, planID != ""
}

// GetCCBillFlexForm returns the CCBill flexform configuration (form name + flex ID)
func (p *Price) GetCCBillFlexForm() (formName, flexID string, ok bool) {
	config := p.GetRailConfig(RailCCBill)
	if config == nil {
		return "", "", false
	}
	formName = strings.TrimSpace(config[RailKeyCCBillFormName])
	flexID = strings.TrimSpace(config[RailKeyCCBillFlexID])
	if formName == "" || flexID == "" {
		return "", "", false
	}

	return formName, flexID, true
}

// GetCCBillRecurringBillingOption returns the CCBill Recurring Billing Option id
// (the price/product/plan identity, #601), independent of the FlexForm.
func (p *Price) GetCCBillRecurringBillingOption() (rboID string, ok bool) {
	config := p.GetRailConfig(RailCCBill)
	if config == nil {
		return "", false
	}
	rboID = strings.TrimSpace(config[RailKeyCCBillRecurringBillingOption])
	return rboID, rboID != ""
}

// GetSolanaConfig returns the Solana rail configuration
func (p *Price) GetSolanaConfig() (ok bool) {
	// Solana rail just needs to be present in the map to be enabled
	return p.HasRail(RailSolana)
}

// HasTrial reports whether this price has a trial first phase (#622).
func (p *Price) HasTrial() bool { return p.TrialUnitAmount != nil && p.TrialDurationHours != nil }

// GetTrial returns the trial first phase (unit amount + length in HOURS) and
// whether one is set. A unit amount of 0 is a free trial.
func (p *Price) GetTrial() (trialUnitAmount int64, trialDurationHours int, ok bool) {
	if !p.HasTrial() {
		return 0, 0, false
	}
	return *p.TrialUnitAmount, *p.TrialDurationHours, true
}

// GetStripeConfig returns Stripe price ID
func (p *Price) GetStripeConfig() (priceID string, ok bool) {
	config := p.GetRailConfig(RailStripe)
	if config == nil {
		return "", false
	}
	priceID = strings.TrimSpace(config[RailKeyStripePriceID])
	return priceID, priceID != ""
}

// SetRailConfig sets the configuration for a specific rail
func (p *Price) SetRailConfig(rail Rail, config map[string]string) {
	if p.Rails == nil {
		p.Rails = make(map[string]map[string]string)
	}
	p.Rails[string(rail)] = config
}

// SetNMIConfig sets the NMI rail configuration using "mobius" as the key
func (p *Price) SetNMIConfig(planID, provider string) {
	provider = normalize.FirstNonEmpty(normalize.Lower(provider), string(RailMobius))
	config := map[string]string{
		RailKeyPlanID:   planID,
		RailKeyProvider: provider,
	}
	p.SetRailConfig(RailMobius, config)
}

// SetNMIConfigForRail sets the NMI config for a specific rail (e.g., "acme")
func (p *Price) SetNMIConfigForRail(railName, planID string) {
	p.SetRailConfig(Rail(railName), map[string]string{
		RailKeyPlanID: planID,
	})
}

// SetCCBillConfig sets the CCBill rail configuration
func (p *Price) SetCCBillConfig(formName, flexID string) {
	p.SetRailConfig(RailCCBill, map[string]string{
		RailKeyCCBillFormName: formName,
		RailKeyCCBillFlexID:   flexID,
	})
}

// SetSolanaConfig enables the Solana rail
func (p *Price) SetSolanaConfig() {
	p.SetRailConfig(RailSolana, map[string]string{
		"enabled": "true",
	})
}

// SetStripeConfig sets the Stripe price ID
func (p *Price) SetStripeConfig(priceID string) {
	p.SetRailConfig(RailStripe, map[string]string{
		RailKeyStripePriceID: priceID,
	})
}
