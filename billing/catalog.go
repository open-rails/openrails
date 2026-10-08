package billing

import (
	"time"

	"github.com/open-rails/openrails/catalog"
)

// Product is something a customer buys: its named features (EntitlementsSpec),
// optional prepaid balance (CreditGrant), and its current prices. A product in a tier
// group is one tier of a plan family, ranked by TierRank. Archived products
// keep their purchases and subscribers but are not sold.
type Product struct {
	// Revision advances automatically when this product changes, without retaining product versions.
	Revision int64     `json:"revision"`
	ID       ProductID `json:"id"`
	// Key is the product's merchant-unique name.
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	// EntitlementsSpec maps each entitlement the product grants to its access
	// in hours. Null follows the purchased access terms, including subscription
	// access policy; it does not independently promise permanent ownership.
	EntitlementsSpec map[string]*int          `json:"entitlements_spec"`
	CreditGrant      *catalog.CreditGrantSpec `json:"credit_grant,omitempty"`
	TierGroup        *string                  `json:"tier_group"`
	TierRank         int                      `json:"tier_rank"`
	Archived         bool                     `json:"archived"`
	// Prices are the product's current prices; archived ones are listed with
	// ListPrices.
	Prices    []Price   `json:"prices"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateProductParams creates a product. Archived creates it retired, for a
// historical plan that still has subscribers.
type CreateProductParams struct {
	Key              string                   `json:"key"`
	DisplayName      string                   `json:"display_name"`
	Description      string                   `json:"description,omitempty"`
	EntitlementsSpec map[string]*int          `json:"entitlements_spec,omitempty"`
	CreditGrant      *catalog.CreditGrantSpec `json:"credit_grant,omitempty"`
	TierGroup        *string                  `json:"tier_group,omitempty"`
	TierRank         int                      `json:"tier_rank,omitempty"`
	Archived         bool                     `json:"archived,omitempty"`
}

// UpdateProductParams changes a product's fields: an omitted field is left
// as it is, null clears description, entitlements_spec and tier_group.
// Archiving takes the product off sale; its purchases and subscribers keep
// their access.
type UpdateProductParams struct {
	DisplayName      catalog.Field[string]                  `json:"display_name,omitzero"`
	Description      catalog.Field[string]                  `json:"description,omitzero"`
	EntitlementsSpec catalog.Field[map[string]*int]         `json:"entitlements_spec,omitzero"`
	CreditGrant      catalog.Field[catalog.CreditGrantSpec] `json:"credit_grant,omitzero"`
	TierGroup        catalog.Field[string]                  `json:"tier_group,omitzero"`
	TierRank         catalog.Field[int]                     `json:"tier_rank,omitzero"`
	Archived         catalog.Field[bool]                    `json:"archived,omitzero"`
}

// ProductListParams filters ListProducts. A nil Archived lists both live and
// archived products.
type ProductListParams struct {
	PageRequest
	Archived  *bool
	TierGroup string
}

// Price is one way to buy a product: UnitAmount (micros of Currency) for
// AccessDurationHours of one-off access (null: permanent), or one paid period
// and billing interval when AutoRenew. Dunning access is separate from paid time.
// A recurring price may start with a trial at TrialUnitAmount. A credit-only
// product delivers a credit lot instead of product ownership. A price's terms
// never change; Key names its current version, and selling on other terms
// means a new price under the same key. PSPs is the price's state on each PSP
// it is linked to.
type Price struct {
	CustomerAmount *catalog.CustomerAmount `json:"customer_amount,omitempty"`
	// Revision is assigned automatically within this product/key, starting at zero.
	Revision            int64                   `json:"revision"`
	ID                  PriceID                 `json:"id"`
	Key                 string                  `json:"key"`
	ProductID           ProductID               `json:"product_id"`
	Archived            bool                    `json:"archived"`
	UnitAmount          int64                   `json:"unit_amount,string"`
	Currency            string                  `json:"currency"`
	AccessDurationHours *int                    `json:"access_duration_hours"`
	AutoRenew           bool                    `json:"auto_renew"`
	TrialUnitAmount     *int64                  `json:"trial_unit_amount,string"`
	TrialDurationHours  *int                    `json:"trial_duration_hours"`
	PSPs                map[string]PSPLinkState `json:"psps"`
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
	ProductID      ProductID               `json:"product_id,omitzero"`
	ProductKey     string                  `json:"product_key,omitempty"`
	ProductData    *CreatePriceProduct     `json:"product_data,omitempty"`
	Key            string                  `json:"key,omitempty"`
	UnitAmount     int64                   `json:"unit_amount,string"`
	Currency       string                  `json:"currency"`
	// AccessDurationHours is the one-off access window (nil: permanent), or
	// the paid period and billing interval when AutoRenew. Grace access does
	// not extend paid coverage. Credit lot expiry is configured on the product.
	AccessDurationHours *int `json:"access_duration_hours,omitempty"`
	AutoRenew           bool `json:"auto_renew,omitempty"`
	// TrialUnitAmount and TrialDurationHours are a first period on other
	// terms (0 is a free trial); set both or neither, with AutoRenew.
	TrialUnitAmount    *int64                       `json:"trial_unit_amount,omitempty,string"`
	TrialDurationHours *int                         `json:"trial_duration_hours,omitempty"`
	PSPs               []string                     `json:"psps,omitempty"`
	PSPLinks           map[string]map[string]string `json:"psp_links,omitempty"`
	// Archived creates the price retired: subscribers keep paying it, nobody
	// new can buy it.
	Archived bool `json:"archived,omitempty"`
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
}

// GetPriceParams reads a price. Verify asks each linked PSP for its copy and
// reports any drift; it is a read, never a write.
type GetPriceParams struct {
	Verify bool
}

// PriceListParams filters ListPrices. Nil Archived and AutoRenew list both.
type PriceListParams struct {
	PageRequest
	ProductID ProductID
	Currency  string
	AutoRenew *bool
	Archived  *bool
}

// PriceKeyMovement is one point in a price key's history: from EffectiveAt
// the key named Price, or, when Archived, nothing.
type PriceKeyMovement struct {
	EffectiveAt time.Time `json:"effective_at"`
	Archived    bool      `json:"archived"`
	Price       Price     `json:"price"`
}
