package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"

	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ProviderStatus is the per-provider attachment state surfaced in admin
// responses. Issue #208 defines these four values.
type ProviderStatus = openrails.ProviderStatus

const (
	ProviderStatusLinked            ProviderStatus = "linked"
	ProviderStatusPendingManualLink ProviderStatus = "pending_manual_link"
	ProviderStatusSyncDisabled      ProviderStatus = "sync_disabled"
	ProviderStatusError             ProviderStatus = "error"
)

// SyncStatus is the per-provider freshness/drift state. Populated only by
// paths that perform a live retrieve (?verify=true reads or reconcile);
// otherwise defaults to "unknown".
type SyncStatus = openrails.SyncStatus

const (
	SyncStatusUnknown      SyncStatus = "unknown"
	SyncStatusInSync       SyncStatus = "in_sync"
	SyncStatusDrifted      SyncStatus = "drifted"
	SyncStatusMissing      SyncStatus = "missing"
	SyncStatusNeverSynced  SyncStatus = "never_synced"
	SyncStatusSyncDisabled SyncStatus = "sync_disabled"
)

// ProviderState is the uniform per-provider response surface. Replaces the
// pre-#208 stripe-specific StripeRailState.
type ProviderState = openrails.ProviderState

// DriftField describes a single divergent field discovered by verify/reconcile.
// Replaces the pre-#208 RailDriftField (Stripe-only).
type DriftField = openrails.DriftField

// CatalogProduct is the OpenRails-side view of a product. Products are pure
// OpenRails concepts and have NO direct provider linkage in the user-facing
// shape — provider state lives on CatalogPrice (issue #208).
//
// The Stripe Product ID some prices carry is purely an artifact of Stripe's
// requirement that every Stripe Price attach to a Stripe Product; it is
// denormalized onto price rows and managed implicitly by price-level
// operations. There is no product-level provider field, no product-level
// verify/reconcile, no product-level reconcile route.
type CatalogProduct = openrails.CatalogProduct

type CreateProductRequest = openrails.CreateProductRequest

func (s *Service) CreateProduct(ctx context.Context, req CreateProductRequest) (*CatalogProduct, error) {
	owned, err := catalogOwnerRequest(ctx)
	if err != nil {
		return nil, err
	}
	if owned && (req.EntitlementsSpec != nil || req.TierGroup != nil || req.TierRank != 0) {
		return nil, catalog.ErrOwnerOperation
	}
	if owned && !req.CatalogID.IsZero() && req.CatalogID.UUID() != *catalogscope.QueryID(ctx) {
		return nil, catalog.ErrOwnerScope
	}
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*CatalogProduct, error) {
		return scoped.createProduct(ctx, req)
	})
}

func (s *Service) createProduct(ctx context.Context, req CreateProductRequest) (*CatalogProduct, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	req.Key = strings.TrimSpace(req.Key)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Description = strings.TrimSpace(req.Description)
	if req.Key == "" {
		return nil, apperr.Invalidf("key required")
	}
	if req.DisplayName == "" {
		return nil, apperr.Invalidf("display_name required")
	}

	now := time.Now().UTC()
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	p := &models.Product{
		// #662: the product id is a pure function of its immutable natural key
		// (merchant_id, key) — same logical product → same id in every DB.
		ID:               uuidutil.DeterministicID(uuidutil.DeterministicNamespace, tid.UUID().String(), req.Key),
		MerchantID:       tid.UUID(),
		CatalogID:        req.CatalogID.UUID(),
		Key:              req.Key,
		DisplayName:      req.DisplayName,
		Description:      req.Description,
		EntitlementsSpec: req.EntitlementsSpec,
		TierGroup:        req.TierGroup,
		TierRank:         req.TierRank,
		Archived:         req.Archived,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := products.Create(ctx, p); err != nil {
		return nil, catalogWrite(err)
	}
	return productToCatalogProduct(p), nil
}

// ErrProductTierGroupInUse reports a product identity conflict with live subscriptions.
var ErrProductTierGroupInUse = catalog.ErrProductTierGroupInUse

// UpdateProductRequest is a field-selective patch. Nil scalar pointers (including
// JSON null) leave fields unchanged; an empty description clears it. Definitions
// change only with their Set flag: true plus nil sets SQL NULL, true plus an empty
// map sets an empty definition, and false omits the field regardless of its value.
// Same-field concurrent patches use last-committed-write wins.
type UpdateProductRequest = openrails.UpdateProductRequest

func (s *Service) UpdateProduct(ctx context.Context, id openrails.ProductID, req UpdateProductRequest) (*CatalogProduct, error) {
	owned, err := catalogOwnerRequest(ctx)
	if err != nil {
		return nil, err
	}
	if owned && (req.EntitlementsSpec != nil || req.SetEntitlements || req.TierGroup != nil || req.SetTierGroup || req.TierRank != nil || req.SkipRailSync) {
		return nil, catalog.ErrOwnerOperation
	}
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*CatalogProduct, error) {
		return scoped.updateProduct(ctx, id, req)
	})
}

func (s *Service) updateProduct(ctx context.Context, id openrails.ProductID, req UpdateProductRequest) (*CatalogProduct, error) {
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
	p, err := products.UpdateDefinition(ctx, productID, catalog.ProductDefinitionUpdateParams{
		DisplayName:      req.DisplayName,
		Description:      req.Description,
		EntitlementsSpec: req.EntitlementsSpec,
		SetEntitlements:  req.SetEntitlements,
		TierGroup:        req.TierGroup,
		SetTierGroup:     req.SetTierGroup,
		TierRank:         req.TierRank,
		Archived:         req.Archived,
	})
	if err != nil {
		return nil, productLookup(err)
	}

	s.catalogAfterCommit(ctx, func(ctx context.Context, s *Service) {
		// Propagate mutable Product changes to Stripe (display name + description + active).
		// The Stripe product ID is not stored on the OpenRails product row itself —
		// it lives on associated prices' psp_links.stripe.product_id. Look up one
		// such price to find it; if no prices have a Stripe link yet, there is
		// nothing to propagate (no Stripe Product exists for this OpenRails product).
		if !s.localCatalogOnly && !req.SkipRailSync && (req.DisplayName != nil || req.Description != nil || req.Archived != nil) && s.rt.Config != nil {
			stripeProductID := s.lookupStripeProductID(ctx, productID)
			if stripeProductID != "" {
				stripeSvc := &catalog.StripeCatalogService{StripeClients: s.rt.StripeClients, Config: s.rt.Config, Rails: s.rt.RailConfigs}
				params := catalog.UpdateProductParams{}
				if req.DisplayName != nil {
					name := strings.TrimSpace(*req.DisplayName)
					params.Name = &name
				}
				if req.Description != nil {
					desc := strings.TrimSpace(*req.Description)
					params.Description = &desc
				}
				if req.Archived != nil {
					// archived -> Stripe active=false.
					active := !*req.Archived
					params.Active = &active
				}
				// Best-effort propagation: log on failure, do not roll back the DB change.
				// Drift will surface on next ?verify=true read.
				_ = stripeSvc.UpdateProduct(ctx, stripeProductID, params)
			}
		}

		// #586: when entitlements change, re-sync the product's Stripe Features so the
		// mirror matches OpenRails. An emptied spec detaches all OpenRails-managed
		// features. Best-effort, like the propagation above. Only runs once a Stripe
		// Product exists for this product (i.e. a price has linked it).
		if !s.localCatalogOnly && !req.SkipRailSync && req.SetEntitlements && s.rt.Config != nil {
			if stripeProductID := s.lookupStripeProductID(ctx, productID); stripeProductID != "" {
				stripeSvc := &catalog.StripeCatalogService{StripeClients: s.rt.StripeClients, Config: s.rt.Config, Rails: s.rt.RailConfigs}
				keys := make([]string, 0, len(p.EntitlementsSpec))
				for k := range p.EntitlementsSpec {
					keys = append(keys, k)
				}
				_ = stripeSvc.SyncProductFeatures(ctx, stripeProductID, keys)
			}
		}

	})

	return productToCatalogProduct(p), nil
}

// propagateProductActiveToStripe pushes the product's active flag to its Stripe
// Product, if one exists. Used by the lifecycle paths (activate / deactivate /
// SetProductStatus) so a status change reaches Stripe — UpdateProduct already
// handles the display_name/description/active propagation for definition edits,
// but the dedicated lifecycle entrypoints bypass it. Best-effort: failures are
// swallowed (drift surfaces on the next product reconcile).
func (s *Service) propagateProductActiveToStripe(ctx context.Context, productID uuid.UUID, active bool) {
	s.catalogAfterCommit(ctx, func(ctx context.Context, committed *Service) {
		committed.propagateProductActiveToStripeCommitted(ctx, productID, active)
	})
}

func (s *Service) propagateProductActiveToStripeCommitted(ctx context.Context, productID uuid.UUID, active bool) {
	if s.localCatalogOnly {
		return
	}
	if s.rt == nil || s.rt.Config == nil {
		return
	}
	stripeProductID := s.lookupStripeProductID(ctx, productID)
	if stripeProductID == "" {
		return
	}
	stripeSvc := &catalog.StripeCatalogService{StripeClients: s.rt.StripeClients, Config: s.rt.Config, Rails: s.rt.RailConfigs}
	a := active
	_ = stripeSvc.UpdateProduct(ctx, stripeProductID, catalog.UpdateProductParams{Active: &a})
}

// lookupStripeProductID returns the Stripe Product ID associated with the
// given OpenRails product by scanning its prices for psp_links.stripe.product_id.
// Returns "" if no associated price has a Stripe Product link.
func (s *Service) lookupStripeProductID(ctx context.Context, productID uuid.UUID) string {
	if s.rt.PriceService == nil {
		return ""
	}
	priceList, err := s.rt.PriceService.GetByProductID(ctx, productID)
	if err != nil {
		return ""
	}
	for _, p := range priceList {
		if id := strings.TrimSpace(p.PSPLinkForRail(models.RailStripe)[models.RailKeyStripeProductID]); id != "" {
			return id
		}
	}
	return ""
}

func productToCatalogProduct(p *models.Product) *CatalogProduct {
	return &CatalogProduct{
		ID:               openrails.ProductID(p.ID),
		CatalogID:        openrails.CatalogID(p.CatalogID),
		Key:              p.Key,
		DisplayName:      p.DisplayName,
		Description:      p.Description,
		EntitlementsSpec: p.EntitlementsSpec,
		TierGroup:        p.TierGroup,
		TierRank:         p.TierRank,
		Archived:         p.Archived,
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
	}
}

// CatalogPrice is the OpenRails-side view of a price. The declarative
// `providers` shape is the only rail configuration surface.
type CatalogPrice = openrails.CatalogPrice

// CreatePriceRequest is the declarative-shape create request introduced in
// issue #208. Callers state which providers a price should exist in (Providers)
// and, optionally, pre-supply provider-specific link ids (ProviderLinks). For
// each provider:
//   - if a non-empty link map is supplied: the adapter validates and stores it.
//   - if no link is supplied and the adapter SupportsAutoCreate: the adapter
//     mints a new external object (today: stripe only).
//   - if no link is supplied and the adapter does not SupportsAutoCreate: the
//     price is created in OpenRails with a pending_manual_link status for
//     that provider; the response carries a PendingAction telling the operator
//     what to do.
type CreatePriceRequest = openrails.CreatePriceRequest

// priceNaturalKeyNull is the canonical encoding of a SQL NULL price field for
// id derivation (#662). The unique_prices_product_amount_window index is
// NULLS NOT DISTINCT (a NULL equals another NULL), so every absent value must
// hash to ONE fixed token — and it carries a NUL byte, which a decimal integer
// string can never contain, so it can never collide with a present value.
const priceNaturalKeyNull = "\x00null"

// priceDeterministicID derives a price's id from the immutable financial tuple
// that IS its identity — exactly the unique_prices_product_amount_window
// columns (#662), canonicalized the SAME way that NULLS-NOT-DISTINCT unique
// index compares them (amount as decimal, currency lowercased, absent nullable
// fields → priceNaturalKeyNull). Equal terms therefore always hash equal, so a
// derived id can never violate the constraint; a reprice changes a frozen field
// and correctly hashes to a new id while the archived old row keeps its own.
func priceDeterministicID(productID uuid.UUID, amount int64, currency string, accessDurationHours *int, autoRenew bool, trialUnitAmount *int64, trialDurationHours *int) uuid.UUID {
	accessDur := priceNaturalKeyNull
	if accessDurationHours != nil {
		accessDur = strconv.Itoa(*accessDurationHours)
	}
	trialAmt := priceNaturalKeyNull
	if trialUnitAmount != nil {
		trialAmt = strconv.FormatInt(*trialUnitAmount, 10)
	}
	trialDur := priceNaturalKeyNull
	if trialDurationHours != nil {
		trialDur = strconv.Itoa(*trialDurationHours)
	}
	return uuidutil.DeterministicID(
		uuidutil.DeterministicNamespace,
		productID.String(),
		strconv.FormatInt(amount, 10),
		strings.ToLower(currency),
		accessDur,
		strconv.FormatBool(autoRenew),
		trialAmt,
		trialDur,
	)
}

// RecurringCycleDays returns the recurring billing cadence in WHOLE DAYS for an
// auto-renewing request, or nil for a one-off/durable price (#622). The window
// is in hours; providers bill in days, so the cadence is hours/24.
func priceRequestCycleDays(req CreatePriceRequest) *int {
	if !req.AutoRenew || req.AccessDurationHours == nil {
		return nil
	}
	days := *req.AccessDurationHours / 24
	return &days
}

func (s *Service) CreatePrice(ctx context.Context, req CreatePriceRequest) (*CatalogPrice, error) {
	owned, err := catalogOwnerRequest(ctx)
	if err != nil {
		return nil, err
	}
	if owned && (req.PSPs != nil || req.PSPLinks != nil) {
		return nil, catalog.ErrOwnerOperation
	}
	if err := s.checkCatalogWritePolicy(ctx); err != nil {
		return nil, err
	}
	if req.ProductData != nil {
		return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*CatalogPrice, error) {
			return scoped.createPrice(ctx, req, owned)
		})
	}
	return s.createPrice(ctx, req, owned)
}

func (s *Service) createPrice(ctx context.Context, req CreatePriceRequest, owned bool) (*CatalogPrice, error) {
	if req.ProductData != nil {
		return s.createPriceWithProduct(ctx, req)
	}
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, _, err := s.requireCatalogServices()
	if err != nil {
		return nil, err
	}
	if req.ProductID.IsZero() {
		return nil, apperr.Invalidf("product_id required")
	}
	// CUR-6: canonicalise at the price WRITE boundary. ValidateCurrency below
	// is case-insensitive, so without this a caller-supplied "usd" validated
	// fine and then failed the prices_currency_shape CHECK at INSERT.
	req.Currency = money.NormalizeCurrency(req.Currency)
	if err := validateCatalogPriceTerms(req); err != nil {
		return nil, err
	}

	product, err := products.GetByID(ctx, req.ProductID.UUID())
	if err != nil {
		return nil, productLookup(err)
	}

	// #662: the price id is a pure function of its immutable financial tuple —
	// exactly the unique_prices_product_amount_window columns. A reprice hashes
	// to a NEW id (the archived old row keeps its own); equal terms always hash
	// equal, so the id can never violate that unique constraint.
	priceID := priceDeterministicID(req.ProductID.UUID(), req.UnitAmount, req.Currency, req.AccessDurationHours, req.AutoRenew, req.TrialUnitAmount, req.TrialDurationHours)
	if owned {
		req.PSPs, err = s.creatorProviderKeys(ctx)
		if err != nil {
			return nil, err
		}
	}

	var rails map[string]map[string]string
	var providerStates map[string]ProviderState
	var pending []PendingAction
	if prepared, ok := s.catalogPreparedLinks[req.Key]; s.localCatalogOnly && ok {
		rails = cloneRails(prepared)
	} else {
		rails, providerStates, pending, err = s.resolveProviders(ctx, product, req, priceID)
		if err != nil {
			return nil, err
		}
	}

	price, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*models.Price, error) {
		products, err := scoped.requireProductService()
		if err != nil {
			return nil, err
		}
		current, err := products.GetByID(ctx, product.ID)
		if err != nil {
			return nil, productLookup(err)
		}
		if !reflect.DeepEqual(productToCatalogProduct(current), productToCatalogProduct(product)) {
			return nil, ErrCatalogConflict
		}
		price, err := scoped.writeCatalogPrice(ctx, req, current, priceID, rails)
		if err != nil {
			return nil, err
		}
		if prepared, ok := scoped.catalogPreparedLinks[req.Key]; scoped.localCatalogOnly && ok {
			prices, err := scoped.requirePriceService()
			if err != nil {
				return nil, err
			}
			if !sameCatalogLinks(price.PSPLinks, prepared) {
				if err := prices.UpdatePSPLinks(ctx, price.ID, prepared); err != nil {
					return nil, err
				}
				return prices.GetByID(ctx, price.ID)
			}
		}
		return price, nil
	})
	if err != nil {
		return nil, err
	}

	// Created-as-archived: the providers were auto-created active above, so
	// propagate active=false to match the archived lifecycle (best-effort; drift
	// surfaces on next verify if a provider rejects it).
	if !s.localCatalogOnly && req.Archived && len(rails) > 0 && !s.catalogRemoteWritesDisabled() {
		inactive := false
		adapters := s.providerAdapters()
		for provider, ids := range rails {
			adapter, ok := adapters[strings.ToLower(strings.TrimSpace(provider))]
			if !ok {
				continue
			}
			_ = adapter.Update(ctx, ids, mutableUpdate{IsActive: &inactive})
		}
	}
	out := priceToCatalogPrice(price)
	// Overlay the dispatcher-computed states (they carry the freshly-minted
	// IDs and the per-provider status for the create response). priceToCatalogPrice
	// alone can only see what's in the row; the dispatcher also knows which
	// providers came back as pending_manual_link and why.
	if len(providerStates) > 0 {
		out.Providers = providerStates
	}
	if len(pending) > 0 {
		out.PendingManualActions = pending
	}
	return out, nil
}

// UpdatePriceRequest is the declarative-shape PATCH for a price. Add or rotate
// PSP links via `psp_links` (partial merge into the existing map). To clear a
// PSP entirely, supply an empty inner map for it and set ReplacePSPLinks=true.
type UpdatePriceRequest = openrails.UpdatePriceRequest

func (s *Service) UpdatePrice(ctx context.Context, id openrails.PriceID, req UpdatePriceRequest) (*CatalogPrice, error) {
	owned, err := catalogOwnerRequest(ctx)
	if err != nil {
		return nil, err
	}
	if owned && (req.PSPLinks != nil || req.ReplacePSPLinks || req.SkipRailSync) {
		return nil, catalog.ErrOwnerOperation
	}
	if err := s.checkCatalogWritePolicy(ctx); err != nil {
		return nil, err
	}
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	return s.updatePrice(ctx, id, req)
}

func (s *Service) updatePrice(ctx context.Context, id openrails.PriceID, req UpdatePriceRequest) (*CatalogPrice, error) {

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("price_id required")
	}
	priceID := id.UUID()
	// Declarative PSP link rotation. ReplacePSPLinks=true overwrites the
	// entire psp_links map; otherwise the supplied entries are merged
	// into the existing map (partial PATCH). Empty inner maps clear a provider.
	existing, getErr := prices.GetByID(ctx, priceID)
	if getErr != nil {
		return nil, priceLookup(getErr)
	}
	var preparedProduct *models.Product
	var next map[string]map[string]string
	var pending []PendingAction
	if req.PSPLinks != nil {
		// The existing price + its product give the substance (product key + money terms)
		// each adapter's Attach validates the supplied link against. Fetch it
		// regardless of merge/replace so a rotated link is verified, not blindly
		// stored.
		if s.localCatalogOnly {
			return nil, apperr.Invalidf("provider link changes must be prepared outside a catalog application")
		}
		products, err := s.requireProductService()
		if err != nil {
			return nil, err
		}
		preparedProduct, err = products.GetByID(ctx, existing.ProductID)
		if err != nil {
			return nil, productLookup(err)
		}
		pctx, ctxErr := s.priceLinkContext(ctx, existing)
		if ctxErr != nil {
			return nil, ctxErr
		}
		if req.ReplacePSPLinks {
			next = map[string]map[string]string{}
		} else {
			next = cloneRails(existing.PSPLinks)
			if next == nil {
				next = map[string]map[string]string{}
			}
		}
		adapters := s.providerAdapters()
		accountRails := s.merchantAccountRails(ctx)
		for psp, link := range req.PSPLinks {
			psp = strings.ToLower(strings.TrimSpace(psp))
			if psp == "" {
				continue
			}
			normalized := normalizeLinkMap(link)
			// Empty link map = clear this PSP (only on merge; replace already
			// starts empty).
			if len(normalized) == 0 {
				delete(next, psp)
				continue
			}
			rail := psp
			adapter, ok := adapters[psp]
			if !ok {
				if acct, found := accountRails[psp]; found {
					rail = acct.rail
					adapter, ok = adapters[acct.rail]
				}
			}
			if !ok {
				return nil, apperr.Invalidf("unknown PSP %q: not a rail or a declared PSP key", psp)
			}
			ids, attachErr := adapter.Attach(ctx, normalized, pctx)
			if errors.Is(attachErr, errPendingManualLink) || errors.Is(attachErr, errRemoteWritesDisabled) {
				template := adapter.PendingActionTemplate(priceID)
				if template.Provider == "" {
					template.Provider = rail
				}
				pending = append(pending, template)
				// The rotation did NOT take effect. Keep the previously stored
				// (verified) link so a ReplacePSPLinks pass never deletes it
				// while the response only reports a pending action.
				if prev, ok := existing.PSPLinks[psp]; ok {
					if _, kept := next[psp]; !kept {
						next[psp] = maps.Clone(prev)
					}
				}
				continue
			}
			if attachErr != nil {
				return nil, fmt.Errorf("%s: %w", psp, attachErr)
			}
			if ids == nil {
				ids = map[string]string{}
			}
			ids[models.RailKeyRail] = rail
			next[psp] = ids
		}
	}
	updated, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*models.Price, error) {
		prices, err := scoped.requirePriceService()
		if err != nil {
			return nil, err
		}
		current, err := prices.GetByID(ctx, priceID)
		if err != nil {
			return nil, priceLookup(err)
		}
		if !reflect.DeepEqual(current, existing) {
			return nil, ErrCatalogConflict
		}
		if preparedProduct != nil {
			products, err := scoped.requireProductService()
			if err != nil {
				return nil, err
			}
			product, err := products.GetByID(ctx, preparedProduct.ID)
			if err != nil {
				return nil, productLookup(err)
			}
			if !reflect.DeepEqual(productToCatalogProduct(product), productToCatalogProduct(preparedProduct)) {
				return nil, ErrCatalogConflict
			}
		}
		if req.PSPLinks != nil {
			if err := prices.UpdatePSPLinks(ctx, priceID, next); err != nil {
				return nil, priceLookup(err)
			}
		}
		if req.Archived != nil {
			// This method propagates once, after its complete local commit and
			// only when SkipRailSync permits it; nested lifecycle work is local.
			lifecycle := *scoped
			lifecycle.localCatalogOnly = true
			if *req.Archived {
				_, err = lifecycle.deactivatePrice(ctx, id)
			} else {
				_, err = lifecycle.activatePrice(ctx, id)
			}
			if err != nil {
				return nil, err
			}
		}
		return prices.GetByID(ctx, priceID)
	})
	if err != nil {
		return nil, err
	}

	// Propagate mutable changes to every attached provider via its adapter.
	// Only when the caller did not opt out via SkipRailSync. Failures are
	// logged-and-swallowed: drift will surface on the next ?verify=true read.
	if !s.localCatalogOnly && !req.SkipRailSync && req.Archived != nil {
		active := !*req.Archived
		mutable := mutableUpdate{IsActive: &active}
		if !s.catalogRemoteWritesDisabled() {
			adapters := s.providerAdapters()
			for _, ids := range updated.PSPLinks {
				// Entries are account-keyed; the rail lives in the entry.
				adapter, ok := adapters[strings.ToLower(strings.TrimSpace(ids[models.RailKeyRail]))]
				if !ok {
					continue
				}
				_ = adapter.Update(ctx, ids, mutable)
			}
		}
	}

	out := priceToCatalogPrice(updated)
	out.PendingManualActions = pending
	return out, nil
}

// priceToCatalogPrice maps the DB row into the response shape, including the
// per-provider Providers map. SyncStatus defaults to "unknown" — only paths
// that perform a live retrieve (?verify=true, reconcile) populate richer
// values.
func priceToCatalogPrice(p *models.Price) *CatalogPrice {
	cp := &CatalogPrice{
		ID:                  openrails.PriceID(p.ID),
		Key:                 p.Key,
		ProductID:           openrails.ProductID(p.ProductID),
		Archived:            p.Archived,
		UnitAmount:          p.Amount,
		Currency:            p.Currency,
		AccessDurationHours: p.AccessDurationHours,
		AutoRenew:           p.AutoRenew,
		TrialUnitAmount:     p.TrialUnitAmount,
		TrialDurationHours:  p.TrialDurationHours,
		CreatedAt:           p.CreatedAt,
		UpdatedAt:           p.UpdatedAt,
	}
	if len(p.PSPLinks) == 0 {
		return cp
	}
	cp.Providers = make(map[string]ProviderState, len(p.PSPLinks))
	for name, ids := range p.PSPLinks {
		if len(ids) == 0 {
			continue
		}
		state := ProviderState{
			Status:     ProviderStatusLinked,
			IDs:        copyStringMap(ids),
			LookupKey:  strings.TrimSpace(ids[providerLookupKey]),
			SyncStatus: SyncStatusUnknown,
		}
		cp.Providers[name] = state
	}
	return cp
}

func validateCatalogPriceTerms(req CreatePriceRequest) error {
	if req.UnitAmount < 0 {
		return apperr.Invalidf("unit_amount must be non-negative")
	}
	if req.Currency == "" {
		return apperr.Invalidf("currency required")
	}
	// #622 access window: a finite window must be positive; auto_renew needs one.
	if req.AccessDurationHours != nil && *req.AccessDurationHours <= 0 {
		return apperr.Invalidf("access_duration_hours must be positive (omit for indefinite)")
	}
	if req.AutoRenew && req.AccessDurationHours == nil {
		return apperr.Invalidf("auto_renew requires a finite access_duration_hours")
	}
	// #622 trial: both-or-neither; non-negative amount (0 = free trial); positive
	// period; only on an auto-renewing price (there is a "then recurring" part).
	if (req.TrialUnitAmount == nil) != (req.TrialDurationHours == nil) {
		return apperr.Invalidf("trial_unit_amount and trial_duration_hours must be set together")
	}
	if req.TrialUnitAmount != nil {
		if *req.TrialUnitAmount < 0 {
			return apperr.Invalidf("trial_unit_amount must be >= 0 (0 = free trial)")
		}
		if *req.TrialDurationHours <= 0 {
			return apperr.Invalidf("trial_duration_hours must be positive")
		}
		if !req.AutoRenew {
			return apperr.Invalidf("trial pricing requires auto_renew")
		}
	}
	if err := moneyutil.ValidateCurrency(req.Currency); err != nil {
		return apperr.Invalidf("%v", err)
	}

	return nil
}
