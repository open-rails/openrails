package models

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// Product represents a product offering (e.g., Premium Membership)
// This represents our product catalog concept
type Product struct {
	bun.BaseModel `bun:"table:billing.products,alias:prod"`

	ID          uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	Slug        string    `bun:"slug,notnull,unique" json:"slug"`
	DisplayName string    `bun:"display_name,notnull" json:"display_name"`
	Description string    `bun:"description,nullzero" json:"description"`

	// Entitlements configuration: map entitlement name -> duration days (nil or 0 means indefinite)
	EntitlementsSpec map[string]*int `bun:"entitlements_spec,type:jsonb,nullzero" json:"entitlements_spec,omitempty"`

	// Credits configuration: bundled promo credits for subscriptions
	CreditsSpec CreditsSpec `bun:"credits_spec,type:jsonb,nullzero" json:"credits_spec,omitempty"`

	// Tier configuration for upgrade/downgrade relationships
	// Products in the same TierGroup are mutually exclusive - user must upgrade/downgrade between them
	// TierRank determines direction: higher rank = more premium (upgrade), lower rank = downgrade
	TierGroup *string `bun:"tier_group,nullzero" json:"tier_group,omitempty"`
	TierRank  int     `bun:"tier_rank,notnull,default:0" json:"tier_rank"`

	IsActive  bool      `bun:"is_active,notnull,default:true" json:"is_active"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`

	// Relationships
	Prices []*Price `bun:"rel:has-many,join:id=product_id" json:"prices,omitempty"`
}

// CreditsSpec defines bundled credit grants for a product (e.g., promo credits on signup,
// or a recurring monthly stipend on renewals).
//
// Keys are credit type names (billing.credit_types.name). Amounts are stored as BIGINT in the
// credit type's base units (unit-agnostic), not USD cents.
type CreditsSpec map[string]CreditGrantSpec

// UnmarshalJSON decodes the current map-based credits_spec schema.
func (c *CreditsSpec) UnmarshalJSON(b []byte) error {
	if c == nil {
		return nil
	}
	if len(b) == 0 || string(b) == "null" {
		*c = nil
		return nil
	}

	var v2 map[string]CreditGrantSpec
	if err := json.Unmarshal(b, &v2); err != nil {
		return err
	}
	*c = v2
	return nil
}

type CreditGrantCadence string

const (
	CreditGrantCadenceOnce       CreditGrantCadence = "once"
	CreditGrantCadencePerRenewal CreditGrantCadence = "per_renewal"
)

type CreditGrantSpec struct {
	Amount      int64              `json:"amount"`
	ExpiresDays *int               `json:"expires_days,omitempty"`
	Cadence     CreditGrantCadence `json:"cadence,omitempty"` // once|per_renewal (default once)
}

// Price represents a specific pricing option for a product
// This represents pricing options similar to Stripe's pricing model
type Price struct {
	bun.BaseModel `bun:"table:billing.prices,alias:price"`

	ID          uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	ProductID   uuid.UUID `bun:"product_id,notnull" json:"product_id"`
	DisplayName string    `bun:"display_name,notnull" json:"display_name"`
	IsActive    bool      `bun:"is_active,notnull,default:true" json:"is_active"`
	Amount      int64     `bun:"amount,notnull" json:"amount"`
	Currency    string    `bun:"currency,notnull" json:"currency"`

	// Billing interval in days (nullable for one-time purchases)
	// 30 = monthly, 365 = yearly, null = one-time purchase
	BillingCycleDays *int `bun:"billing_cycle_days,nullzero" json:"billing_cycle_days"`

	// Processors is a JSONB map of processor name -> processor-specific configuration
	// Keys: "mobius", "ccbill", "solana", etc.
	// Values: processor-specific data (e.g., plan_id, price_id, provider)
	// Example: {"mobius": {"plan_id": "123"}, "ccbill": {"price_id": "456"}}
	Processors map[string]map[string]string `bun:"processors,type:jsonb,nullzero" json:"processors,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`

	// Relationships
	Product       *Product       `bun:"rel:belongs-to,join:product_id=id" json:"-"`
	Subscriptions []Subscription `bun:"rel:has-many,join:id=price_id" json:"-"`
	Payments      []Payment      `bun:"rel:has-many,join:id=price_id" json:"-"`
}

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

// GetNMIConfig returns the Mobius processor configuration.
func (p *Price) GetNMIConfig() (planID, provider string, ok bool) {
	config := p.GetProcessorConfig(ProcessorMobius)
	if config == nil {
		return "", "", false
	}
	planID = config[ProcessorKeyPlanID]
	provider = config[ProcessorKeyProvider]
	if provider == "" {
		provider = "mobius"
	}
	return planID, provider, planID != ""
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
	config := map[string]string{
		ProcessorKeyPlanID: planID,
	}
	if provider != "" && provider != "mobius" {
		config[ProcessorKeyProvider] = provider
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
