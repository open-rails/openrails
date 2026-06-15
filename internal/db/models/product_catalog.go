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
	MerchantID  uuid.UUID `json:"tenant_id"`
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
	// Unit is the currency code of the granted balance (#472). "" => "usd".
	// Unqualified = built-in currency; a future tenant/name = custom credit (#473).
	Unit   string `json:"unit,omitempty"`
	Amount int64  `json:"amount"`
	// ExpiryDays: balance expires now+N days. null/omitted => 365 default; explicit
	// 0 => never expires (#472).
	ExpiryDays *int `json:"expiry_days,omitempty"`
	// ExpiresDays is the legacy field name, still parsed for backward compat.
	ExpiresDays *int               `json:"expires_days,omitempty"`
	Cadence     CreditGrantCadence `json:"cadence,omitempty"` // once|per_renewal (default once)
}

// UnitOrDefault returns the grant's currency code, defaulting to "usd" (#472).
func (g CreditGrantSpec) UnitOrDefault() string {
	if g.Unit == "" {
		return "usd"
	}
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
	MerchantID uuid.UUID     `json:"tenant_id"`
	ProductID  uuid.UUID     `json:"product_id"`
	Status     CatalogStatus `json:"status"`
	Amount     int64         `json:"amount"`
	Currency   string        `json:"currency"`

	// Billing interval in days (nullable for one-time purchases)
	// 30 = monthly, 365 = yearly, null = one-time purchase
	BillingCycleDays *int `json:"billing_cycle_days"`

	// Processors is a JSONB map of processor name -> processor-specific configuration
	// Keys: "mobius", "ccbill", "solana", etc.
	// Values: processor-specific data (e.g., plan_id, price_id, provider)
	// Example: {"mobius": {"plan_id": "123"}, "ccbill": {"price_id": "456"}}
	Processors map[string]map[string]string `json:"processors,omitempty"`

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

// Processor config key constants (used in the Processors JSONB map)
const (
	ProcessorKeyPlanID          = "plan_id"
	ProcessorKeyProvider        = "provider"
	ProcessorKeyCCBillFormName  = "form_name"
	ProcessorKeyCCBillFlexID    = "flex_id"
	ProcessorKeyStripePriceID   = "price_id"
	ProcessorKeyStripeProductID = "product_id"
)

// GetProcessorConfig returns the configuration for a specific processor, or nil if not configured
func (p *Price) GetProcessorConfig(processor Processor) map[string]string {
	if p.Processors == nil {
		return nil
	}
	return p.Processors[string(processor)]
}

// HasProcessor checks if a specific processor is configured for this price
func (p *Price) HasProcessor(processor Processor) bool {
	return p.GetProcessorConfig(processor) != nil
}

// GetNMIConfigForProcessor returns the NMI config for a specific processor (e.g., "mobius", "acme")
// This allows support for multiple NMI-backed processors with different plan IDs
func (p *Price) GetNMIConfigForProcessor(processorName string) (planID string, ok bool) {
	config := p.GetProcessorConfig(Processor(processorName))
	if config == nil {
		return "", false
	}
	planID = config[ProcessorKeyPlanID]
	return planID, planID != ""
}

// GetCCBillFlexForm returns the CCBill flexform configuration (form name + flex ID)
func (p *Price) GetCCBillFlexForm() (formName, flexID string, ok bool) {
	config := p.GetProcessorConfig(ProcessorCCBill)
	if config == nil {
		return "", "", false
	}
	formName = strings.TrimSpace(config[ProcessorKeyCCBillFormName])
	flexID = strings.TrimSpace(config[ProcessorKeyCCBillFlexID])
	if formName == "" || flexID == "" {
		return "", "", false
	}

	return formName, flexID, true
}

// GetSolanaConfig returns the Solana processor configuration
func (p *Price) GetSolanaConfig() (ok bool) {
	// Solana processor just needs to be present in the map to be enabled
	return p.HasProcessor(ProcessorSolana)
}

// GetStripeConfig returns Stripe price ID
func (p *Price) GetStripeConfig() (priceID string, ok bool) {
	config := p.GetProcessorConfig(ProcessorStripe)
	if config == nil {
		return "", false
	}
	priceID = strings.TrimSpace(config[ProcessorKeyStripePriceID])
	return priceID, priceID != ""
}

// SetProcessorConfig sets the configuration for a specific processor
func (p *Price) SetProcessorConfig(processor Processor, config map[string]string) {
	if p.Processors == nil {
		p.Processors = make(map[string]map[string]string)
	}
	p.Processors[string(processor)] = config
}

// SetNMIConfig sets the NMI processor configuration using "mobius" as the key
func (p *Price) SetNMIConfig(planID, provider string) {
	provider = normalize.FirstNonEmpty(normalize.Lower(provider), string(ProcessorMobius))
	config := map[string]string{
		ProcessorKeyPlanID:   planID,
		ProcessorKeyProvider: provider,
	}
	p.SetProcessorConfig(ProcessorMobius, config)
}

// SetNMIConfigForProcessor sets the NMI config for a specific processor (e.g., "acme")
func (p *Price) SetNMIConfigForProcessor(processorName, planID string) {
	p.SetProcessorConfig(Processor(processorName), map[string]string{
		ProcessorKeyPlanID: planID,
	})
}

// SetCCBillConfig sets the CCBill processor configuration
func (p *Price) SetCCBillConfig(formName, flexID string) {
	p.SetProcessorConfig(ProcessorCCBill, map[string]string{
		ProcessorKeyCCBillFormName: formName,
		ProcessorKeyCCBillFlexID:   flexID,
	})
}

// SetSolanaConfig enables the Solana processor
func (p *Price) SetSolanaConfig() {
	p.SetProcessorConfig(ProcessorSolana, map[string]string{
		"enabled": "true",
	})
}

// SetStripeConfig sets the Stripe price ID
func (p *Price) SetStripeConfig(priceID string) {
	p.SetProcessorConfig(ProcessorStripe, map[string]string{
		ProcessorKeyStripePriceID: priceID,
	})
}
