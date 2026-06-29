package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

type CreditGrantCadence string

const (
	CreditGrantCadenceOnce       CreditGrantCadence = "once"
	CreditGrantCadencePerRenewal CreditGrantCadence = "per_renewal"
)

type CreditGrantSpec struct {
	Unit        string             `json:"unit,omitempty"`
	Amount      int64              `json:"amount"`
	ExpiresDays *int               `json:"expires_days,omitempty"`
	Cadence     CreditGrantCadence `json:"cadence,omitempty"`
}

type CreditsSpec map[string]CreditGrantSpec

func toModelCreditsSpec(in CreditsSpec) models.CreditsSpec {
	if in == nil {
		return nil
	}
	out := make(models.CreditsSpec, len(in))
	for k, v := range in {
		cadence := models.CreditGrantCadence(v.Cadence)
		out[k] = models.CreditGrantSpec{
			Unit:        v.Unit,
			Amount:      v.Amount,
			ExpiresDays: v.ExpiresDays,
			Cadence:     cadence,
		}
	}
	return out
}

// ProviderStatus is the per-provider attachment state surfaced in admin
// responses. Issue #208 defines these four values.
type ProviderStatus string

const (
	ProviderStatusLinked            ProviderStatus = "linked"
	ProviderStatusPendingManualLink ProviderStatus = "pending_manual_link"
	ProviderStatusSyncDisabled      ProviderStatus = "sync_disabled"
	ProviderStatusError             ProviderStatus = "error"
)

// SyncStatus is the per-provider freshness/drift state. Populated only by
// paths that perform a live retrieve (?verify=true reads or reconcile);
// otherwise defaults to "unknown".
type SyncStatus string

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
type ProviderState struct {
	Status       ProviderStatus    `json:"status"`
	IDs          map[string]string `json:"ids,omitempty"`
	LookupKey    string            `json:"lookup_key,omitempty"`
	LastSyncedAt *time.Time        `json:"last_synced_at,omitempty"`
	SyncStatus   SyncStatus        `json:"sync_status,omitempty"`
	Drift        []DriftField      `json:"drift,omitempty"`
	Message      string            `json:"message,omitempty"`
}

// DriftField describes a single divergent field discovered by verify/reconcile.
// Replaces the pre-#208 RailDriftField (Stripe-only).
type DriftField struct {
	Field          string `json:"field"`
	OpenRailsValue string `json:"openrails_value"`
	RemoteValue    string `json:"remote_value"`
}

// CatalogProduct is the OpenRails-side view of a product. Products are pure
// OpenRails concepts and have NO direct provider linkage in the user-facing
// shape — provider state lives on CatalogPrice (issue #208).
//
// The Stripe Product ID some prices carry is purely an artifact of Stripe's
// requirement that every Stripe Price attach to a Stripe Product; it is
// denormalized onto price rows and managed implicitly by price-level
// operations. There is no product-level provider field, no product-level
// verify/reconcile, no product-level reconcile route.
type CatalogProduct struct {
	ID               uuid.UUID            `json:"id"`
	Slug             string               `json:"slug"`
	DisplayName      string               `json:"display_name"`
	Description      string               `json:"description"`
	EntitlementsSpec map[string]*int      `json:"entitlements_spec,omitempty"`
	CreditsSpec      CreditsSpec          `json:"credits_spec,omitempty"`
	TierGroup        *string              `json:"tier_group,omitempty"`
	TierRank         int                  `json:"tier_rank"`
	Status           models.CatalogStatus `json:"status"`
	CreatedAt        time.Time            `json:"created_at"`
	UpdatedAt        time.Time            `json:"updated_at"`
}

type CreateProductRequest struct {
	Slug             string          `json:"slug"`
	DisplayName      string          `json:"display_name"`
	Description      string          `json:"description"`
	EntitlementsSpec map[string]*int `json:"entitlements_spec,omitempty"`
	CreditsSpec      CreditsSpec     `json:"credits_spec,omitempty"`
	TierGroup        *string         `json:"tier_group,omitempty"`
	TierRank         int             `json:"tier_rank,omitempty"`
	// Status is the optional initial lifecycle state (draft|active|archived).
	// Defaults to active. Creating archived directly supports migrating
	// historical plans that already have subscribers (no purchasable gap).
	Status models.CatalogStatus `json:"status,omitempty"`
}

func (s *Service) CreateProduct(ctx context.Context, req CreateProductRequest) (*CatalogProduct, error) {
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	req.Slug = strings.TrimSpace(req.Slug)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Description = strings.TrimSpace(req.Description)
	if req.Slug == "" {
		return nil, fmt.Errorf("slug required")
	}
	if req.DisplayName == "" {
		return nil, fmt.Errorf("display_name required")
	}

	now := time.Now().UTC()
	status := req.Status
	if status == "" {
		status = models.CatalogStatusActive
	}
	if !status.Valid() {
		return nil, fmt.Errorf("invalid status %q", status)
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	p := &models.Product{
		ID:               uuidutil.NewV7(),
		MerchantID:       tid.UUID(),
		Slug:             req.Slug,
		DisplayName:      req.DisplayName,
		Description:      req.Description,
		EntitlementsSpec: req.EntitlementsSpec,
		CreditsSpec:      toModelCreditsSpec(req.CreditsSpec),
		TierGroup:        req.TierGroup,
		TierRank:         req.TierRank,
		Status:           status,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := products.Create(ctx, p); err != nil {
		return nil, err
	}
	return productToCatalogProduct(p), nil
}

type UpdateProductRequest struct {
	DisplayName      *string         `json:"display_name,omitempty"`
	Description      *string         `json:"description,omitempty"`
	EntitlementsSpec map[string]*int `json:"entitlements_spec,omitempty"`
	SetEntitlements  bool            `json:"set_entitlements,omitempty"`
	CreditsSpec      CreditsSpec     `json:"credits_spec,omitempty"`
	SetCredits       bool            `json:"set_credits,omitempty"`
	TierGroup        *string         `json:"tier_group,omitempty"`
	SetTierGroup     bool            `json:"set_tier_group,omitempty"`
	TierRank         *int            `json:"tier_rank,omitempty"`
	// Status sets the lifecycle state (draft|active|archived). archived/draft
	// propagate to Stripe as active=false; active propagates as active=true.
	Status *models.CatalogStatus `json:"status,omitempty"`
	// SkipRailSync, when true, suppresses any propagation of this update to
	// configured external rails (Stripe etc.). The DB row is updated as usual.
	// Use sparingly — drift introduced this way will appear as sync_status="drifted"
	// on subsequent ?verify=true reads or reconcile actions.
	SkipRailSync bool `json:"skip_rail_sync,omitempty"`
}

func (s *Service) UpdateProduct(ctx context.Context, productID uuid.UUID, req UpdateProductRequest) (*CatalogProduct, error) {
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if productID == uuid.Nil {
		return nil, fmt.Errorf("product_id required")
	}
	p, err := products.UpdateDefinition(ctx, productID, catalog.ProductDefinitionUpdateParams{
		DisplayName:      req.DisplayName,
		Description:      req.Description,
		EntitlementsSpec: req.EntitlementsSpec,
		SetEntitlements:  req.SetEntitlements,
		CreditsSpec:      toModelCreditsSpec(req.CreditsSpec),
		SetCredits:       req.SetCredits,
		TierGroup:        req.TierGroup,
		SetTierGroup:     req.SetTierGroup,
		TierRank:         req.TierRank,
		Status:           req.Status,
	})
	if err != nil {
		return nil, err
	}

	// Propagate mutable Product changes to Stripe (display name + description + active).
	// The Stripe product ID is not stored on the OpenRails product row itself —
	// it lives on associated prices' rails.stripe.product_id. Look up one
	// such price to find it; if no prices have a Stripe link yet, there is
	// nothing to propagate (no Stripe Product exists for this OpenRails product).
	if !req.SkipRailSync && (req.DisplayName != nil || req.Description != nil || req.Status != nil) && s.rt.Config != nil {
		stripeProductID := s.lookupStripeProductID(ctx, productID)
		if stripeProductID != "" {
			stripeSvc := &catalog.StripeCatalogService{Config: s.rt.Config, Rails: s.rt.Rails}
			params := catalog.UpdateProductParams{}
			if req.DisplayName != nil {
				name := strings.TrimSpace(*req.DisplayName)
				params.Name = &name
			}
			if req.Description != nil {
				desc := strings.TrimSpace(*req.Description)
				params.Description = &desc
			}
			if req.Status != nil {
				// active -> Stripe active=true; draft/archived -> active=false.
				active := *req.Status == models.CatalogStatusActive
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
	if !req.SkipRailSync && req.SetEntitlements && s.rt.Config != nil {
		if stripeProductID := s.lookupStripeProductID(ctx, productID); stripeProductID != "" {
			stripeSvc := &catalog.StripeCatalogService{Config: s.rt.Config, Rails: s.rt.Rails}
			keys := make([]string, 0, len(p.EntitlementsSpec))
			for k := range p.EntitlementsSpec {
				keys = append(keys, k)
			}
			_ = stripeSvc.SyncProductFeatures(ctx, stripeProductID, keys)
		}
	}

	return productToCatalogProduct(p), nil
}

// propagateProductActiveToStripe pushes the product's active flag to its Stripe
// Product, if one exists. Used by the lifecycle paths (activate / deactivate /
// SetProductStatus) so a status change reaches Stripe — UpdateProduct already
// handles the display_name/description/active propagation for definition edits,
// but the dedicated lifecycle entrypoints bypass it. Best-effort: failures are
// swallowed (drift surfaces on the next product reconcile).
func (s *Service) propagateProductActiveToStripe(ctx context.Context, productID uuid.UUID, active bool) {
	if s.rt == nil || s.rt.Config == nil {
		return
	}
	stripeProductID := s.lookupStripeProductID(ctx, productID)
	if stripeProductID == "" {
		return
	}
	stripeSvc := &catalog.StripeCatalogService{Config: s.rt.Config, Rails: s.rt.Rails}
	a := active
	_ = stripeSvc.UpdateProduct(ctx, stripeProductID, catalog.UpdateProductParams{Active: &a})
}

// lookupStripeProductID returns the Stripe Product ID associated with the
// given OpenRails product by scanning its prices for rails.stripe.product_id.
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
		if id := strings.TrimSpace(p.Rails["stripe"][models.RailKeyStripeProductID]); id != "" {
			return id
		}
	}
	return ""
}

func productToCatalogProduct(p *models.Product) *CatalogProduct {
	var credits CreditsSpec
	if len(p.CreditsSpec) > 0 {
		credits = make(CreditsSpec, len(p.CreditsSpec))
		for k, v := range p.CreditsSpec {
			credits[k] = CreditGrantSpec{
				Unit:        v.Unit,
				Amount:      v.Amount,
				ExpiresDays: v.ExpiresDays,
				Cadence:     CreditGrantCadence(v.Cadence),
			}
		}
	}
	return &CatalogProduct{
		ID:               p.ID,
		Slug:             p.Slug,
		DisplayName:      p.DisplayName,
		Description:      p.Description,
		EntitlementsSpec: p.EntitlementsSpec,
		CreditsSpec:      credits,
		TierGroup:        p.TierGroup,
		TierRank:         p.TierRank,
		Status:           p.Status,
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
	}
}

// CatalogPrice is the OpenRails-side view of a price. The declarative
// `providers` shape is the only rail configuration surface.
type CatalogPrice struct {
	ID                uuid.UUID            `json:"id"`
	ProductID         uuid.UUID            `json:"product_id"`
	Status            models.CatalogStatus `json:"status"`
	UnitAmount        int64                `json:"unit_amount"`
	Currency          string               `json:"currency"`
	AccessDurationHours *int               `json:"access_duration_hours,omitempty"`
	AutoRenew          bool                `json:"auto_renew"`
	TrialUnitAmount    *int64              `json:"trial_unit_amount,omitempty"`
	TrialDurationHours *int                `json:"trial_duration_hours,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
	UpdatedAt          time.Time           `json:"updated_at"`

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
type CreatePriceRequest struct {
	ProductID uuid.UUID `json:"product_id"`
	// A price's identity IS its financial substance — the product slug plus
	// these immutable money terms. There is no price slug: the content-based
	// provider keys are derived from (product_slug, currency, unit_amount,
	// billing_cycle_days), so they are stable across DB rebuilds and a different
	// amount is, by construction, a different price.
	UnitAmount int64  `json:"unit_amount"`
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
	TrialUnitAmount    *int64 `json:"trial_unit_amount,omitempty"`
	TrialDurationHours *int   `json:"trial_duration_hours,omitempty"`

	// Providers is the list of provider names to attach (e.g. ["stripe",
	// "ccbill", "mobius"]). Empty means "DB-only price with no external
	// links" — useful for testing or for prices that are not sold externally.
	Providers []string `json:"providers,omitempty"`

	// ProviderLinks maps provider name -> pre-existing link key/value pairs.
	// Schema is per-provider:
	//   stripe : {"price_id": "price_xxx", "product_id": "prod_xxx" (optional)}
	//   ccbill : {"form_name": "...", "flex_id": "..."}
	//   mobius : {"plan_id": "..."}
	// Any provider with a non-empty link here is implicitly added to the
	// attach set even if absent from Providers.
	ProviderLinks map[string]map[string]string `json:"provider_links,omitempty"`

	// Status is the optional initial lifecycle state (draft|active|archived).
	// Defaults to active. draft prices are not created in the external provider;
	// archived can be created directly to migrate a historical plan in one step.
	Status models.CatalogStatus `json:"status,omitempty"`
}

// RecurringCycleDays returns the recurring billing cadence in WHOLE DAYS for an
// auto-renewing request, or nil for a one-off/durable price (#622). The window
// is in hours; providers bill in days, so the cadence is hours/24.
func (req CreatePriceRequest) RecurringCycleDays() *int {
	if !req.AutoRenew || req.AccessDurationHours == nil {
		return nil
	}
	days := *req.AccessDurationHours / 24
	return &days
}

func (s *Service) CreatePrice(ctx context.Context, req CreatePriceRequest) (*CatalogPrice, error) {
	products, prices, err := s.requireCatalogServices()
	if err != nil {
		return nil, err
	}
	if req.ProductID == uuid.Nil {
		return nil, fmt.Errorf("product_id required")
	}
	req.Currency = strings.TrimSpace(req.Currency)
	if req.UnitAmount < 0 {
		return nil, fmt.Errorf("unit_amount must be non-negative")
	}
	if req.Currency == "" {
		return nil, fmt.Errorf("currency required")
	}
	// #622 access window: a finite window must be positive; auto_renew needs one.
	if req.AccessDurationHours != nil && *req.AccessDurationHours <= 0 {
		return nil, fmt.Errorf("access_duration_hours must be positive (omit for indefinite)")
	}
	if req.AutoRenew && req.AccessDurationHours == nil {
		return nil, fmt.Errorf("auto_renew requires a finite access_duration_hours")
	}
	// #622 trial: both-or-neither; non-negative amount (0 = free trial); positive
	// period; only on an auto-renewing price (there is a "then recurring" part).
	if (req.TrialUnitAmount == nil) != (req.TrialDurationHours == nil) {
		return nil, fmt.Errorf("trial_unit_amount and trial_duration_hours must be set together")
	}
	if req.TrialUnitAmount != nil {
		if *req.TrialUnitAmount < 0 {
			return nil, fmt.Errorf("trial_unit_amount must be >= 0 (0 = free trial)")
		}
		if *req.TrialDurationHours <= 0 {
			return nil, fmt.Errorf("trial_duration_hours must be positive")
		}
		if !req.AutoRenew {
			return nil, fmt.Errorf("trial pricing requires auto_renew")
		}
	}
	if err := money.ValidateCurrency(req.Currency); err != nil {
		return nil, err
	}
	req.Currency = strings.ToLower(req.Currency)

	// Validate product exists.
	product, err := products.GetByID(ctx, req.ProductID)
	if err != nil {
		return nil, fmt.Errorf("product not found")
	}

	status := req.Status
	if status == "" {
		status = models.CatalogStatusActive
	}
	if !status.Valid() {
		return nil, fmt.Errorf("invalid status %q", status)
	}

	priceID := uuidutil.NewV7()

	// Draft prices are not created in any external provider — they have no
	// subscribers and are not purchasable, so there is nothing to mint remotely.
	var (
		rails          map[string]map[string]string
		providerStates map[string]ProviderState
		pending        []PendingAction
	)
	if status != models.CatalogStatusDraft {
		rails, providerStates, pending, err = s.resolveProviders(ctx, product, req, priceID)
		if err != nil {
			return nil, err
		}
	}

	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	price := &models.Price{
		ID:                priceID,
		MerchantID:        tid.UUID(),
		ProductID:         req.ProductID,
		Status:            status,
		Amount:            req.UnitAmount,
		Currency:          req.Currency,
		AccessDurationHours: req.AccessDurationHours,
		AutoRenew:          req.AutoRenew,
		TrialUnitAmount:    req.TrialUnitAmount,
		TrialDurationHours: req.TrialDurationHours,
		Rails:              rails,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := prices.Create(ctx, price); err != nil {
		return nil, err
	}
	// Created-as-archived: the providers were auto-created active above, so
	// propagate active=false to match the archived lifecycle (best-effort; drift
	// surfaces on next verify if a provider rejects it).
	if status == models.CatalogStatusArchived && len(rails) > 0 && !s.catalogRemoteWritesDisabled() {
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
// provider links via `provider_links` (partial merge into the
// existing map). To clear a provider entirely, supply an empty inner map for
// it and set ReplaceProviderLinks=true.
type UpdatePriceRequest struct {
	// ProviderLinks merges per-provider link maps into the existing rails
	// map. Supply only the providers you want to add or rotate. Each map's
	// values are validated through the matching provider adapter's Attach.
	ProviderLinks map[string]map[string]string `json:"provider_links,omitempty"`

	// ReplaceProviderLinks, when true, replaces the entire rails map
	// rather than merging — useful for clearing a provider. When false (the
	// default) supplied entries are merged into the existing map and providers
	// not mentioned are left alone.
	ReplaceProviderLinks bool `json:"replace_provider_links,omitempty"`

	// Status sets the lifecycle state (draft|active|archived). active propagates
	// to providers as active=true; draft/archived as active=false.
	Status *models.CatalogStatus `json:"status,omitempty"`

	// See UpdateProductRequest.SkipRailSync.
	SkipRailSync bool `json:"skip_rail_sync,omitempty"`
}

func (s *Service) UpdatePrice(ctx context.Context, priceID uuid.UUID, req UpdatePriceRequest) (*CatalogPrice, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if priceID == uuid.Nil {
		return nil, fmt.Errorf("price_id required")
	}
	// Declarative provider link rotation. ReplaceProviderLinks=true overwrites
	// the entire rails map; otherwise the supplied entries are merged
	// into the existing map (partial PATCH). Empty inner maps clear a provider.
	if req.ProviderLinks != nil {
		// The existing price + its product give the substance (slug + money terms)
		// each adapter's Attach validates the supplied link against. Fetch it
		// regardless of merge/replace so a rotated link is verified, not blindly
		// stored.
		existing, getErr := prices.GetByID(ctx, priceID)
		if getErr != nil {
			return nil, getErr
		}
		pctx, ctxErr := s.priceLinkContext(ctx, existing)
		if ctxErr != nil {
			return nil, ctxErr
		}
		var next map[string]map[string]string
		if req.ReplaceProviderLinks {
			next = map[string]map[string]string{}
		} else {
			next = cloneRails(existing.Rails)
			if next == nil {
				next = map[string]map[string]string{}
			}
		}
		adapters := s.providerAdapters()
		for provider, link := range req.ProviderLinks {
			provider = strings.ToLower(strings.TrimSpace(provider))
			if provider == "" {
				continue
			}
			normalized := normalizeLinkMap(link)
			// Empty link map = clear this provider (only on merge; replace
			// already starts empty).
			if len(normalized) == 0 {
				delete(next, provider)
				continue
			}
			adapter, ok := adapters[provider]
			if !ok {
				// Unknown provider: store raw, same as resolveProviders tolerance.
				next[provider] = normalized
				continue
			}
			ids, attachErr := adapter.Attach(ctx, normalized, pctx)
			if attachErr != nil {
				return nil, fmt.Errorf("%s: %w", provider, attachErr)
			}
			next[provider] = ids
		}
		if err := prices.UpdateRails(ctx, priceID, next); err != nil {
			return nil, err
		}
	}
	if req.Status != nil {
		if !req.Status.Valid() {
			return nil, fmt.Errorf("invalid status %q", *req.Status)
		}
		if err := prices.SetStatus(ctx, priceID, *req.Status); err != nil {
			return nil, err
		}
	}
	updated, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, err
	}

	// Propagate mutable changes to every attached provider via its adapter.
	// Only when the caller did not opt out via SkipRailSync. Failures are
	// logged-and-swallowed: drift will surface on the next ?verify=true read.
	if !req.SkipRailSync && req.Status != nil {
		mutable := mutableUpdate{}
		if req.Status != nil {
			active := *req.Status == models.CatalogStatusActive
			mutable.IsActive = &active
		}
		if !s.catalogRemoteWritesDisabled() {
			adapters := s.providerAdapters()
			for provider, ids := range updated.Rails {
				adapter, ok := adapters[strings.ToLower(strings.TrimSpace(provider))]
				if !ok {
					continue
				}
				_ = adapter.Update(ctx, ids, mutable)
			}
		}
	}

	return priceToCatalogPrice(updated), nil
}

// priceToCatalogPrice maps the DB row into the response shape, including the
// per-provider Providers map. SyncStatus defaults to "unknown" — only paths
// that perform a live retrieve (?verify=true, reconcile) populate richer
// values.
func priceToCatalogPrice(p *models.Price) *CatalogPrice {
	cp := &CatalogPrice{
		ID:                p.ID,
		ProductID:         p.ProductID,
		Status:            p.Status,
		UnitAmount:        p.Amount,
		Currency:          p.Currency,
		AccessDurationHours: p.AccessDurationHours,
		AutoRenew:          p.AutoRenew,
		TrialUnitAmount:    p.TrialUnitAmount,
		TrialDurationHours: p.TrialDurationHours,
		CreatedAt:          p.CreatedAt,
		UpdatedAt:          p.UpdatedAt,
	}
	if len(p.Rails) == 0 {
		return cp
	}
	cp.Providers = make(map[string]ProviderState, len(p.Rails))
	for name, ids := range p.Rails {
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
