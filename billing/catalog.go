package billing

import (
	"time"

	"github.com/open-rails/openrails/catalog"
)

// Product is something a customer buys: its opaque access keys (Entitlements),
// optional prepaid balance (CreditGrant), and its current prices. A product in a tier
// group is one tier of a plan family, ranked by TierRank. Archived products
// keep their purchases and subscribers but are not sold.
type Product struct {
	// Revision advances on every change to the product, its keys or its rate
	// cards, never on a change to its prices. An edit may send it back as
	// expected_revision.
	Revision int64     `json:"revision"`
	ID       ProductID `json:"id"`
	// Key is the product's merchant-unique name.
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	// Entitlements are opaque access keys granted by the purchased price.
	// The price defines their access duration; keys have no separate duration.
	Entitlements []string `json:"entitlements"`
	// RequiredEntitlements are the keys a customer must already hold to order
	// the product; empty sells it to anyone.
	RequiredEntitlements []string                 `json:"required_entitlements"`
	CreditGrant          *catalog.CreditGrantSpec `json:"credit_grant,omitempty"`
	TierGroup            *string                  `json:"tier_group"`
	TierRank             int                      `json:"tier_rank"`
	// Ownership is the declared rule; null derives it (catalog.DeriveOwnership).
	Ownership *catalog.Ownership `json:"ownership"`
	Archived  bool               `json:"archived"`
	// Prices are the product's current prices; archived ones are listed with
	// ListPrices.
	Prices    []Price   `json:"prices"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateProductParams creates a product. Archived creates it retired, for a
// historical plan that still has subscribers.
type CreateProductParams struct {
	Key          string   `json:"key"`
	DisplayName  string   `json:"display_name"`
	Description  string   `json:"description,omitempty"`
	Entitlements []string `json:"entitlements,omitempty"`
	// RequiredEntitlements are the keys a customer must already hold to order
	// the product.
	RequiredEntitlements []string                 `json:"required_entitlements,omitempty"`
	CreditGrant          *catalog.CreditGrantSpec `json:"credit_grant,omitempty"`
	TierGroup            *string                  `json:"tier_group,omitempty"`
	TierRank             int                      `json:"tier_rank,omitempty"`
	// Ownership declares the rule; empty derives it.
	Ownership catalog.Ownership `json:"ownership,omitempty"`
	Archived  bool              `json:"archived,omitempty"`
}

// UpdateProductParams changes a product's fields: an omitted field is left
// as it is. An empty entitlements or required_entitlements list clears it;
// null is invalid. Null clears description and tier_group.
// Archiving takes the product off sale; its purchases and subscribers keep
// their access.
type UpdateProductParams struct {
	DisplayName  catalog.Field[string]   `json:"display_name,omitzero"`
	Description  catalog.Field[string]   `json:"description,omitzero"`
	Entitlements catalog.Field[[]string] `json:"entitlements,omitzero"`
	// RequiredEntitlements are the keys a customer must already hold to
	// order the product.
	RequiredEntitlements catalog.Field[[]string]                `json:"required_entitlements,omitzero"`
	CreditGrant          catalog.Field[catalog.CreditGrantSpec] `json:"credit_grant,omitzero"`
	TierGroup            catalog.Field[string]                  `json:"tier_group,omitzero"`
	TierRank             catalog.Field[int]                     `json:"tier_rank,omitzero"`
	// Ownership declares the rule; null derives it again.
	Ownership catalog.Field[catalog.Ownership] `json:"ownership,omitzero"`
	Archived  catalog.Field[bool]              `json:"archived,omitzero"`
	// ExpectedRevision refuses the edit with revision_mismatch unless the
	// product is still at this revision.
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
}

// ProductListParams filters ListProducts. A nil Archived lists both live and
// archived products. Keys lists the products with those keys, Entitlements
// those granting any of those keys now (each at most MaxBatchItems). ForSale
// true lists products some live price sells; false lists products that are
// only granted. IDs instead reads 1 to MaxBatchItems named products in one
// page, whatever their state; unknown ones are absent.
type ProductListParams struct {
	PageRequest
	IDs          []ProductID
	Keys         []string
	Archived     *bool
	TierGroup    string
	Entitlements []string
	ForSale      *bool
}

// Price is one way to buy a product: UnitAmount (micros of Currency) for
// AccessDurationHours of access (null: no scheduled expiry). BillingIntervalHours
// separately sets its recurring cadence (null: one-time charge).
// A recurring price may start with a trial at TrialUnitAmount. A credit-only
// product delivers a credit lot instead of product ownership. A price's terms
// never change; Key names its current version, and selling on other terms
// means a new price under the same key.
//
// A price is ours unless Provider is set: OpenRails vaults the card and runs
// the billing, and nothing is written to a provider's catalog. A provider
// price is owned and run by its provider (a legacy NMI plan, a Stripe price,
// a CCBill FlexForm); its terms are read from the provider, or declared for
// CCBill, and OpenRails never writes it.
type Price struct {
	CustomerAmount *catalog.CustomerAmount `json:"customer_amount,omitempty"`
	// Quantity makes a recurring price per seat: UnitAmount is one seat's, and
	// a subscription holds Quantity.Min to Quantity.Max seats. Null: the price
	// has no quantity.
	Quantity *catalog.Quantity `json:"quantity"`
	// Revision is the price key's: it advances on every change to any version
	// of the key or its provider objects. An edit may send it back as
	// expected_revision.
	Revision int64 `json:"revision"`
	// Version numbers this price's immutable terms within its key, from one.
	// It is the v1 suffix in ID; restoring archived terms keeps that version.
	Version              int64     `json:"version"`
	ID                   PriceID   `json:"id"`
	Key                  string    `json:"key"`
	ProductID            ProductID `json:"product_id"`
	Archived             bool      `json:"archived"`
	UnitAmount           int64     `json:"unit_amount,string"`
	Currency             string    `json:"currency"`
	AccessDurationHours  *int      `json:"access_duration_hours"`
	BillingIntervalHours *int      `json:"billing_interval_hours"`
	TrialUnitAmount      *int64    `json:"trial_unit_amount,string"`
	TrialDurationHours   *int      `json:"trial_duration_hours"`
	// SellVia are the methods one of ours sells through: SellViaCard (the
	// merchant's card vault and charge PSPs) and SellViaSolana. Null: every
	// method the merchant has. A provider price sells through its provider.
	SellVia []string `json:"sell_via"`
	// Provider is the provider that owns and runs a provider price; null on
	// ours.
	Provider *PriceProvider `json:"provider"`
	// SolanaPlan is the on-chain plan OpenRails published for a recurring
	// price of ours; null when it sells on no Solana plan.
	SolanaPlan *string   `json:"solana_plan"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Methods a price of ours sells through.
const (
	SellViaCard   = "card"
	SellViaSolana = "solana"
)

// PriceProvider is the PSP that owns and runs a provider price and its
// identifiers there: plan_id (NMI), price_id (Stripe), or flex_id and
// recurring_billing_option_id (CCBill). A buyer's view (BuyerPrices) keeps
// the PSP and rail only. After a verifying read, SyncStatus says whether the
// provider's terms still match the price's and Drift how they differ.
type PriceProvider struct {
	PSP        string            `json:"psp"`
	PSPID      PSPID             `json:"psp_id"`
	Rail       string            `json:"rail"`
	IDs        map[string]string `json:"ids"`
	SyncStatus SyncStatus        `json:"sync_status"`
	Drift      []DriftField      `json:"drift"`
}

// BuyerPrices are prices as a buyer sees them, for a host to put in its own
// pages: ours, and the provider prices that still sell new (CCBill), naming
// their PSP without the provider's identifiers.
func BuyerPrices(prices []Price) []Price {
	out := make([]Price, 0, len(prices))
	for _, p := range prices {
		if p.Provider != nil {
			if p.Provider.Rail != "ccbill" || p.BillingIntervalHours == nil {
				continue
			}
			p.Provider = &PriceProvider{PSP: p.Provider.PSP, PSPID: p.Provider.PSPID, Rail: p.Provider.Rail, IDs: map[string]string{}, SyncStatus: SyncStatusUnknown, Drift: []DriftField{}}
		}
		p.SolanaPlan = nil
		out = append(out, p)
	}
	return out
}

// SyncStatus is whether a provider's copy of a provider price matches
// OpenRails. Only a verifying read (GetPriceParams.Verify) or reconciliation
// knows; otherwise it is unknown.
type SyncStatus string

const (
	SyncStatusUnknown      SyncStatus = "unknown"
	SyncStatusInSync       SyncStatus = "in_sync"
	SyncStatusDrifted      SyncStatus = "drifted"
	SyncStatusMissing      SyncStatus = "missing"
	SyncStatusSyncDisabled SyncStatus = "sync_disabled"
)

// DriftField is one field on which a provider's copy differs from OpenRails.
type DriftField struct {
	Field          string `json:"field"`
	OpenRailsValue string `json:"openrails_value"`
	RemoteValue    string `json:"remote_value"`
}

// ErrPriceKeyCadenceConflict refuses a price created without a key whose
// default key is already held by a price on another cadence.
var ErrPriceKeyCadenceConflict error = newCodedError("price_key_cadence_conflict", ErrConflict)

// CreatePriceParams creates a price on the product named by exactly one of
// ProductID, ProductKey and ProductData.
//
// Key defaults to "<product key>-<cadence>": <n>h, <n>d on whole days, or
// weekly, monthly, quarterly or yearly for exactly 168, 720, 2160 or 8760
// hours; "onetime" when the price does not renew. A default key held by a
// price on another cadence is refused with ErrPriceKeyCadenceConflict.
// Creating a price under the key of a live price with other terms makes it
// the key's current version and archives the previous one. The price is ours:
// OpenRails sells it by card through the merchant's card_vault and
// charge_psps, and on Solana; nothing is written to a provider's catalog.
type CreatePriceParams struct {
	CustomerAmount *catalog.CustomerAmount `json:"customer_amount,omitempty"`
	// Quantity sells a recurring price per seat within its bounds.
	Quantity    *catalog.Quantity   `json:"quantity,omitempty"`
	ProductID   ProductID           `json:"product_id,omitzero"`
	ProductKey  string              `json:"product_key,omitempty"`
	ProductData *CreatePriceProduct `json:"product_data,omitempty"`
	Key         string              `json:"key,omitempty"`
	UnitAmount  int64               `json:"unit_amount,string"`
	// Currency is fiat or SOL; a stablecoin is refused.
	Currency string `json:"currency"`
	// AccessDurationHours is the access window (nil: no scheduled expiry),
	// independently of billing cadence. Credit lot expiry belongs to the product.
	AccessDurationHours *int `json:"access_duration_hours,omitempty"`
	// BillingIntervalHours is nil for one-time charges, or positive for recurring billing.
	BillingIntervalHours *int `json:"billing_interval_hours,omitempty"`
	// TrialUnitAmount and TrialDurationHours are a first period on other
	// terms (0 is a free trial); set both or neither, with a recurring billing interval.
	TrialUnitAmount    *int64 `json:"trial_unit_amount,omitempty,string"`
	TrialDurationHours *int   `json:"trial_duration_hours,omitempty"`
	// SellVia restricts the methods the price sells through (SellViaCard,
	// SellViaSolana); empty is every method the merchant has.
	SellVia []string `json:"sell_via,omitempty"`
	// Archived creates the price retired: subscribers keep paying it, nobody
	// new can buy it.
	Archived bool `json:"archived,omitempty"`
	// ExpectedRevision, for a new version of an existing key, refuses it with
	// revision_mismatch unless the key is still at this revision.
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
}

// CreatePriceProduct creates the price's product when no product holds Key;
// an existing product under Key is reused unchanged.
type CreatePriceProduct struct {
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
}

// CreateProviderPriceParams links a price a provider owns and runs to the
// product named by ProductID or ProductKey, under Key. PSP is the merchant's
// PSP key and names the provider's object: PlanID (an NMI recurring plan),
// PriceID (a Stripe price), or FlexID and RecurringBillingOptionID (a CCBill
// FlexForm). NMI and Stripe terms are read from the provider; CCBill, which
// has no read API, declares UnitAmount, Currency and BillingIntervalHours.
// AccessDurationHours defaults to the billing interval; OneOff is a price
// that does not renew. Nothing is created or edited at the provider.
type CreateProviderPriceParams struct {
	ProductID                ProductID `json:"product_id,omitzero"`
	ProductKey               string    `json:"product_key,omitempty"`
	Key                      string    `json:"key"`
	PSP                      string    `json:"psp"`
	PlanID                   string    `json:"plan_id,omitempty"`
	PriceID                  string    `json:"price_id,omitempty"`
	FlexID                   string    `json:"flex_id,omitempty"`
	RecurringBillingOptionID string    `json:"recurring_billing_option_id,omitempty"`
	// Currency prices an NMI plan; USD when omitted.
	Currency             string `json:"currency,omitempty"`
	UnitAmount           *int64 `json:"unit_amount,omitempty,string"`
	BillingIntervalHours *int   `json:"billing_interval_hours,omitempty"`
	AccessDurationHours  *int   `json:"access_duration_hours,omitempty"`
	TrialUnitAmount      *int64 `json:"trial_unit_amount,omitempty,string"`
	TrialDurationHours   *int   `json:"trial_duration_hours,omitempty"`
	OneOff               bool   `json:"one_off,omitempty"`
	Archived             bool   `json:"archived,omitempty"`
}

// UpdatePriceParams changes what about a price can change; omitted fields are
// left as they are. Keys and financial terms are immutable.
type UpdatePriceParams struct {
	Archived catalog.Field[bool] `json:"archived,omitzero"`
	// SellVia restricts the methods one of ours sells through; null sells
	// through every method the merchant has. It is not a term: the price
	// keeps its version.
	SellVia catalog.Field[[]string] `json:"sell_via,omitzero"`
	// ExpectedRevision refuses the edit with revision_mismatch unless the
	// price key is still at this revision.
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
}

// GetPriceParams reads a price. Verify asks a provider price's provider for
// its terms and reports any drift; it is a read, never a write.
type GetPriceParams struct {
	Verify bool
}

// PriceListParams filters ListPrices. Nil Archived and Recurring list both.
// ProductKey and Key select by key: a key's current price is the one not
// archived, its earlier versions the archived ones. IDs instead reads 1 to
// MaxBatchItems named prices in one page, whatever their state; unknown ones
// are absent.
type PriceListParams struct {
	PageRequest
	IDs        []PriceID
	ProductID  ProductID
	ProductKey string
	Key        string
	Currency   string
	Recurring  *bool
	Archived   *bool
}

// PriceKeyMovement is one point in a price key's history: from EffectiveAt
// the key named Price, or, when Archived, nothing.
type PriceKeyMovement struct {
	EffectiveAt time.Time `json:"effective_at"`
	Archived    bool      `json:"archived"`
	Price       Price     `json:"price"`
}
