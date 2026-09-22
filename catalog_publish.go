package openrails

import (
	"context"
	"github.com/open-rails/openrails/pkg/catalog"
	"net/http"
)

// CatalogPublishRequest declares one complete desired catalog. No mutation
// flags means plan-only; the three explicit mutation classes compose.
type CatalogPublishRequest struct {
	Catalog   catalog.Manifest `json:"catalog"`
	Insert    bool             `json:"insert,omitempty"`
	Overwrite bool             `json:"overwrite,omitempty"`
	Prune     bool             `json:"prune,omitempty"`
}

// CatalogPublishResponse contains the proposed changes and, after mutation,
// the applied counts. Result is nil for a read-only plan.
type CatalogPublishResponse struct {
	Plan   *CatalogPlan        `json:"plan"`
	Result *CatalogApplyResult `json:"result,omitempty"`
}

// CatalogProductAction is the per-product change a plan records.
type CatalogProductAction string

const (
	CatalogProductCreate    CatalogProductAction = "create"
	CatalogProductUpdate    CatalogProductAction = "update"
	CatalogProductUnchanged CatalogProductAction = "unchanged"
	CatalogProductArchive   CatalogProductAction = "archive" // active in OpenRails, removed from manifest
)

// CatalogPriceAction is the per-price change a plan records.
type CatalogPriceAction string

const (
	CatalogPriceCreate    CatalogPriceAction = "create"
	CatalogPriceActivate  CatalogPriceAction = "activate"
	CatalogPriceArchive   CatalogPriceAction = "archive"
	CatalogPriceUnchanged CatalogPriceAction = "unchanged"
)

// CatalogPlan describes the difference between the declaration and stored catalog.
// MetersChanged and RateCardsChanged identify changes to those billing definitions,
// including definitions omitted from the declaration. Mutation flags decide which
// additions, edits and removals are applied.
type CatalogPlan struct {
	MetersChanged    bool               `json:"meters_changed"`
	RateCardsChanged bool               `json:"rate_cards_changed"`
	Groups           []CatalogGroupPlan `json:"groups"`
}

// CatalogGroupPlan is the diff for one tier group.
type CatalogGroupPlan struct {
	Key      string               `json:"key"`
	Products []CatalogProductPlan `json:"products"`
	// RemovedProducts are active OpenRails products in this tier group not
	// declared in the manifest; they are archived (deactivated) on apply.
	RemovedProducts []CatalogProduct `json:"removed_products,omitempty"`
}

// CatalogProductPlan is the diff for one product plus its price set.
type CatalogProductPlan struct {
	Key    string               `json:"key"`
	Action CatalogProductAction `json:"action"`

	// CreateReq / UpdateReq / UpdateID are prepared for apply.
	CreateReq CreateProductRequest `json:"create_req,omitempty"`
	UpdateReq UpdateProductRequest `json:"update_req,omitempty"`
	UpdateID  ProductID            `json:"update_id,omitzero"`

	Prices []CatalogPricePlan `json:"prices,omitempty"`
}

// CatalogPricePlan is the diff for one price (identity = financial substance).
type CatalogPricePlan struct {
	Label  string             `json:"label"`
	Action CatalogPriceAction `json:"action"`

	// ExistingID is the matched OpenRails price (zero when creating).
	ExistingID PriceID            `json:"existing_id,omitzero"`
	CreateReq  CreatePriceRequest `json:"create_req,omitempty"`

	// Key (#774) is this declared price's resolved key (explicit or
	// auto-defaulted). For Action==CatalogPriceCreate it rides CreateReq.Key; for a
	// MATCHED price (Unchanged/Activate/Archive) it is set ONLY when it
	// differs from the matched row's current key — signaling apply must
	// relabel (a plain key rename, no substance change) via SetPriceKey.
	Key string `json:"key,omitempty"`

	// PSPLinks is set ONLY for a MATCHED price whose manifest declares a
	// psp_links entry the stored link does not already satisfy (a key missing
	// or holding a different value) — e.g. `solana: {token: DUSD}` against a
	// row still bound to a USDC plan. Apply merges exactly these entries via
	// UpdatePrice, whose rail adapters validate/publish the new link, so a
	// link rotation is a manifest edit like any other change instead of a
	// blocked admin PATCH. Once stored, the same declaration reads as
	// satisfied and the plan is quiet again.
	PSPLinks map[string]map[string]string `json:"psp_links,omitempty"`
}

// CatalogApplyResult summarizes what an Apply run did, including any per-provider
// manual actions surfaced by CreatePrice (e.g. CCBill, an unconfigured Solana
// plan) that the operator must complete out-of-band.
type CatalogApplyResult struct {
	ProductsCreated  int                    `json:"products_created"`
	ProductsUpdated  int                    `json:"products_updated"`
	ProductsArchived int                    `json:"products_archived"`
	PricesCreated    int                    `json:"prices_created"`
	PricesActivated  int                    `json:"prices_activated"`
	PricesArchived   int                    `json:"prices_archived"`
	PricesRelinked   int                    `json:"prices_relinked"`
	PendingActions   []CatalogPendingAction `json:"pending_actions,omitempty"`
}

// CatalogPendingAction pairs a returned service.PendingAction with the price it
// belongs to, so the apply output can tell the operator which price needs a
// manual link.
type CatalogPendingAction struct {
	ProductKey string        `json:"product_key"`
	PriceLabel string        `json:"price_label"`
	Action     PendingAction `json:"action"`
}

// HasChanges reports whether the plan would mutate anything.
func (plan *CatalogPlan) HasChanges() bool {
	if plan.MetersChanged || plan.RateCardsChanged {
		return true
	}
	for gi := range plan.Groups {
		gp := &plan.Groups[gi]
		if len(gp.RemovedProducts) > 0 {
			return true
		}
		for pi := range gp.Products {
			pp := &gp.Products[pi]
			if pp.Action != CatalogProductUnchanged {
				return true
			}
			for _, price := range pp.Prices {
				if price.Action != CatalogPriceUnchanged || price.Key != "" || len(price.PSPLinks) > 0 {
					return true
				}
			}
		}
	}
	return false
}

// PublishCatalog plans a complete declaration and applies the requested mutation
// classes. Insert adds absent entries; Overwrite edits existing entries; Prune
// removes omitted meters/rate cards and archives omitted catalog entries. With
// no flags it only returns the plan. Customer-specific rate-card overrides are
// never replaced by a merchant declaration. Provider failures or concurrent
// changes can leave partial progress; a subsequent publish computes a new plan.
func (c *Client) PublishCatalog(ctx context.Context, request CatalogPublishRequest, requestOptions ...RequestOption) (*CatalogPublishResponse, error) {
	var out CatalogPublishResponse
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/catalog/publish", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
