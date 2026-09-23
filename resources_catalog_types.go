package openrails

import "time"

type Product struct {
	ID               string          `json:"id"`
	CatalogID        string          `json:"catalog_id"`
	Key              string          `json:"key"`
	DisplayName      string          `json:"display_name"`
	Description      string          `json:"description"`
	EntitlementsSpec map[string]*int `json:"entitlements_spec,omitempty"`
	TierGroup        *string         `json:"tier_group,omitempty"`
	TierRank         int             `json:"tier_rank"`
	Archived         bool            `json:"archived"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type ProductCreateParams struct {
	// CatalogID selects a catalog for authorized merchant administrators. An
	// owner client derives its catalog from its verified subject.
	CatalogID        string          `json:"catalog_id,omitzero"`
	Key              string          `json:"key"`
	DisplayName      string          `json:"display_name"`
	Description      string          `json:"description"`
	EntitlementsSpec map[string]*int `json:"entitlements_spec,omitempty"`
	TierGroup        *string         `json:"tier_group,omitempty"`
	TierRank         int             `json:"tier_rank,omitempty"`
	// Archived creates the product retired. Supports migrating historical
	// plans that already have subscribers (no purchasable gap).
	Archived bool `json:"archived,omitempty"`
}

type ProductUpdateParams struct {
	DisplayName      *string         `json:"display_name,omitempty"`
	Description      *string         `json:"description,omitempty"`
	EntitlementsSpec map[string]*int `json:"entitlements_spec,omitempty"`
	SetEntitlements  bool            `json:"set_entitlements,omitempty"`
	TierGroup        *string         `json:"tier_group,omitempty"`
	SetTierGroup     bool            `json:"set_tier_group,omitempty"`
	TierRank         *int            `json:"tier_rank,omitempty"`
	// Archived sets the lifecycle flag. archived propagates to Stripe as
	// active=false; unarchived as active=true.
	Archived *bool `json:"archived,omitempty"`
	// SkipRailSync, when true, suppresses any propagation of this update to
	// configured external rails (Stripe etc.). The DB row is updated as usual.
	// Use sparingly — drift introduced this way will appear as sync_status="drifted"
	// on subsequent ?verify=true reads or reconcile actions.
	SkipRailSync bool `json:"skip_rail_sync,omitempty"`
}

type Price struct {
	ID string `json:"id"`
	// Key (#774) is the durable, per-merchant-unique movable-pointer handle for
	// this price's substance-version chain — the stable name to check out
	// against, reprice by, or reference in support conversations. ID stays the
	// #662 immutable substance UUID.
	Key                 string    `json:"key"`
	ProductID           string    `json:"product_id"`
	Archived            bool      `json:"archived"`
	UnitAmount          int64     `json:"unit_amount,string"`
	Currency            string    `json:"currency"`
	AccessDurationHours *int      `json:"access_duration_hours,omitempty"`
	AutoRenew           bool      `json:"auto_renew"`
	TrialUnitAmount     *int64    `json:"trial_unit_amount,omitempty,string"`
	TrialDurationHours  *int      `json:"trial_duration_hours,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`

	// Providers carries the typed per-provider attachment state for every
	// rail this price is linked to. Always populated when at least one
	// provider is attached; SyncStatus defaults to "unknown" until a
	// verify/reconcile path is invoked.
	Providers map[string]ProviderState `json:"providers,omitempty"`

	// PendingManualActions lists per-provider manual steps the operator must
	// complete to bring a pending_manual_link provider to linked status. Set
	// on the CreatePrice response and populated on GetPrice when at least one
	// provider is still pending.
	PendingManualActions []PendingAction `json:"pending_manual_actions,omitempty"`
}

type PriceCreateParams struct {
	ProductID  string `json:"product_id,omitempty"`
	ProductKey string `json:"product_key,omitempty"`
	// ProductData creates or reuses a product by key within the authorized catalog.
	// Existing product labels remain unchanged. Exactly one of ProductID,
	// ProductKey and ProductData is required. ProductData.Key creates an identity;
	// the outer Key names the price, independently of its product selector.
	ProductData *PriceCreateProductDataParams `json:"product_data,omitempty"`

	// Key (#774) is the durable, per-merchant-unique MOVABLE POINTER handle for
	// this price's substance-version chain — distinct from ID, which stays the
	// #662 immutable substance UUID. Optional: auto-defaults to
	// "<product-key>-<interval>" when omitted (see PriceIntervalLabel).
	// With ProductData, a different financial substance under the same key is
	// a conflict: use a new key for a new immutable offer. With ProductID or ProductKey,
	// declaring the SAME key with a DIFFERENT financial substance is a version
	// bump: the new/reactivated substance row becomes the key's current
	// target and the previously-current row is archived (grandfathered).
	Key string `json:"key,omitempty"`

	// A price's ROW IDENTITY IS its financial substance — the product key plus
	// these immutable money terms. There is no price slug: the content-based
	// provider keys are derived from (product_key, currency, unit_amount,
	// access duration, renewal flag, and trial terms), so they are stable across
	// DB rebuilds and a different amount is, by construction, a different price.
	// UnitAmount uses OpenRails currency precision: USD micros, not cents.
	UnitAmount int64  `json:"unit_amount,string"`
	Currency   string `json:"currency"`

	// AccessDurationHours (#622): the access window a purchase grants, in HOURS
	// (supports sub-day windows). nil = indefinite/durable; a positive value = a
	// finite window (rental, one-off, or the billing period when AutoRenew). Part
	// of price identity.
	AccessDurationHours *int `json:"access_duration_hours,omitempty"`
	// AutoRenew (#622): whether the price recharges and extends the window after
	// AccessDurationHours. Requires a finite AccessDurationHours. Part of identity.
	AutoRenew bool `json:"auto_renew"`

	// TrialUnitAmount / TrialDurationHours (#622): optional trial FIRST phase that
	// differs from the recurring terms. TrialUnitAmount 0 = free trial; both nil =
	// a flat price. Must be set together and require AutoRenew (there is a "then
	// recurring" part).
	TrialUnitAmount    *int64 `json:"trial_unit_amount,omitempty,string"`
	TrialDurationHours *int   `json:"trial_duration_hours,omitempty"`

	// Providers is the list of provider names to attach (e.g. ["stripe",
	// "ccbill", "nmi"]). Empty means "DB-only price with no external
	// links" — useful for testing or for prices that are not sold externally.
	PSPs []string `json:"psps,omitempty"`

	// PSPLinks maps PSP key -> provider-specific link/config key/value pairs.
	// Schema is per-provider:
	//   stripe : {"price_id": "price_xxx", "product_id": "prod_xxx" (optional)}
	//   ccbill : {"form_name": "...", "flex_id": "..."}
	//   nmi : {"plan_id": "..."}
	//   solana : {"token": "USD1"} or {"plan_pda": "..."}; omitted token defaults to USDC
	// Any provider with a non-empty link here is implicitly added to the
	// attach set even if absent from Providers.
	PSPLinks map[string]map[string]string `json:"psp_links,omitempty"`

	// Archived creates the price retired — migrates a historical plan in one
	// step (grandfathered subscriptions bill it; new buyers cannot).
	Archived bool `json:"archived,omitempty"`
}

type PriceUpdateParams struct {
	// PSPLinks merges per-PSP link maps into the existing psp_links map.
	// Supply only the PSPs you want to add or rotate. Each map's values are
	// validated through the matching rail adapter's Attach.
	PSPLinks map[string]map[string]string `json:"psp_links,omitempty"`

	// ReplacePSPLinks, when true, replaces the entire psp_links map rather
	// than merging — useful for clearing a PSP. When false (the default)
	// supplied entries are merged into the existing map and PSPs not
	// mentioned are left alone.
	ReplacePSPLinks bool `json:"replace_psp_links,omitempty"`

	// Archived sets the lifecycle flag. archived propagates to providers as
	// active=false; unarchived as active=true.
	Archived *bool `json:"archived,omitempty"`

	// See ProductUpdateParams.SkipRailSync.
	SkipRailSync bool `json:"skip_rail_sync,omitempty"`
}

// PriceCreateProductDataParams creates a product only when its key is absent.
// Reuse requires the same merchant and catalog. Labels are creation defaults;
// update existing product labels explicitly with Products.Update.
type PriceCreateProductDataParams struct {
	CatalogID   string `json:"catalog_id,omitempty"`
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
}
