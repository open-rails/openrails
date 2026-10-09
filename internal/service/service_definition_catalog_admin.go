package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// GetProduct returns a product by ID.
func (s *Service) GetProduct(ctx context.Context, id billing.ProductID) (*billing.Product, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("product_id required")
	}
	productID := id.UUID()
	p, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, productLookup(err)
	}
	return productToCatalogProduct(p), nil
}

// GetProductByKey returns a product by its key.
func (s *Service) GetProductByKey(ctx context.Context, key string) (*billing.Product, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, apperr.Invalidf("key required")
	}
	p, err := products.GetByKey(ctx, key)
	if err != nil {
		return nil, productLookup(err)
	}
	return productToCatalogProduct(p), nil
}

// ListProducts returns one page of products, newest first. Prices are not
// loaded; HydratePrices adds them.
func (s *Service) ListProducts(ctx context.Context, params billing.ProductListParams) (billing.ListPage[billing.Product], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.ListPage[billing.Product]{}, err
	}
	defer release()
	products, err := s.requireProductService()
	if err != nil {
		return billing.ListPage[billing.Product]{}, err
	}
	filter := catalog.ProductFilter{IDs: uuidutil.Of(params.IDs), Archived: params.Archived, TierGroup: params.TierGroup, Entitlement: params.Entitlement, ForSale: params.ForSale}
	page, err := products.List(ctx, filter, params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.Product]{}, err
	}
	return pagination.Map(page, func(p *models.Product) billing.Product { return *productToCatalogProduct(p) }), nil
}

// HydratePrices sets each product's Prices to its current prices.
func (s *Service) HydratePrices(ctx context.Context, products []billing.Product) error {
	if len(products) == 0 {
		return nil
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return err
	}
	defer release()
	prices, err := s.requirePriceService()
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, len(products))
	for i, p := range products {
		ids[i] = p.ID.UUID()
	}
	current, err := prices.CurrentByProducts(ctx, ids)
	if err != nil {
		return err
	}
	for i := range products {
		products[i].Prices = make([]billing.Price, 0, len(current[ids[i]]))
		for _, p := range current[ids[i]] {
			products[i].Prices = append(products[i].Prices, *priceToCatalogPrice(p))
		}
	}
	return nil
}

// ActivateProduct sets status=active on a product.
func (s *Service) ActivateProduct(ctx context.Context, id billing.ProductID) (*billing.Product, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Product, error) {
		return scoped.activateProduct(ctx, id)
	})
}

func (s *Service) activateProduct(ctx context.Context, id billing.ProductID) (*billing.Product, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("product_id required")
	}
	productID := id.UUID()
	current, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, productLookup(err)
	}
	if err := s.validateProductCreditUpdate(ctx, id, current.CreditGrant); err != nil {
		return nil, err
	}
	if err := products.Activate(ctx, productID); err != nil {
		return nil, productLookup(err)
	}
	updated, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, productLookup(err)
	}
	// Propagate the active flag to Stripe so re-activating an OpenRails product
	// re-activates its Stripe Product (best-effort).
	s.propagateProductActiveToStripe(ctx, productID, true)
	return productToCatalogProduct(updated), nil
}

// DeactivateProduct archives a product. Existing subscriptions on its prices
// are grandfathered and keep billing.
func (s *Service) DeactivateProduct(ctx context.Context, id billing.ProductID) (*billing.Product, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Product, error) {
		return scoped.deactivateProduct(ctx, id)
	})
}

func (s *Service) deactivateProduct(ctx context.Context, id billing.ProductID) (*billing.Product, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("product_id required")
	}
	productID := id.UUID()
	if err := products.Deactivate(ctx, productID); err != nil {
		return nil, productLookup(err)
	}
	updated, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, productLookup(err)
	}
	// Propagate the active flag to Stripe (archived -> Stripe active=false).
	s.propagateProductActiveToStripe(ctx, productID, false)
	return productToCatalogProduct(updated), nil
}

// GetPrice returns a price by ID.
func (s *Service) GetPrice(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("price_id required")
	}
	priceID := id.UUID()
	p, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, priceLookup(err)
	}
	return priceToCatalogPrice(p), nil
}

// ListPricesByProduct returns all prices belonging to a product. Set activeOnly=true to filter inactive.
func (s *Service) ListPricesByProduct(ctx context.Context, id billing.ProductID, activeOnly bool) ([]billing.Price, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("product_id required")
	}
	productID := id.UUID()
	var raws []*models.Price
	if activeOnly {
		raws, err = prices.GetActiveByProductID(ctx, productID)
	} else {
		raws, err = prices.GetByProductID(ctx, productID)
	}
	if err != nil {
		return nil, err
	}
	out := make([]billing.Price, 0, len(raws))
	for _, p := range raws {
		out = append(out, *priceToCatalogPrice(p))
	}
	return out, nil
}

// ListPrices returns one page of prices, newest first.
func (s *Service) ListPrices(ctx context.Context, params billing.PriceListParams) (billing.ListPage[billing.Price], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.ListPage[billing.Price]{}, err
	}
	defer release()
	prices, err := s.requirePriceService()
	if err != nil {
		return billing.ListPage[billing.Price]{}, err
	}
	filter := catalog.PriceFilter{IDs: uuidutil.Of(params.IDs), Archived: params.Archived, Currency: moneyutil.NormalizeCurrency(params.Currency), Recurring: params.Recurring}
	if !params.ProductID.IsZero() {
		id := params.ProductID.UUID()
		filter.ProductID = &id
	}
	page, err := prices.List(ctx, filter, params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.Price]{}, err
	}
	return pagination.Map(page, func(p *models.Price) billing.Price { return *priceToCatalogPrice(p) }), nil
}

// propagatePriceActiveToStripe pushes a price's active flag to its linked Stripe
// price (best-effort), mirroring propagateProductActiveToStripe — so archiving or
// re-activating a price in OpenRails is reflected in Stripe.
func (s *Service) propagatePriceActiveToStripe(ctx context.Context, price *models.Price, active bool) {
	s.catalogAfterCommit(ctx, func(ctx context.Context, committed *Service) {
		committed.propagatePriceActiveToStripeCommitted(ctx, price, active)
	})
}

func (s *Service) propagatePriceActiveToStripeCommitted(ctx context.Context, price *models.Price, active bool) {
	if s.localCatalogOnly || s.rt == nil || s.rt.Config == nil || price == nil {
		return
	}
	var stripePriceID string
	if m := price.PSPLinkForRail(models.RailStripe); m != nil {
		stripePriceID = strings.TrimSpace(m[models.RailKeyStripePriceID])
	}
	if stripePriceID == "" {
		return
	}
	stripeSvc := &catalog.StripeCatalogService{StripeClients: s.rt.StripeClients, Config: s.rt.Config, Rails: s.rt.RailConfigs}
	a := active
	_ = stripeSvc.UpdatePrice(ctx, stripePriceID, catalog.UpdatePriceParams{Active: &a})
}

// ActivatePrice un-archives a price. #774: for a price that was ARCHIVED, this
// is exactly the flip-flop REACTIVATION path — re-declaring a previously-seen
// substance (matched by matchPrice against the #662 deterministic id) finds
// this archived row rather than minting a new one. Its key is already set
// from creation, so activation repoints that key here: whatever OTHER row
// currently holds it is archived first (the partial unique index on
// (merchant_id, key) WHERE NOT archived allows only one live holder), then
// this row is un-archived, then one pointer-movement log entry records the
// move. Activating an already-active row is a no-op (no movement logged).
func (s *Service) ActivatePrice(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Price, error) {
		return scoped.activatePrice(ctx, id)
	})
}

func (s *Service) activatePrice(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("price_id required")
	}
	priceID := id.UUID()
	current, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, priceLookup(err)
	}
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	product, err := products.GetByID(ctx, current.ProductID)
	if err != nil {
		return nil, productLookup(err)
	}
	if err := validateCreditPrice(product.CreditGrant, billing.CreatePriceParams{Currency: current.Currency, UnitAmount: current.Amount, BillingIntervalHours: current.BillingIntervalHours, CustomerAmount: current.CustomerAmount}); err != nil {
		return nil, err
	}
	wasArchived := current.Archived
	if wasArchived {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		displaced, dErr := prices.GetCurrentByKey(ctx, tid.UUID(), current.ProductID, current.Key)
		if dErr != nil && !errors.Is(dErr, pgx.ErrNoRows) {
			return nil, fmt.Errorf("resolve current holder of price key %q: %w", current.Key, dErr)
		}
		if displaced != nil && displaced.ID != priceID {
			if err := prices.SetArchived(ctx, displaced.ID, true); err != nil {
				return nil, fmt.Errorf("archive displaced price %s for key %q: %w", displaced.ID, current.Key, err)
			}
		}
	}
	if err := prices.Activate(ctx, priceID); err != nil {
		return nil, priceLookup(err)
	}
	updated, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, priceLookup(err)
	}
	if wasArchived {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		if err := prices.RecordAuthoredKeyMovement(ctx, tid.UUID(), priceID, updated.Key); err != nil {
			return nil, fmt.Errorf("record key movement for %q -> %s: %w", updated.Key, priceID, err)
		}
	}
	s.propagatePriceActiveToStripe(ctx, updated, true)
	return priceToCatalogPrice(updated), nil
}

// DeactivatePrice archives a price. Existing subscriptions on this price are
// grandfathered and keep billing; new purchases are rejected.
func (s *Service) DeactivatePrice(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Price, error) {
		return scoped.deactivatePrice(ctx, id)
	})
}

func (s *Service) deactivatePrice(ctx context.Context, id billing.PriceID) (*billing.Price, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("price_id required")
	}
	priceID := id.UUID()
	if err := prices.Deactivate(ctx, priceID); err != nil {
		return nil, priceLookup(err)
	}
	updated, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, priceLookup(err)
	}
	s.propagatePriceActiveToStripe(ctx, updated, false)
	return priceToCatalogPrice(updated), nil
}

// VerifyPriceSync performs a live retrieve against every attached provider
// and returns a populated Providers map. Replaces issue #205's
// VerifyPriceStripeSync. Each provider's adapter does its own retrieve; the
// dispatcher merges per-provider drift / missing / configured signals into the
// uniform billing.PSPLinkState surface.
func (s *Service) VerifyPriceSync(ctx context.Context, priceID uuid.UUID) (map[string]billing.PSPLinkState, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if priceID == uuid.Nil {
		return nil, apperr.Invalidf("price_id required")
	}
	p, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, priceLookup(err)
	}
	if len(p.PSPLinks) == 0 {
		return nil, nil
	}
	local := &priceVerifyContext{
		IsActive:   !p.Archived,
		UnitAmount: p.Amount,
		Currency:   p.Currency,
	}
	adapters := s.providerAdapters()
	out := make(map[string]billing.PSPLinkState, len(p.PSPLinks))
	for name, ids := range p.PSPLinks {
		// Entries are account-keyed; the rail lives in the entry.
		adapter, ok := adapters[strings.ToLower(strings.TrimSpace(ids[models.RailKeyRail]))]
		if !ok {
			// Unknown providers stay visible but uncomputed.
			out[name] = billing.PSPLinkState{
				Status:     billing.PSPLinkLinked,
				IDs:        copyStringMap(ids),
				LookupKey:  ids[providerLookupKey],
				SyncStatus: billing.SyncStatusUnknown,
			}
			continue
		}
		state := billing.PSPLinkState{
			Status:    billing.PSPLinkLinked,
			IDs:       copyStringMap(ids),
			LookupKey: ids[providerLookupKey],
		}
		// Read exactly the account this link is bound to.
		verifyCtx := ctx
		if pspID, perr := uuid.Parse(ids[models.RailKeyPSPID]); perr == nil {
			verifyCtx = db.WithPSPID(ctx, pspID)
		}
		drift, missing, verifyErr := adapter.Verify(verifyCtx, ids, local)
		if verifyErr != nil {
			if errors.Is(verifyErr, errProviderNotArmed) {
				state.SyncStatus = billing.SyncStatusSyncDisabled
			} else {
				state.Status = billing.PSPLinkError
				state.SyncStatus = billing.SyncStatusUnknown
				state.Message = verifyErr.Error()
			}
		} else if missing {
			state.SyncStatus = billing.SyncStatusMissing
		} else if len(drift) > 0 {
			state.SyncStatus = billing.SyncStatusDrifted
			state.Drift = drift
		} else {
			state.SyncStatus = billing.SyncStatusInSync
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
	Providers map[string]billing.PSPLinkState `json:"providers,omitempty"`
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
	if !opts.DryRun {
		if err := s.checkCatalogWritePolicy(ctx); err != nil {
			return nil, err
		}
	}
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if priceID == uuid.Nil {
		return nil, apperr.Invalidf("price_id required")
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
		return nil, priceLookup(err)
	}
	adapters := s.providerAdapters()
	actions := make(map[string]string, len(verified))
	mutated := false
	for name, state := range verified {
		actions[name] = "no_op"
		switch state.SyncStatus {
		case billing.SyncStatusInSync, billing.SyncStatusNeverSynced, billing.SyncStatusSyncDisabled, billing.SyncStatusUnknown:
			continue
		case billing.SyncStatusMissing:
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
		case billing.SyncStatusDrifted:
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
			active := !local.Archived
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
	var finalStates map[string]billing.PSPLinkState
	if mutated {
		finalStates, _ = s.VerifyPriceSync(ctx, priceID)
	} else {
		finalStates = verified
	}
	// Close drift only for accounts whose post-reconcile read proved the price in
	// sync. Unknown, disabled, missing or drifted accounts keep their findings.
	if !opts.DryRun {
		for _, state := range finalStates {
			if state.SyncStatus != billing.SyncStatusInSync {
				continue
			}
			pspID, perr := uuid.Parse(state.IDs[models.RailKeyPSPID])
			if perr != nil {
				continue
			}
			if _, derr := s.ResolveDriftForResource(ctx, pspID, models.CatalogDriftResourcePrice, priceID.String()); derr != nil {
				log.WithContext(ctx).WithError(derr).WithField("price_id", priceID.String()).Warn("catalog reconcile: drift resolution deferred to the next pass")
			}
		}
	}
	return &ReconcileResult{Providers: finalStates, Actions: actions}, nil
}

// ProductReconcileResult is the response of a ReconcileProduct call.
type ProductReconcileResult struct {
	// SyncStatus is the product's Stripe sync state after the pass
	// (in_sync / drifted / missing / sync_disabled / unknown).
	SyncStatus billing.SyncStatus `json:"sync_status"`
	// Drift carries the field-level divergence observed (pre-reconcile on DryRun,
	// otherwise the residual after the push).
	Drift []billing.DriftField `json:"drift,omitempty"`
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
	if !opts.DryRun {
		if err := s.checkCatalogWritePolicy(ctx); err != nil {
			return nil, err
		}
	}
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if productID == uuid.Nil {
		return nil, apperr.Invalidf("product_id required")
	}
	local, err := products.GetByID(ctx, productID)
	if err != nil {
		return nil, productLookup(err)
	}
	stripeProductID := s.lookupStripeProductID(ctx, productID)
	if stripeProductID == "" {
		// No Stripe Product is associated with this OpenRails product (no price
		// has a Stripe link). Nothing to reconcile.
		return &ProductReconcileResult{SyncStatus: billing.SyncStatusUnknown, Action: "missing"}, nil
	}

	adapter := &stripeAdapter{svc: s}
	drift, missing, configured, verifyErr := adapter.verifyStripeProduct(ctx, stripeProductID, local)
	if !configured {
		return &ProductReconcileResult{SyncStatus: billing.SyncStatusSyncDisabled, Action: "sync_disabled"}, nil
	}
	if verifyErr != nil {
		return nil, verifyErr
	}
	if missing {
		// The Stripe Product 404'd. Product recreate is out of scope here (it
		// would orphan the prices that reference the old product id); surface it.
		return &ProductReconcileResult{SyncStatus: billing.SyncStatusMissing, Action: "missing"}, nil
	}
	if len(drift) == 0 {
		return &ProductReconcileResult{SyncStatus: billing.SyncStatusInSync, Action: "no_op"}, nil
	}
	if opts.DryRun {
		return &ProductReconcileResult{SyncStatus: billing.SyncStatusDrifted, Drift: drift, Action: "would_update_remote"}, nil
	}

	// Push OpenRails values to Stripe: name, description, and the active flag.
	stripeSvc := &catalog.StripeCatalogService{StripeClients: s.rt.StripeClients, Config: s.rt.Config, Rails: s.rt.RailConfigs}
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
	syncStatus := billing.SyncStatusInSync
	if len(residual) > 0 {
		syncStatus = billing.SyncStatusDrifted
	}
	// Close product drift only for the active Stripe account just verified in sync.
	if syncStatus == billing.SyncStatusInSync {
		if account, ok, aerr := catalog.ActiveDriftPSP(ctx, s.rt.RailConfigs, models.RailStripe); aerr == nil && ok {
			if _, derr := s.ResolveDriftForResource(ctx, account.ID, models.CatalogDriftResourceProduct, productID.String()); derr != nil {
				log.WithContext(ctx).WithError(derr).WithField("product_id", productID.String()).Warn("catalog reconcile: drift resolution deferred to the next pass")
			}
		}
	}
	return &ProductReconcileResult{SyncStatus: syncStatus, Drift: residual, Action: "updated_remote"}, nil
}

// recreateStripePrice repairs a missing remote object for the same immutable
// local price. It never changes an existing remote object's money terms.
func (s *Service) recreateStripePrice(ctx context.Context, prices *catalog.PriceService, prod *models.Product, local *models.Price, priceID uuid.UUID, stripeProductID string) (string, error) {
	priceKey := priceID.String()
	stripeSvc := &catalog.StripeCatalogService{StripeClients: s.rt.StripeClients, Config: s.rt.Config, Rails: s.rt.RailConfigs}
	unitAmountCents, err := moneyutil.NativeToRailMinorExact(local.Currency, local.Amount)
	if err != nil {
		return "", err
	}
	newPriceID, err := stripeSvc.CreatePrice(ctx, catalog.CreatePriceParams{
		StripeProductID:  stripeProductID,
		UnitAmount:       int64(unitAmountCents),
		Currency:         local.Currency,
		BillingCycleDays: local.RecurringCycleDays(),
		LookupKey:        internalStripeLookupKey(priceID),
		// Repair retries use the retained local price identity.
		IdempotencyKey: "openrails-price-" + priceKey,
		Metadata: map[string]string{
			catalog.StripeMetadataOpenRailsPriceKey:   priceKey,
			catalog.StripeMetadataOpenRailsProductKey: strings.TrimSpace(prod.Key),
			// Informational only — not used for matching.
			catalog.StripeMetadataOpenRailsPriceID:   priceID.String(),
			catalog.StripeMetadataOpenRailsProductID: prod.ID.String(),
		},
	})
	if err != nil {
		return "", err
	}
	newRails := cloneRails(local.PSPLinks)
	if newRails["stripe"] == nil {
		newRails["stripe"] = map[string]string{models.RailKeyRail: string(models.RailStripe)}
	}
	newRails["stripe"][models.RailKeyStripePriceID] = newPriceID
	if err := prices.UpdatePSPLinks(ctx, priceID, newRails); err != nil {
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
