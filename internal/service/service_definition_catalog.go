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
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

func (s *Service) CreateProduct(ctx context.Context, req billing.CreateProductParams) (*billing.Product, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Product, error) {
		return scoped.createProduct(ctx, req)
	})
}

func (s *Service) createProduct(ctx context.Context, req billing.CreateProductParams) (*billing.Product, error) {
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

	credit, err := normalizeCreditGrant(req.CreditGrant)
	if err != nil {
		return nil, err
	}
	req.CreditGrant = credit
	now := time.Now().UTC()
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	p := &models.Product{
		// #662: the product id is a pure function of its immutable natural key
		// (merchant_id, key) — same logical product → same id in every DB.
		ID:           uuidutil.DeterministicID(uuidutil.DeterministicNamespace, tid.UUID().String(), req.Key),
		MerchantID:   tid.UUID(),
		Key:          req.Key,
		DisplayName:  req.DisplayName,
		Description:  req.Description,
		Entitlements: req.Entitlements, CreditGrant: req.CreditGrant,
		TierGroup: req.TierGroup,
		TierRank:  req.TierRank,
		Archived:  req.Archived,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := products.Create(ctx, p, s.keyEdit(ctx)); err != nil {
		return nil, catalogWrite(err)
	}
	return productToCatalogProduct(p), nil
}

// ErrProductTierGroupInUse reports a product identity conflict with live subscriptions.
var ErrProductTierGroupInUse = catalog.ErrProductTierGroupInUse

// UpdateProductRequest is the engine's product patch: a nil field is left
// as it is, and a Set flag writes its nullable field (nil clears it). An
// empty description clears it. SkipRailSync keeps the change local; the
// catalog application reconciles PSPs itself.
type UpdateProductRequest struct {
	CreditGrant    *catalogwire.CreditGrantSpec
	SetCreditGrant bool
	// DeferCreditPriceValidation is used only by an atomic catalog batch, which
	// validates the final product and live prices after applying every edit.
	DeferCreditPriceValidation bool
	DisplayName                *string
	Description                *string
	Entitlements               []string
	SetEntitlements            bool
	TierGroup                  *string
	SetTierGroup               bool
	TierRank                   *int
	Archived                   *bool
	SkipRailSync               bool
}

// productPatch reads a merge patch: null clears description and tier_group.
// Entitlements must be a list; [] clears it and omission leaves it unchanged.
func productPatch(p billing.UpdateProductParams) (UpdateProductRequest, error) {
	var req UpdateProductRequest
	if p.DisplayName.Null || p.TierRank.Null || p.Archived.Null {
		return req, apperr.Invalidf("display_name, tier_rank and archived cannot be null")
	}
	if p.DisplayName.Set {
		req.DisplayName = &p.DisplayName.Value
	}
	if p.Description.Set {
		req.Description = &p.Description.Value
	}
	if p.Entitlements.Set {
		if p.Entitlements.Null || p.Entitlements.Value == nil {
			return req, apperr.Invalidf("entitlements must be a string list, not null; use [] for none")
		}
		req.SetEntitlements, req.Entitlements = true, p.Entitlements.Value
	}
	if p.CreditGrant.Set {
		req.SetCreditGrant = true
		if !p.CreditGrant.Null {
			req.CreditGrant = &p.CreditGrant.Value
		}
	}
	if p.TierGroup.Set {
		req.SetTierGroup = true
		if !p.TierGroup.Null {
			req.TierGroup = &p.TierGroup.Value
		}
	}
	if p.TierRank.Set {
		req.TierRank = &p.TierRank.Value
	}
	if p.Archived.Set {
		req.Archived = &p.Archived.Value
	}
	return req, nil
}

// UpdateProduct applies a merge patch to a product.
func (s *Service) UpdateProduct(ctx context.Context, id billing.ProductID, params billing.UpdateProductParams) (*billing.Product, error) {
	req, err := productPatch(params)
	if err != nil {
		return nil, err
	}
	return s.patchProduct(ctx, id, req)
}

func (s *Service) patchProduct(ctx context.Context, id billing.ProductID, req UpdateProductRequest) (*billing.Product, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Product, error) {
		return scoped.updateProduct(ctx, id, req)
	})
}

func (s *Service) updateProduct(ctx context.Context, id billing.ProductID, req UpdateProductRequest) (*billing.Product, error) {
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
	if req.SetCreditGrant {
		credit, err := normalizeCreditGrant(req.CreditGrant)
		if err != nil {
			return nil, err
		}
		req.CreditGrant = credit
	}
	if !req.DeferCreditPriceValidation && (req.SetCreditGrant || req.Archived != nil && !*req.Archived) {
		current, err := products.GetByID(ctx, id.UUID())
		if err != nil {
			return nil, productLookup(err)
		}
		credit, archived := current.CreditGrant, current.Archived
		if req.SetCreditGrant {
			credit = req.CreditGrant
		}
		if req.Archived != nil {
			archived = *req.Archived
		}
		if !archived {
			if err := s.validateProductCreditUpdate(ctx, id, credit); err != nil {
				return nil, err
			}
		}
	}
	productID := id.UUID()
	p, keys, err := products.UpdateDefinition(ctx, productID, catalog.ProductDefinitionUpdateParams{
		DisplayName:     req.DisplayName,
		Description:     req.Description,
		Entitlements:    req.Entitlements,
		SetEntitlements: req.SetEntitlements, CreditGrant: req.CreditGrant, SetCreditGrant: req.SetCreditGrant,
		TierGroup:    req.TierGroup,
		SetTierGroup: req.SetTierGroup,
		TierRank:     req.TierRank,
		Archived:     req.Archived,
		KeyEdit:      s.keyEdit(ctx),
	})
	if err != nil {
		return nil, productLookup(err)
	}
	if keys.Changed() {
		change := entitlementChange(p, keys)
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		if err := announceKeyChanges(ctx, s.catalogDatabase().Gen(ctx), mid.UUID(), s.keyEdit(ctx).At, []*billing.EntitlementChange{&change}); err != nil {
			return nil, err
		}
		if s.catalogKeyChanges != nil {
			*s.catalogKeyChanges = append(*s.catalogKeyChanges, change)
		}
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
				_ = stripeSvc.SyncProductFeatures(ctx, stripeProductID, p.Entitlements)
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

func productToCatalogProduct(p *models.Product) *billing.Product {
	v := p.View()
	return &v
}

// priceNaturalKeyNull is the canonical encoding of a SQL NULL price field for
// id derivation (#662). The prices_product_amount_window_key index is
// NULLS NOT DISTINCT (a NULL equals another NULL), so every absent value must
// hash to ONE fixed token — and it carries a NUL byte, which a decimal integer
// string can never contain, so it can never collide with a present value.
const priceNaturalKeyNull = "\x00null"

// priceDeterministicID derives a price's id from the immutable financial tuple
// that IS its identity — exactly the prices_product_amount_window_key
// columns (#662), canonicalized the SAME way that NULLS-NOT-DISTINCT unique
// index compares them (amount as decimal, currency lowercased, absent nullable
// fields → priceNaturalKeyNull). Equal terms therefore always hash equal, so a
// derived id can never violate the constraint; a reprice changes a frozen field
// and correctly hashes to a new id while the archived old row keeps its own.
func priceDeterministicID(productID uuid.UUID, key string, amount int64, currency string, accessDurationHours, billingIntervalHours *int, trialUnitAmount *int64, trialDurationHours *int) uuid.UUID {
	accessDur := priceNaturalKeyNull
	if accessDurationHours != nil {
		accessDur = strconv.Itoa(*accessDurationHours)
	}
	billingInterval := priceNaturalKeyNull
	if billingIntervalHours != nil {
		billingInterval = strconv.Itoa(*billingIntervalHours)
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
		productID.String(), key,
		strconv.FormatInt(amount, 10),
		strings.ToLower(currency),
		accessDur,
		billingInterval,
		trialAmt,
		trialDur,
	)
}

// priceRequestCycleDays returns the billing cadence in days, independently of
// the access window. A nil interval means a one-time price.
func priceRequestCycleDays(req billing.CreatePriceParams) *int {
	if req.BillingIntervalHours == nil {
		return nil
	}
	days := *req.BillingIntervalHours / 24
	return &days
}

func (s *Service) CreatePrice(ctx context.Context, req billing.CreatePriceParams) (*billing.Price, error) {
	if err := s.checkCatalogWritePolicy(ctx); err != nil {
		return nil, err
	}
	selectors := 0
	if !req.ProductID.IsZero() {
		selectors++
	}
	if req.ProductKey != "" {
		selectors++
		if strings.TrimSpace(req.ProductKey) == "" {
			return nil, apperr.Invalidf("product_key is invalid")
		}
	}
	if req.ProductData != nil {
		selectors++
	}
	if selectors != 1 {
		return nil, apperr.Invalidf("exactly one of product_id, product_key and product_data is required")
	}
	if req.ProductData != nil {
		return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Price, error) {
			return scoped.createPrice(ctx, req)
		})
	}
	return s.createPrice(ctx, req)
}

func (s *Service) createPrice(ctx context.Context, req billing.CreatePriceParams) (*billing.Price, error) {
	if req.ProductData != nil {
		return s.createPriceWithProduct(ctx, req)
	}
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, prices, err := s.requireCatalogServices()
	if err != nil {
		return nil, err
	}
	if req.ProductID.IsZero() && req.ProductKey == "" {
		return nil, apperr.Invalidf("product_id required")
	}
	// CUR-6: canonicalise at the price WRITE boundary. ValidateCurrency below
	// is case-insensitive, so without this a caller-supplied "usd" validated
	// fine and then failed the prices_currency_check CHECK at INSERT.
	req.Currency = money.NormalizeCurrency(req.Currency)
	if err := validateCatalogPriceTerms(req); err != nil {
		return nil, err
	}

	var product *models.Product
	if req.ProductKey != "" {
		product, err = products.GetByKey(ctx, req.ProductKey)
	} else {
		product, err = products.GetByID(ctx, req.ProductID.UUID())
	}
	if err != nil {
		return nil, productLookup(err)
	}
	req.ProductID = billing.ProductID(product.ID)
	if err := validateCreditPrice(product.CreditGrant, req); err != nil {
		return nil, err
	}

	// #662: the price id is a pure function of its immutable financial tuple —
	// exactly the prices_product_amount_window_key columns. A reprice hashes
	// to a NEW id (the archived old row keeps its own); equal terms always hash
	// equal, so the id can never violate that unique constraint.
	key, _ := resolvePriceKey(product, req)
	priceID := priceDeterministicID(req.ProductID.UUID(), key, req.UnitAmount, req.Currency, req.AccessDurationHours, req.BillingIntervalHours, req.TrialUnitAmount, req.TrialDurationHours)
	if req.CustomerAmount != nil {
		priceID = uuidutil.DeterministicID(priceID, "customer_amount", strconv.FormatInt(req.CustomerAmount.MinAmount, 10), strconv.FormatInt(req.CustomerAmount.MaxAmount, 10))
	}
	existing, err := prices.FindByTerms(ctx, req, key)
	if err == nil {
		priceID = existing.ID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var rails map[string]map[string]string
	var providerStates map[string]billing.PSPLinkState
	var pending []billing.PendingAction
	if prepared, ok := s.catalogPreparedLinks[[2]string{product.Key, req.Key}]; s.localCatalogOnly && ok {
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
		if prepared, ok := scoped.catalogPreparedLinks[[2]string{product.Key, req.Key}]; scoped.localCatalogOnly && ok {
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
		out.PSPs = providerStates
	}
	if len(pending) > 0 {
		out.PendingManualActions = pending
	}
	return out, nil
}

// UpdatePriceRequest is the engine's price patch: nil fields are left as
// they are. PSPLinks merges into the price's links; an empty link unlinks
// its PSP. SkipRailSync keeps the change local.
type UpdatePriceRequest struct {
	Archived     *bool
	PSPLinks     map[string]map[string]string
	SkipRailSync bool
}

// pricePatch reads a merge patch: a PSP link set to null is unlinked.
func pricePatch(p billing.UpdatePriceParams) (UpdatePriceRequest, error) {
	var req UpdatePriceRequest
	if p.Archived.Null {
		return req, apperr.Invalidf("archived cannot be null")
	}
	if p.Archived.Set {
		req.Archived = &p.Archived.Value
	}
	if p.PSPLinks != nil {
		req.PSPLinks = make(map[string]map[string]string, len(p.PSPLinks))
		for psp, link := range p.PSPLinks {
			if link.Null || len(link.Value) == 0 {
				req.PSPLinks[psp] = map[string]string{}
				continue
			}
			req.PSPLinks[psp] = link.Value
		}
	}
	return req, nil
}

// UpdatePrice applies a merge patch to a price.
func (s *Service) UpdatePrice(ctx context.Context, id billing.PriceID, params billing.UpdatePriceParams) (*billing.Price, error) {
	req, err := pricePatch(params)
	if err != nil {
		return nil, err
	}
	return s.patchPrice(ctx, id, req)
}

func (s *Service) patchPrice(ctx context.Context, id billing.PriceID, req UpdatePriceRequest) (*billing.Price, error) {
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

func (s *Service) updatePrice(ctx context.Context, id billing.PriceID, req UpdatePriceRequest) (*billing.Price, error) {

	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("price_id required")
	}
	priceID := id.UUID()
	// Declarative PSP link rotation: the supplied entries merge into the
	// existing links; an empty link clears its PSP.
	existing, getErr := prices.GetByID(ctx, priceID)
	if getErr != nil {
		return nil, priceLookup(getErr)
	}
	if existing.CustomerAmount != nil {
		for _, link := range req.PSPLinks {
			if len(link) > 0 {
				return nil, apperr.Invalidf("customer_amount uses checkout amounts, not provider catalog links")
			}
		}
	}
	var preparedProduct *models.Product
	var next map[string]map[string]string
	var pending []billing.PendingAction
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
		next = cloneRails(existing.PSPLinks)
		if next == nil {
			next = map[string]map[string]string{}
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
				if template.PSP == "" {
					template.PSP = rail
				}
				pending = append(pending, template)
				// The rotation did NOT take effect. Keep the previously stored
				// (verified) link while the response reports a pending action.
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

// priceToCatalogPrice maps the DB row into the response shape, including its
// state on each linked PSP. SyncStatus is unknown until a verifying read or a
// reconciliation fills it.
func priceToCatalogPrice(p *models.Price) *billing.Price {
	v := p.View()
	return &v
}

func validateCatalogPriceTerms(req billing.CreatePriceParams) error {
	if req.CustomerAmount != nil {
		if req.UnitAmount != 0 || req.BillingIntervalHours != nil || req.AccessDurationHours != nil || req.TrialUnitAmount != nil || req.TrialDurationHours != nil {
			return apperr.Invalidf("customer_amount requires a one-off price with unit_amount zero and no access or trial duration")
		}
		if req.CustomerAmount.MinAmount <= 0 || req.CustomerAmount.MaxAmount < req.CustomerAmount.MinAmount {
			return apperr.Invalidf("customer_amount requires positive inclusive min_amount and max_amount")
		}
		for _, amount := range []int64{req.CustomerAmount.MinAmount, req.CustomerAmount.MaxAmount} {
			if _, err := moneyutil.NativeToRailMinorExact(req.Currency, amount); err != nil {
				return apperr.Invalidf("customer_amount: %v", err)
			}
		}
	}
	if req.UnitAmount < 0 {
		return apperr.Invalidf("unit_amount must be non-negative")
	}
	if req.Currency == "" {
		return apperr.Invalidf("currency required")
	}
	// Access and billing are independent. Every finite value must fit the time
	// arithmetic used for access grants and scheduled billing.
	for _, duration := range []struct {
		name  string
		hours *int
	}{
		{"access_duration_hours", req.AccessDurationHours},
		{"billing_interval_hours", req.BillingIntervalHours},
		{"trial_duration_hours", req.TrialDurationHours},
	} {
		if duration.hours != nil && (*duration.hours <= 0 || *duration.hours > catalogwire.MaxDurationHours) {
			return apperr.Invalidf("%s must be between 1 and %d hours or null", duration.name, catalogwire.MaxDurationHours)
		}
	}
	// #622 trial: both-or-neither; non-negative amount (0 = free trial); positive
	// period; only on a recurring price (there is a "then recurring" part).
	if (req.TrialUnitAmount == nil) != (req.TrialDurationHours == nil) {
		return apperr.Invalidf("trial_unit_amount and trial_duration_hours must be set together")
	}
	if req.TrialUnitAmount != nil {
		if *req.TrialUnitAmount < 0 {
			return apperr.Invalidf("trial_unit_amount must be >= 0 (0 = free trial)")
		}
		if req.BillingIntervalHours == nil {
			return apperr.Invalidf("trial pricing requires billing_interval_hours")
		}
	}
	if err := moneyutil.ValidateCurrency(req.Currency); err != nil {
		return apperr.Invalidf("%v", err)
	}

	return nil
}

// samePriceTerms compares stored terms instead of assuming how an imported ID
// was minted. Financial identity includes the product-local key.
func samePriceTerms(p billing.Price, req billing.CreatePriceParams) bool {
	return p.ProductID == req.ProductID && p.Key == req.Key && p.UnitAmount == req.UnitAmount && strings.EqualFold(p.Currency, req.Currency) &&
		reflect.DeepEqual(p.BillingIntervalHours, req.BillingIntervalHours) && reflect.DeepEqual(p.AccessDurationHours, req.AccessDurationHours) &&
		reflect.DeepEqual(p.TrialUnitAmount, req.TrialUnitAmount) && reflect.DeepEqual(p.TrialDurationHours, req.TrialDurationHours) && reflect.DeepEqual(p.CustomerAmount, req.CustomerAmount)
}
