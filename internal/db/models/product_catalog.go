package models

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// Product represents a product (e.g., Premium Membership)
// This represents our product catalog concept
type Product struct {
	Revision    int64     `json:"revision"`
	ID          uuid.UUID `json:"id"`
	MerchantID  uuid.UUID `json:"merchant_id"`
	Key         string    `json:"key"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`

	// Entitlements are opaque feature names granted for the purchased access window.
	Entitlements []string `json:"entitlements"`
	// RequiredEntitlements are the keys a customer must hold to order it.
	RequiredEntitlements []string                 `json:"required_entitlements"`
	CreditGrant          *catalog.CreditGrantSpec `json:"credit_grant,omitempty"`

	// Tier configuration for upgrade/downgrade relationships
	// Products in the same TierGroup are mutually exclusive - user must upgrade/downgrade between them
	// TierRank determines direction: higher rank = more premium (upgrade), lower rank = downgrade
	TierGroup *string `json:"tier_group,omitempty"`
	TierRank  int     `json:"tier_rank"`
	// Ownership is the declared rule, "" when derived.
	Ownership catalog.Ownership `json:"ownership,omitempty"`

	// Archived: retired. Not purchasable, hidden from the public catalog, but
	// existing subscriptions are grandfathered and bill indefinitely.
	Archived  bool      `json:"archived"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Prices []*Price `json:"prices,omitempty"`
}

// IsPurchasable reports whether the product can be bought by a new customer
// (shown in the public catalog).
func (p *Product) IsPurchasable() bool { return !p.Archived }

// Price represents a specific pricing option for a product
// This represents pricing options similar to Stripe's pricing model
type Price struct {
	CustomerAmount *catalog.CustomerAmount `json:"customer_amount,omitempty"`
	// Quantity makes a recurring price per seat; nil: no quantity.
	Quantity *catalog.Quantity `json:"quantity,omitempty"`
	// Revision numbers the terms within the key, from zero (the price's version).
	Revision int64 `json:"revision"`
	// KeyRevision is the price key's revision: every change to any version.
	KeyRevision int64           `json:"key_revision"`
	ID          uuid.UUID       `json:"id"`
	PublicID    billing.PriceID `json:"public_id"`
	MerchantID  uuid.UUID       `json:"merchant_id"`
	ProductID   uuid.UUID       `json:"product_id"`
	// Archived: retired. Not purchasable, but grandfathered subscriptions keep
	// billing it indefinitely.
	Archived bool `json:"archived"`
	// Amount is in currency micros (1e6 per major unit); rails convert to
	// their own minor units at charge time (moneyutil.NativeToRailMinorExact).
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`

	// Key identifies this product's price revision chain. It never changes on
	// a stored price; one non-archived revision may hold each product/key.
	Key string `json:"key"`

	// AccessDurationHours is the access window granted by each purchase, in hours.
	// With ProviderAccessDefault false, nil grants access without scheduled expiry.
	AccessDurationHours *int `json:"access_duration_hours"`

	// BillingIntervalHours is a fixed recurring cadence. Calendar provider
	// prices use BillingIntervalMonths instead; both nil means one-time.
	BillingIntervalHours *int `json:"billing_interval_hours"`

	// Trial pricing (#622): an optional FIRST phase that differs from the recurring
	// terms above. TrialUnitAmount = first-phase price (0 = free trial),
	// TrialDurationHours = first-phase length in hours. Both nil = a flat price.
	// Only valid with a recurring billing interval.
	TrialUnitAmount    *int64 `json:"trial_unit_amount,omitempty"`
	TrialDurationHours *int   `json:"trial_duration_hours,omitempty"`

	// ProviderOwned: a provider owns and runs the price; its one PSPLinks
	// entry is the provider object whose terms the row caches. Ours otherwise.
	ProviderOwned         bool              `json:"provider_owned"`
	ProviderPSPID         *uuid.UUID        `json:"provider_psp_id,omitempty"`
	ProviderSnapshot      map[string]string `json:"provider_snapshot,omitempty"`
	BillingIntervalMonths *int              `json:"billing_interval_months,omitempty"`
	ProviderAccessDefault bool              `json:"provider_access_default"`
	// SellVia are the methods one of ours sells through (SellViaCard,
	// SellViaSolana); nil is every method the merchant has.
	SellVia []string `json:"sell_via,omitempty"`

	// PSPLinks is the catalog projection of normalized price_psp_bindings: a
	// provider price's provider object, or the Solana plans published for one
	// of ours. Keys are current PSP labels; each value carries the immutable
	// psp_id.
	PSPLinks map[string]map[string]string `json:"psp_links,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Product       *Product       `json:"-"`
	Subscriptions []Subscription `json:"-"`
	Payments      []Payment      `json:"-"`
}

// IsPurchasable reports whether the price can be bought by a new customer
// (shown in the public catalog): ours, or a provider's that still sells new
// (CCBill). A legacy NMI or Stripe provider price only bills what it has.
func (p *Price) IsPurchasable() bool {
	if p.Archived {
		return false
	}
	if !p.ProviderOwned {
		return true
	}
	_, link, ok := p.ProviderLink()
	return ok && link[RailKeyRail] == string(RailCCBill) && p.IsRecurring()
}

// Methods one of our prices sells through.
const (
	SellViaCard   = "card"
	SellViaSolana = "solana"
)

// SellsVia reports whether the price sells through method: a provider price
// through its own provider only, one of ours through its sell_via.
func (p *Price) SellsVia(method string) bool {
	if p == nil || p.ProviderOwned {
		return false
	}
	return len(p.SellVia) == 0 || slices.Contains(p.SellVia, method)
}

// ProviderLink is a provider price's provider object: its PSP key and link.
func (p *Price) ProviderLink() (string, map[string]string, bool) {
	if p == nil || !p.ProviderOwned {
		return "", nil, false
	}
	if len(p.PSPLinks) == 1 {
		for key, link := range p.PSPLinks {
			return key, link, true
		}
	}
	if len(p.ProviderSnapshot) > 0 {
		return p.ProviderSnapshot["psp_key"], p.ProviderSnapshot, true
	}
	return "", nil, false
}

// ProviderBillingInterval is the cached cadence at the provider, independent
// of a declaration choosing one-off access. Old bindings use the price's cadence.
func (p *Price) ProviderBillingInterval() *int {
	_, link, ok := p.ProviderLink()
	if ok {
		if raw, named := link[RailKeyProviderBillingInterval]; named {
			if h, err := strconv.Atoi(raw); err == nil && h > 0 {
				return &h
			}
			return nil
		}
	}
	return p.BillingIntervalHours
}

// ProviderBillingMonths retains calendar recurrence independently of hourly access.
func (p *Price) ProviderBillingMonths() *int {
	_, link, ok := p.ProviderLink()
	if ok {
		if raw, named := link[RailKeyProviderBillingMonths]; named {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				return &n
			}
			return nil
		}
	}
	return p.BillingIntervalMonths
}

// IsRecurring reports whether the price has a recurring billing cadence.
func (p *Price) IsRecurring() bool {
	return p != nil && ((p.BillingIntervalHours != nil && *p.BillingIntervalHours > 0) || (p.BillingIntervalMonths != nil && *p.BillingIntervalMonths > 0))
}

// RecurringCycleHours returns a fixed recurring interval. Calendar provider
// prices retain BillingIntervalMonths and never acquire estimated hours.
func (p *Price) RecurringCycleHours() *int {
	if !p.IsRecurring() || p.BillingIntervalHours == nil {
		return nil
	}
	h := *p.BillingIntervalHours
	return &h
}

// RecurringCycleDays returns the billing period in whole days for
// provider boundaries that still require day cadences. Sub-day recurring
// windows floor to 0 days; provider adapters reject or skip those as needed.
func (p *Price) RecurringCycleDays() *int {
	hours := p.RecurringCycleHours()
	if hours == nil {
		return nil
	}
	days := *hours / 24
	return &days
}

// Link entry key constants (used in the PSPLinks JSONB map)
const (
	RailKeyProviderBillingInterval = "provider_billing_interval_hours"
	RailKeyProviderBillingMonths   = "provider_billing_interval_months"
	RailKeyPSPID                   = "psp_id"
	RailKeyPlanID                  = "plan_id"
	RailKeyProvider                = "provider"
	RailKeyCCBillFlexID            = "flex_id"
	// RailKeyCCBillRecurringBillingOption is CCBill's price/product/plan identity
	// (#601) — the "Recurring Billing Option" id (a zero-padded numeric, e.g.
	// "0000042836"). It is DISTINCT from the FlexForm (flex_id, the hosted
	// purchase-flow page): legacy/archived tiers carry an RBO with no
	// FlexForm, and reconciliation keys on the RBO.
	RailKeyCCBillRecurringBillingOption = "recurring_billing_option_id"
	RailKeyStripePriceID                = "price_id"
	RailKeyStripeProductID              = "product_id"
	// RailKeyRail is the rail (gateway) an entry's provider objects live on.
	// The catalog PSPLinks projection keys on the merchant's PSP key (e.g. "mobius") so
	// several accounts can share one rail; every entry records its rail here.
	RailKeyRail = "rail"
)

// PSPLinksOnRail returns every provider-link entry on the given rail, keyed
// by account key. Only the entry's RailKeyRail field decides membership.
func PSPLinksOnRail(rails map[string]map[string]string, rail Rail) map[string]map[string]string {
	if len(rails) == 0 {
		return nil
	}
	want := string(rail)
	var out map[string]map[string]string
	for key, cfg := range rails {
		if cfg == nil || strings.TrimSpace(cfg[RailKeyRail]) != want {
			continue
		}
		if out == nil {
			out = make(map[string]map[string]string, 1)
		}
		out[key] = cfg
	}
	return out
}

// PSPLinkForRail returns the rail's single provider-link entry, or nil when the
// rail is unlinked. Several accounts linked on one rail also yield nil (never
// guess between accounts) — enumerate PSPLinksForRail there.
func (p *Price) PSPLinkForRail(rail Rail) map[string]string {
	entries := PSPLinksOnRail(p.PSPLinks, rail)
	if len(entries) != 1 {
		return nil
	}
	for _, cfg := range entries {
		return cfg
	}
	return nil
}

// PSPLinksForRail returns every provider-link entry on the rail, keyed by
// account key.
func (p *Price) PSPLinksForRail(rail Rail) map[string]map[string]string {
	return PSPLinksOnRail(p.PSPLinks, rail)
}

// HasRail checks if a specific rail is configured for this price
func (p *Price) HasRail(rail Rail) bool {
	return len(PSPLinksOnRail(p.PSPLinks, rail)) > 0
}

// GetCCBillFlexID returns the CCBill FlexForm the price is sold through.
func (p *Price) GetCCBillFlexID() (flexID string, ok bool) {
	flexID = strings.TrimSpace(p.PSPLinkForRail(RailCCBill)[RailKeyCCBillFlexID])
	return flexID, flexID != ""
}

// GetCCBillRecurringBillingOption returns the CCBill Recurring Billing Option id
// (the price/product/plan identity, #601), independent of the FlexForm.
func (p *Price) GetCCBillRecurringBillingOption() (rboID string, ok bool) {
	config := p.PSPLinkForRail(RailCCBill)
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
	config := p.PSPLinkForRail(RailStripe)
	if config == nil {
		return "", false
	}
	priceID = strings.TrimSpace(config[RailKeyStripePriceID])
	return priceID, priceID != ""
}

// SetRailConfig sets the rail's provider-link entry under the rail-named
// account key, stamping the rail into the entry (readers match on RailKeyRail
// only). Account-named entries are written by the catalog apply, which knows
// the account key.
func (p *Price) SetRailConfig(rail Rail, config map[string]string) {
	if p.PSPLinks == nil {
		p.PSPLinks = make(map[string]map[string]string)
	}
	if config == nil {
		config = map[string]string{}
	}
	config[RailKeyRail] = string(rail)
	p.PSPLinks[string(rail)] = config
}

// SetCCBillConfig sets the CCBill rail configuration
func (p *Price) SetCCBillConfig(flexID string) {
	p.SetRailConfig(RailCCBill, map[string]string{RailKeyCCBillFlexID: flexID})
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

// ForPSP returns a price view containing only the selected account's bindings.
// The catalog row and its full binding map remain unchanged for other callers.
func (p *Price) ForPSP(pspID uuid.UUID) *Price {
	if p == nil {
		return nil
	}
	copy := *p
	copy.PSPLinks = nil
	if p.ProviderPSPID != nil && *p.ProviderPSPID != pspID {
		copy.ProviderSnapshot = nil
	}
	for key, cfg := range p.PSPLinks {
		if cfg[RailKeyPSPID] == pspID.String() {
			if copy.PSPLinks == nil {
				copy.PSPLinks = map[string]map[string]string{}
			}
			copy.PSPLinks[key] = cfg
		}
	}
	return &copy
}

// Summary is the product identity embedded in subscription and payment views.
func (p *Product) Summary() *billing.ProductSummary {
	if p == nil {
		return nil
	}
	return &billing.ProductSummary{ID: billing.ProductID(p.ID), Key: p.Key, DisplayName: p.DisplayName, Description: p.Description, TierGroup: p.TierGroup, TierRank: p.TierRank, Archived: p.Archived}
}

// View is the product as the API shows it, without its prices.
func (p *Product) View() billing.Product {
	names := p.Entitlements
	if names == nil {
		names = []string{}
	}
	required := p.RequiredEntitlements
	if required == nil {
		required = []string{}
	}
	v := billing.Product{Revision: p.Revision,
		ID: billing.ProductID(p.ID), Key: p.Key,
		DisplayName: p.DisplayName, Description: p.Description, Entitlements: names, RequiredEntitlements: required, CreditGrant: p.CreditGrant,
		TierGroup: p.TierGroup, TierRank: p.TierRank, Archived: p.Archived,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if p.Ownership != "" {
		v.Ownership = &p.Ownership
	}
	return v
}

// View is the price as the API shows it. A provider price's sync status is
// unknown until a verifying read fills it.
func (p *Price) View() billing.Price {
	out := billing.Price{Revision: p.KeyRevision, Version: p.Revision + 1,
		ID: p.PublicID, Key: p.Key, ProductID: billing.ProductID(p.ProductID), Archived: p.Archived,
		UnitAmount: p.Amount, Currency: p.Currency, AccessDurationHours: p.AccessDurationHours, BillingIntervalHours: p.BillingIntervalHours, BillingIntervalMonths: p.BillingIntervalMonths,
		TrialUnitAmount: p.TrialUnitAmount, TrialDurationHours: p.TrialDurationHours, CustomerAmount: p.CustomerAmount, Quantity: p.Quantity,
		SellVia: slices.Clone(p.SellVia), CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if p.ProviderOwned {
		// Earlier versions resolve their original provider object from the
		// immutable snapshot after the current lookup binding moves.
		provider := &billing.PriceProvider{AccessDurationDefault: p.ProviderAccessDefault, IDs: map[string]string{}, SyncStatus: billing.SyncStatusUnknown, Drift: []billing.DriftField{}}
		if key, link, ok := p.ProviderLink(); ok {
			provider.PSP, provider.Rail = key, link[RailKeyRail]
			if id, err := uuid.Parse(link[RailKeyPSPID]); err == nil {
				provider.PSPID = billing.PSPID(id)
			}
			for _, field := range []string{RailKeyPlanID, RailKeyStripePriceID, RailKeyStripeProductID, RailKeyCCBillFlexID, RailKeyCCBillRecurringBillingOption} {
				if v := strings.TrimSpace(link[field]); v != "" {
					provider.IDs[field] = v
				}
			}
		}
		out.Provider = provider
	}
	if !p.ProviderOwned {
		if pda := strings.TrimSpace(p.PSPLinkForRail(RailSolana)["plan_pda"]); pda != "" {
			out.SolanaPlan = &pda
		}
	}
	return out
}
