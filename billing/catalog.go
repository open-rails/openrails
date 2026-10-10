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
	Entitlements []string                 `json:"entitlements"`
	CreditGrant  *catalog.CreditGrantSpec `json:"credit_grant,omitempty"`
	TierGroup    *string                  `json:"tier_group"`
	TierRank     int                      `json:"tier_rank"`
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
	Key          string                   `json:"key"`
	DisplayName  string                   `json:"display_name"`
	Description  string                   `json:"description,omitempty"`
	Entitlements []string                 `json:"entitlements,omitempty"`
	CreditGrant  *catalog.CreditGrantSpec `json:"credit_grant,omitempty"`
	TierGroup    *string                  `json:"tier_group,omitempty"`
	TierRank     int                      `json:"tier_rank,omitempty"`
	// Ownership declares the rule; empty derives it.
	Ownership catalog.Ownership `json:"ownership,omitempty"`
	Archived  bool              `json:"archived,omitempty"`
}

// UpdateProductParams changes a product's fields: an omitted field is left
// as it is. An empty entitlements list clears it; null is invalid. Null
// clears description and tier_group.
// Archiving takes the product off sale; its purchases and subscribers keep
// their access.
type UpdateProductParams struct {
	DisplayName  catalog.Field[string]                  `json:"display_name,omitzero"`
	Description  catalog.Field[string]                  `json:"description,omitzero"`
	Entitlements catalog.Field[[]string]                `json:"entitlements,omitzero"`
	CreditGrant  catalog.Field[catalog.CreditGrantSpec] `json:"credit_grant,omitzero"`
	TierGroup    catalog.Field[string]                  `json:"tier_group,omitzero"`
	TierRank     catalog.Field[int]                     `json:"tier_rank,omitzero"`
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

// OfferListParams asks what a customer may buy: the products on sale granting
// any of Entitlements, or named by Keys, or both when both are given (at
// least one is required, each at most MaxBatchItems), each with its live
// prices. It is the host backend's read; the public catalog takes keys only.
type OfferListParams struct {
	PageRequest
	Entitlements []string
	Keys         []string
}

// Price is one way to buy a product: UnitAmount (micros of Currency) for
// AccessDurationHours of access (null: no scheduled expiry). BillingIntervalHours
// separately sets its recurring cadence (null: one-time charge).
// A recurring price may start with a trial at TrialUnitAmount. A credit-only
// product delivers a credit lot instead of product ownership. A price's terms
// never change; Key names its current version, and selling on other terms
// means a new price under the same key. PSPs is the price's state on each PSP
// it is linked to.
type Price struct {
	CustomerAmount *catalog.CustomerAmount `json:"customer_amount,omitempty"`
	// Quantity makes a recurring price per seat: UnitAmount is one seat's, and
	// a subscription holds Quantity.Min to Quantity.Max seats. Null: the price
	// has no quantity.
	Quantity *catalog.Quantity `json:"quantity"`
	// Revision is the price key's: it advances on every change to any version
	// of the key or its PSP links. An edit may send it back as
	// expected_revision.
	Revision int64 `json:"revision"`
	// Version numbers this price's terms within its key, from zero.
	Version              int64                   `json:"version"`
	ID                   PriceID                 `json:"id"`
	Key                  string                  `json:"key"`
	ProductID            ProductID               `json:"product_id"`
	Archived             bool                    `json:"archived"`
	UnitAmount           int64                   `json:"unit_amount,string"`
	Currency             string                  `json:"currency"`
	AccessDurationHours  *int                    `json:"access_duration_hours"`
	BillingIntervalHours *int                    `json:"billing_interval_hours"`
	TrialUnitAmount      *int64                  `json:"trial_unit_amount,string"`
	TrialDurationHours   *int                    `json:"trial_duration_hours"`
	PSPs                 map[string]PSPLinkState `json:"psps"`
	// PendingManualActions are the steps an operator still has to take
	// before each PSP whose link is pending_manual_link can sell the price.
	PendingManualActions []PendingAction `json:"pending_manual_actions"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

// PSPLinkState is a price's state on one PSP: the PSP's own identifiers for
// it and, after a verifying read, whether the PSP still matches. Public
// catalog routes answer the status only.
type PSPLinkState struct {
	Status     PSPLinkStatus     `json:"status"`
	IDs        map[string]string `json:"ids"`
	LookupKey  string            `json:"lookup_key"`
	SyncStatus SyncStatus        `json:"sync_status"`
	Drift      []DriftField      `json:"drift"`
	Message    string            `json:"message"`
}

// PSPLinkStatus is whether a price is linked to a PSP.
type PSPLinkStatus string

const (
	PSPLinkLinked            PSPLinkStatus = "linked"
	PSPLinkPendingManualLink PSPLinkStatus = "pending_manual_link"
	PSPLinkSyncDisabled      PSPLinkStatus = "sync_disabled"
	PSPLinkError             PSPLinkStatus = "error"
)

// SyncStatus is whether a PSP's copy of a price matches OpenRails. Only a
// verifying read (GetPriceParams.Verify) or reconciliation knows; otherwise
// it is unknown.
type SyncStatus string

const (
	SyncStatusUnknown      SyncStatus = "unknown"
	SyncStatusInSync       SyncStatus = "in_sync"
	SyncStatusDrifted      SyncStatus = "drifted"
	SyncStatusMissing      SyncStatus = "missing"
	SyncStatusNeverSynced  SyncStatus = "never_synced"
	SyncStatusSyncDisabled SyncStatus = "sync_disabled"
)

// DriftField is one field on which a PSP's copy differs from OpenRails.
type DriftField struct {
	Field          string `json:"field"`
	OpenRailsValue string `json:"openrails_value"`
	RemoteValue    string `json:"remote_value"`
}

// PendingAction is a manual step an operator takes on a PSP so it can sell a
// price; PatchRequired is the psp_links patch to send once it is done.
type PendingAction struct {
	PSP           string                                  `json:"psp"`
	Action        string                                  `json:"action"`
	Hint          string                                  `json:"hint"`
	PatchRequired map[string]map[string]map[string]string `json:"patch_required"`
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
// the key's current version and archives the previous one.
//
// PSPs lists the PSP keys the price is sold on; PSPLinks supplies a PSP's own
// identifiers for it, per rail: stripe {price_id, product_id} or
// {lookup_key}; nmi {plan_id}; ccbill {form_name, flex_id}; solana {token} or
// {plan_pda}. A PSP named in PSPLinks is sold on too.
type CreatePriceParams struct {
	CustomerAmount *catalog.CustomerAmount `json:"customer_amount,omitempty"`
	// Quantity sells a recurring price per seat within its bounds.
	Quantity    *catalog.Quantity   `json:"quantity,omitempty"`
	ProductID   ProductID           `json:"product_id,omitzero"`
	ProductKey  string              `json:"product_key,omitempty"`
	ProductData *CreatePriceProduct `json:"product_data,omitempty"`
	Key         string              `json:"key,omitempty"`
	UnitAmount  int64               `json:"unit_amount,string"`
	Currency    string              `json:"currency"`
	// AccessDurationHours is the access window (nil: no scheduled expiry),
	// independently of billing cadence. Credit lot expiry belongs to the product.
	AccessDurationHours *int `json:"access_duration_hours,omitempty"`
	// BillingIntervalHours is nil for one-time charges, or positive for recurring billing.
	BillingIntervalHours *int `json:"billing_interval_hours,omitempty"`
	// TrialUnitAmount and TrialDurationHours are a first period on other
	// terms (0 is a free trial); set both or neither, with a recurring billing interval.
	TrialUnitAmount    *int64                       `json:"trial_unit_amount,omitempty,string"`
	TrialDurationHours *int                         `json:"trial_duration_hours,omitempty"`
	PSPs               []string                     `json:"psps,omitempty"`
	PSPLinks           map[string]map[string]string `json:"psp_links,omitempty"`
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

// UpdatePriceParams changes what about a price can change; omitted fields are
// left as they are. Keys and financial terms are immutable.
// PSPLinks merges into the price's links: a PSP set to
// null is unlinked.
type UpdatePriceParams struct {
	Archived catalog.Field[bool]                         `json:"archived,omitzero"`
	PSPLinks map[string]catalog.Field[map[string]string] `json:"psp_links,omitempty"`
	// ExpectedRevision refuses the edit with revision_mismatch unless the
	// price key is still at this revision.
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
}

// GetPriceParams reads a price. Verify asks each linked PSP for its copy and
// reports any drift; it is a read, never a write.
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
