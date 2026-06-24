package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// maxCatalogPageSize bounds caller-supplied pagination so a single request can
// never force an unbounded result set (DoS-resistance).
const maxCatalogPageSize = 1000

// clampCatalogPage normalises a (limit, offset) pair:
//   - limit <= 0 → 100 (default page size)
//   - limit > maxCatalogPageSize → maxCatalogPageSize (cap)
//   - offset < 0 → 0 (floor)
func clampCatalogPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 100
	}
	if limit > maxCatalogPageSize {
		limit = maxCatalogPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// GetProduct returns a product by ID.
func (s *Service) GetProduct(ctx context.Context, productID uuid.UUID) (*CatalogProduct, error) {
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if productID == uuid.Nil {
		return nil, fmt.Errorf("product_id required")
	}
	p, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, err
	}
	return productToCatalogProduct(p), nil
}

// GetProductBySlug returns a product by its slug.
func (s *Service) GetProductBySlug(ctx context.Context, slug string) (*CatalogProduct, error) {
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, fmt.Errorf("slug required")
	}
	p, err := products.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	return productToCatalogProduct(p), nil
}

// ListProductsOptions controls ListProducts filtering and pagination.
//
// Zero values mean "no filter / use defaults":
//   - ActiveOnly=false: include both active and inactive products
//   - TierGroup="": no tier_group filter
//   - Limit=0: defaults to 100
//   - Offset=0: start from the first row
type ListProductsOptions struct {
	ActiveOnly bool
	TierGroup  string
	Limit      int
	Offset     int
}

// ListProducts returns a paginated list of products with optional filters.
// Total is the unfiltered-by-pagination count.
func (s *Service) ListProducts(ctx context.Context, opts ListProductsOptions) (items []CatalogProduct, total int64, err error) {
	products, err := s.requireProductService()
	if err != nil {
		return nil, 0, err
	}
	limit, offset := clampCatalogPage(opts.Limit, opts.Offset)

	var raws []*models.Product
	// Use the simplest paginated method that exists on ProductService today.
	// TierGroup filtering is applied in-memory since there's no repo helper for it.
	if opts.ActiveOnly {
		raws, total, err = products.GetActivePaginated(ctx, limit, offset)
	} else {
		raws, total, err = products.GetAllPaginated(ctx, limit, offset)
	}
	if err != nil {
		return nil, 0, err
	}

	tierGroup := strings.TrimSpace(opts.TierGroup)
	out := make([]CatalogProduct, 0, len(raws))
	for _, p := range raws {
		if tierGroup != "" {
			if p.TierGroup == nil || !strings.EqualFold(strings.TrimSpace(*p.TierGroup), tierGroup) {
				continue
			}
		}
		out = append(out, *productToCatalogProduct(p))
	}
	return out, total, nil
}

// ActivateProduct sets status=active on a product.
func (s *Service) ActivateProduct(ctx context.Context, productID uuid.UUID) (*CatalogProduct, error) {
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if productID == uuid.Nil {
		return nil, fmt.Errorf("product_id required")
	}
	if err := products.Activate(ctx, productID); err != nil {
		return nil, err
	}
	updated, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, err
	}
	// Propagate the active flag to Stripe so re-activating an OpenRails product
	// re-activates its Stripe Product (best-effort).
	s.propagateProductActiveToStripe(ctx, productID, true)
	return productToCatalogProduct(updated), nil
}

// DeactivateProduct archives a product (status=archived). Existing subscriptions
// on its prices are grandfathered and keep openrails.
func (s *Service) DeactivateProduct(ctx context.Context, productID uuid.UUID) (*CatalogProduct, error) {
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if productID == uuid.Nil {
		return nil, fmt.Errorf("product_id required")
	}
	if err := products.Deactivate(ctx, productID); err != nil {
		return nil, err
	}
	updated, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, err
	}
	// Propagate the active flag to Stripe (archived -> Stripe active=false).
	s.propagateProductActiveToStripe(ctx, productID, false)
	return productToCatalogProduct(updated), nil
}

// GetPrice returns a price by ID.
func (s *Service) GetPrice(ctx context.Context, priceID uuid.UUID) (*CatalogPrice, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if priceID == uuid.Nil {
		return nil, fmt.Errorf("price_id required")
	}
	p, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, err
	}
	return priceToCatalogPrice(p), nil
}

// ListPricesByProduct returns all prices belonging to a product. Set activeOnly=true to filter inactive.
func (s *Service) ListPricesByProduct(ctx context.Context, productID uuid.UUID, activeOnly bool) ([]CatalogPrice, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if productID == uuid.Nil {
		return nil, fmt.Errorf("product_id required")
	}
	var raws []*models.Price
	if activeOnly {
		raws, err = prices.GetActiveByProductID(ctx, productID)
	} else {
		raws, err = prices.GetByProductID(ctx, productID)
	}
	if err != nil {
		return nil, err
	}
	out := make([]CatalogPrice, 0, len(raws))
	for _, p := range raws {
		out = append(out, *priceToCatalogPrice(p))
	}
	return out, nil
}

// ListPrices returns a paginated list of prices across all products, with filters.
func (s *Service) ListPrices(ctx context.Context, filter catalog.PriceFilter, limit, offset int) (items []CatalogPrice, total int64, err error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, 0, err
	}
	limit, offset = clampCatalogPage(limit, offset)
	raws, total, err := prices.ListPaginated(ctx, filter, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	out := make([]CatalogPrice, 0, len(raws))
	for _, p := range raws {
		out = append(out, *priceToCatalogPrice(p))
	}
	return out, total, nil
}

// ActivatePrice sets status=active on a price.
// propagatePriceActiveToStripe pushes a price's active flag to its linked Stripe
// price (best-effort), mirroring propagateProductActiveToStripe — so archiving or
// re-activating a price in OpenRails is reflected in Stripe.
func (s *Service) propagatePriceActiveToStripe(ctx context.Context, price *models.Price, active bool) {
	if s.rt == nil || s.rt.Config == nil || price == nil {
		return
	}
	var stripePriceID string
	if m := price.Rails["stripe"]; m != nil {
		stripePriceID = strings.TrimSpace(m[models.RailKeyStripePriceID])
	}
	if stripePriceID == "" {
		return
	}
	stripeSvc := &catalog.StripeCatalogService{Config: s.rt.Config, Rails: s.rt.Rails}
	a := active
	_ = stripeSvc.UpdatePrice(ctx, stripePriceID, catalog.UpdatePriceParams{Active: &a})
}

func (s *Service) ActivatePrice(ctx context.Context, priceID uuid.UUID) (*CatalogPrice, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if priceID == uuid.Nil {
		return nil, fmt.Errorf("price_id required")
	}
	if err := prices.Activate(ctx, priceID); err != nil {
		return nil, err
	}
	updated, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, err
	}
	s.propagatePriceActiveToStripe(ctx, updated, true)
	return priceToCatalogPrice(updated), nil
}

// DeactivatePrice archives a price (status=archived). Existing subscriptions on
// this price are grandfathered and keep billing; new purchases are rejected.
func (s *Service) DeactivatePrice(ctx context.Context, priceID uuid.UUID) (*CatalogPrice, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if priceID == uuid.Nil {
		return nil, fmt.Errorf("price_id required")
	}
	if err := prices.Deactivate(ctx, priceID); err != nil {
		return nil, err
	}
	updated, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, err
	}
	s.propagatePriceActiveToStripe(ctx, updated, false)
	return priceToCatalogPrice(updated), nil
}

// SetProductStatus explicitly transitions a product to draft|active|archived.
func (s *Service) SetProductStatus(ctx context.Context, productID uuid.UUID, status models.CatalogStatus) (*CatalogProduct, error) {
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if productID == uuid.Nil {
		return nil, fmt.Errorf("product_id required")
	}
	if !status.Valid() {
		return nil, fmt.Errorf("invalid status %q", status)
	}
	if err := products.SetStatus(ctx, productID, status); err != nil {
		return nil, err
	}
	updated, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, err
	}
	// active -> Stripe active=true; draft/archived -> active=false.
	s.propagateProductActiveToStripe(ctx, productID, status == models.CatalogStatusActive)
	return productToCatalogProduct(updated), nil
}

// SetPriceStatus explicitly transitions a price to draft|active|archived.
func (s *Service) SetPriceStatus(ctx context.Context, priceID uuid.UUID, status models.CatalogStatus) (*CatalogPrice, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if priceID == uuid.Nil {
		return nil, fmt.Errorf("price_id required")
	}
	if !status.Valid() {
		return nil, fmt.Errorf("invalid status %q", status)
	}
	if err := prices.SetStatus(ctx, priceID, status); err != nil {
		return nil, err
	}
	updated, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, err
	}
	s.propagatePriceActiveToStripe(ctx, updated, status == models.CatalogStatusActive)
	return priceToCatalogPrice(updated), nil
}

// VerifyPriceSync performs a live retrieve against every attached provider
// and returns a populated Providers map. Replaces issue #205's
// VerifyPriceStripeSync. Each provider's adapter does its own retrieve; the
// dispatcher merges per-provider drift / missing / configured signals into the
// uniform ProviderState surface.
func (s *Service) VerifyPriceSync(ctx context.Context, priceID uuid.UUID) (map[string]ProviderState, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if priceID == uuid.Nil {
		return nil, fmt.Errorf("price_id required")
	}
	p, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, err
	}
	if len(p.Rails) == 0 {
		return nil, nil
	}
	local := &priceVerifyContext{
		IsActive:   p.Status == models.CatalogStatusActive,
		UnitAmount: p.Amount,
		Currency:   p.Currency,
	}
	adapters := s.providerAdapters()
	out := make(map[string]ProviderState, len(p.Rails))
	for name, ids := range p.Rails {
		adapter, ok := adapters[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			// Unknown providers stay visible but uncomputed.
			out[name] = ProviderState{
				Status:     ProviderStatusLinked,
				IDs:        copyStringMap(ids),
				LookupKey:  ids[providerLookupKey],
				SyncStatus: SyncStatusUnknown,
			}
			continue
		}
		state := ProviderState{
			Status:    ProviderStatusLinked,
			IDs:       copyStringMap(ids),
			LookupKey: ids[providerLookupKey],
		}
		drift, missing, verifyErr := adapter.Verify(ctx, ids, local)
		if verifyErr != nil {
			lower := strings.ToLower(verifyErr.Error())
			if strings.Contains(lower, "stripe is not configured") {
				state.SyncStatus = SyncStatusSyncDisabled
			} else {
				state.Status = ProviderStatusError
				state.SyncStatus = SyncStatusUnknown
				state.Message = verifyErr.Error()
			}
		} else if missing {
			state.SyncStatus = SyncStatusMissing
		} else if len(drift) > 0 {
			state.SyncStatus = SyncStatusDrifted
			state.Drift = drift
		} else {
			state.SyncStatus = SyncStatusInSync
		}
		out[name] = state
	}
	return out, nil
}

// ReconcileOptions controls reconcile behavior.
//
// DryRun: compute the diff but do not mutate either side; the response carries
// the planned action + drift fields.
//
// Recreate: when the stored Stripe object 404s, create a new Stripe object
// under the same lookup_key + metadata and update the OpenRails row to point
// at it. Without Recreate, a 404 returns sync_status=missing with no action.
type ReconcileOptions struct {
	DryRun   bool
	Recreate bool
}

// ReconcileResult is the response of a Reconcile call. Replaces issue #205's
// single-provider shape with a per-provider map so the same struct works for
// any attached provider.
type ReconcileResult struct {
	// Providers carries the post-reconcile per-provider state. The dispatcher
	// fills this from a fresh Verify after any mutations land.
	Providers map[string]ProviderState `json:"providers,omitempty"`
	// Actions maps provider name -> what reconcile did (or would do, on DryRun).
	// Possible action values: "no_op", "updated_remote", "recreated_remote",
	// "would_update_remote" (dry_run), "would_recreate_remote" (dry_run),
	// "missing_no_recreate" (refused; pass recreate=true to remint),
	// "unsupported" (provider exposes no reconcile surface today).
	Actions map[string]string `json:"actions,omitempty"`
}

// ReconcilePrice walks every attached provider and re-applies OpenRails values
// to the remote when drift is detected. OpenRails is authoritative.
func (s *Service) ReconcilePrice(ctx context.Context, priceID uuid.UUID, opts ReconcileOptions) (*ReconcileResult, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if priceID == uuid.Nil {
		return nil, fmt.Errorf("price_id required")
	}
	verified, err := s.VerifyPriceSync(ctx, priceID)
	if err != nil {
		return nil, err
	}
	if len(verified) == 0 {
		return &ReconcileResult{}, nil
	}
	local, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, err
	}
	adapters := s.providerAdapters()
	actions := make(map[string]string, len(verified))
	mutated := false
	for name, state := range verified {
		actions[name] = "no_op"
		switch state.SyncStatus {
		case SyncStatusInSync, SyncStatusNeverSynced, SyncStatusSyncDisabled, SyncStatusUnknown:
			continue
		case SyncStatusMissing:
			if name != "stripe" {
				// Only Stripe supports recreate today.
				actions[name] = "missing_no_recreate"
				continue
			}
			if !opts.Recreate {
				actions[name] = "missing_no_recreate"
				continue
			}
			if opts.DryRun {
				actions[name] = "would_recreate_remote"
				continue
			}
			// Recreate path (stripe only): mint a new Stripe Price under the
			// same lookup_key + metadata, then patch the local row's
			// rails map to point at it.
			product, _, perr := s.requireCatalogServices()
			if perr != nil {
				return nil, perr
			}
			prod, perr := product.GetByID(ctx, local.ProductID)
			if perr != nil {
				return nil, perr
			}
			if _, perr := s.recreateStripePrice(ctx, prices, prod, local, priceID, state.IDs[models.RailKeyStripeProductID]); perr != nil {
				return nil, perr
			}
			actions[name] = "recreated_remote"
			mutated = true
		case SyncStatusDrifted:
			// Prices are immutable on their financial terms (amount/currency/cycle):
			// those fields are baked into the content key, so any change is a
			// different price minted upstream (create-new + archive-old), never an
			// in-place mutation or lookup_key transfer here. The only mutable field
			// reconcile propagates is the active flag — push it via the adapter.
			// Any immutable-field drift surfaced by Verify is informational; we do
			// not attempt to "fix" it by reminting.
			adapter, ok := adapters[strings.ToLower(strings.TrimSpace(name))]
			if !ok {
				actions[name] = "unsupported"
				continue
			}
			if opts.DryRun {
				actions[name] = "would_update_remote"
				continue
			}
			if s.catalogRemoteWritesDisabled() {
				actions[name] = "skipped_remote_writes_disabled"
				continue
			}
			active := local.Status == models.CatalogStatusActive
			if err := adapter.Update(ctx, state.IDs, mutableUpdate{
				IsActive: &active,
			}); err != nil {
				return nil, err
			}
			actions[name] = "updated_remote"
			mutated = true
		default:
			// Unrecognized sync status (e.g. SyncStatus("")) — leave as no_op.
			continue
		}
	}
	// Re-verify after mutation so the response carries the post-reconcile state.
	var finalStates map[string]ProviderState
	if mutated {
		finalStates, _ = s.VerifyPriceSync(ctx, priceID)
	} else {
		finalStates = verified
	}
	// Resolving a price via reconcile auto-closes any open catalog drift events
	// tied to it (issue #209). Best-effort: failures here (e.g. the
	// catalog_drift_events table not yet present) must not fail the reconcile.
	if !opts.DryRun {
		if _, derr := s.ResolveDriftForResource(ctx, models.CatalogDriftResourcePrice, priceID.String()); derr != nil {
			// swallow — drift auto-close is advisory; the next loop run reconciles it.
			_ = derr
		}
	}
	return &ReconcileResult{Providers: finalStates, Actions: actions}, nil
}

// ProductReconcileResult is the response of a ReconcileProduct call.
type ProductReconcileResult struct {
	// SyncStatus is the product's Stripe sync state after the pass
	// (in_sync / drifted / missing / sync_disabled / unknown).
	SyncStatus SyncStatus `json:"sync_status"`
	// Drift carries the field-level divergence observed (pre-reconcile on DryRun,
	// otherwise the residual after the push).
	Drift []DriftField `json:"drift,omitempty"`
	// Action is what reconcile did (or would do): "no_op", "updated_remote",
	// "would_update_remote" (dry_run), "missing" (no Stripe product to update),
	// "sync_disabled" (stripe not configured).
	Action string `json:"action"`
}

// ReconcileProduct re-applies the OpenRails product's mutable fields
// (display_name, description, active) to its Stripe Product when drift is
// detected. OpenRails is authoritative. Unlike prices, products have no row-level
// provider link — the Stripe Product id is discovered via the product's prices.
// This is the product-level analog of ReconcilePrice.
func (s *Service) ReconcileProduct(ctx context.Context, productID uuid.UUID, opts ReconcileOptions) (*ProductReconcileResult, error) {
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if productID == uuid.Nil {
		return nil, fmt.Errorf("product_id required")
	}
	local, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, err
	}
	stripeProductID := s.lookupStripeProductID(ctx, productID)
	if stripeProductID == "" {
		// No Stripe Product is associated with this OpenRails product (no price
		// has a Stripe link). Nothing to reconcile.
		return &ProductReconcileResult{SyncStatus: SyncStatusUnknown, Action: "missing"}, nil
	}

	adapter := &stripeAdapter{svc: s}
	drift, missing, configured, verifyErr := adapter.verifyStripeProduct(ctx, stripeProductID, local)
	if !configured {
		return &ProductReconcileResult{SyncStatus: SyncStatusSyncDisabled, Action: "sync_disabled"}, nil
	}
	if verifyErr != nil {
		return nil, verifyErr
	}
	if missing {
		// The Stripe Product 404'd. Product recreate is out of scope here (it
		// would orphan the prices that reference the old product id); surface it.
		return &ProductReconcileResult{SyncStatus: SyncStatusMissing, Action: "missing"}, nil
	}
	if len(drift) == 0 {
		return &ProductReconcileResult{SyncStatus: SyncStatusInSync, Action: "no_op"}, nil
	}
	if opts.DryRun {
		return &ProductReconcileResult{SyncStatus: SyncStatusDrifted, Drift: drift, Action: "would_update_remote"}, nil
	}

	// Push OpenRails values to Stripe: name, description, and the active flag.
	stripeSvc := &catalog.StripeCatalogService{Config: s.rt.Config, Rails: s.rt.Rails}
	name := strings.TrimSpace(local.DisplayName)
	desc := strings.TrimSpace(local.Description)
	active := local.IsPurchasable()
	if err := stripeSvc.UpdateProduct(ctx, stripeProductID, catalog.UpdateProductParams{
		Name:        &name,
		Description: &desc,
		Active:      &active,
	}); err != nil {
		return nil, err
	}

	// Re-verify so the residual drift (should be empty) is reflected.
	residual, _, _, _ := adapter.verifyStripeProduct(ctx, stripeProductID, local)
	syncStatus := SyncStatusInSync
	if len(residual) > 0 {
		syncStatus = SyncStatusDrifted
	}
	// Resolving via reconcile auto-closes open product drift events.
	if _, derr := s.ResolveDriftForResource(ctx, models.CatalogDriftResourceProduct, productID.String()); derr != nil {
		_ = derr
	}
	return &ProductReconcileResult{SyncStatus: syncStatus, Drift: residual, Action: "updated_remote"}, nil
}

// recreateStripePrice mints a fresh Stripe Price for the OpenRails price under
// its content lookup_key and repoints the local rails map at the new id.
// This is the missing→recreate path: the stored Stripe price 404'd, so we
// re-mint one carrying the SAME content lookup_key + metadata (derived from the
// product slug + the price's immutable money terms) and point the row at it.
//
// There is no amount-drift / transfer_lookup_key path: a price's financial
// terms are baked into its content key, so a change is a different price minted
// upstream (create-new + archive-old), never an in-place re-mint+transfer.
func (s *Service) recreateStripePrice(ctx context.Context, prices *catalog.PriceService, prod *models.Product, local *models.Price, priceID uuid.UUID, stripeProductID string) (string, error) {
	priceContentKey := openRailsPriceContentKey(prod.Slug, local.Currency, local.Amount, local.BillingCycleDays)
	stripeSvc := &catalog.StripeCatalogService{Config: s.rt.Config, Rails: s.rt.Rails}
	newPriceID, err := stripeSvc.CreatePrice(ctx, catalog.CreatePriceParams{
		StripeProductID:  stripeProductID,
		UnitAmount:       local.Amount,
		Currency:         local.Currency,
		BillingCycleDays: local.BillingCycleDays,
		LookupKey:        internalStripeLookupKey(prod.Slug, local.Currency, local.Amount, local.BillingCycleDays),
		// Content key is the idempotency key: replaying recreate for the same
		// price terms returns the same Stripe object rather than duplicating.
		IdempotencyKey: "openrails-price-" + priceContentKey,
		Metadata: map[string]string{
			catalog.StripeMetadataOpenRailsPriceKey:   priceContentKey,
			catalog.StripeMetadataOpenRailsProductKey: strings.TrimSpace(prod.Slug),
			// Informational only — not used for matching.
			catalog.StripeMetadataOpenRailsPriceID:   priceID.String(),
			catalog.StripeMetadataOpenRailsProductID: prod.ID.String(),
		},
	})
	if err != nil {
		return "", err
	}
	newRails := cloneRails(local.Rails)
	if newRails["stripe"] == nil {
		newRails["stripe"] = map[string]string{}
	}
	newRails["stripe"][models.RailKeyStripePriceID] = newPriceID
	if err := prices.UpdateRails(ctx, priceID, newRails); err != nil {
		return "", err
	}
	return newPriceID, nil
}

// cloneRails returns a shallow-deep copy of a rails map. Used by
// UpdatePrice when computing the merged provider_links result so we don't
// mutate the row's in-memory map before the DB round-trip.
func cloneRails(in map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(in))
	for k, v := range in {
		inner := make(map[string]string, len(v))
		for k2, v2 := range v {
			inner[k2] = v2
		}
		out[k] = inner
	}
	return out
}
