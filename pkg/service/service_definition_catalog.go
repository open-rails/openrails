package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

type CreditGrantCadence string

const (
	CreditGrantCadenceOnce       CreditGrantCadence = "once"
	CreditGrantCadencePerRenewal CreditGrantCadence = "per_renewal"
)

type CreditGrantSpec struct {
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
// pre-#208 stripe-specific StripeProcessorState.
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
// Replaces the pre-#208 ProcessorDriftField (Stripe-only).
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
	ID               uuid.UUID       `json:"id"`
	Slug             string          `json:"slug"`
	DisplayName      string          `json:"display_name"`
	Description      string          `json:"description"`
	EntitlementsSpec map[string]*int `json:"entitlements_spec,omitempty"`
	CreditsSpec      CreditsSpec     `json:"credits_spec,omitempty"`
	TierGroup        *string         `json:"tier_group,omitempty"`
	TierRank         int             `json:"tier_rank"`
	IsActive         bool            `json:"is_active"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type CreateProductRequest struct {
	Slug             string          `json:"slug"`
	DisplayName      string          `json:"display_name"`
	Description      string          `json:"description"`
	EntitlementsSpec map[string]*int `json:"entitlements_spec,omitempty"`
	CreditsSpec      CreditsSpec     `json:"credits_spec,omitempty"`
	TierGroup        *string         `json:"tier_group,omitempty"`
	TierRank         int             `json:"tier_rank,omitempty"`
	IsActive         *bool           `json:"is_active,omitempty"`
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
	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}
	p := &models.Product{
		ID:               uuid.New(),
		Slug:             req.Slug,
		DisplayName:      req.DisplayName,
		Description:      req.Description,
		EntitlementsSpec: req.EntitlementsSpec,
		CreditsSpec:      toModelCreditsSpec(req.CreditsSpec),
		TierGroup:        req.TierGroup,
		TierRank:         req.TierRank,
		IsActive:         active,
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
	IsActive         *bool           `json:"is_active,omitempty"`
	// SkipProcessorSync, when true, suppresses any propagation of this update to
	// configured external processors (Stripe etc.). The DB row is updated as usual.
	// Use sparingly — drift introduced this way will appear as sync_status="drifted"
	// on subsequent ?verify=true reads or reconcile actions.
	SkipProcessorSync bool `json:"skip_processor_sync,omitempty"`
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
		IsActive:         req.IsActive,
	})
	if err != nil {
		return nil, err
	}

	// Propagate mutable Product changes to Stripe (display name + description + active).
	// The Stripe product ID is not stored on the OpenRails product row itself —
	// it lives on associated prices' processors.stripe.product_id. Look up one
	// such price to find it; if no prices have a Stripe link yet, there is
	// nothing to propagate (no Stripe Product exists for this OpenRails product).
	if !req.SkipProcessorSync && (req.DisplayName != nil || req.Description != nil || req.IsActive != nil) && s.rt.Config != nil {
		stripeProductID := s.lookupStripeProductID(ctx, productID)
		if stripeProductID != "" {
			stripeSvc := &catalog.StripeCatalogService{Config: s.rt.Config}
			params := catalog.UpdateProductParams{}
			if req.DisplayName != nil {
				name := strings.TrimSpace(*req.DisplayName)
				params.Name = &name
			}
			if req.Description != nil {
				desc := strings.TrimSpace(*req.Description)
				params.Description = &desc
			}
			if req.IsActive != nil {
				active := *req.IsActive
				params.Active = &active
			}
			// Best-effort propagation: log on failure, do not roll back the DB change.
			// Drift will surface on next ?verify=true read.
			_ = stripeSvc.UpdateProduct(ctx, stripeProductID, params)
		}
	}

	return productToCatalogProduct(p), nil
}

// lookupStripeProductID returns the Stripe Product ID associated with the
// given OpenRails product by scanning its prices for processors.stripe.product_id.
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
		if id := strings.TrimSpace(p.Processors["stripe"][models.ProcessorKeyStripeProductID]); id != "" {
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
		IsActive:         p.IsActive,
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
	}
}

// CatalogPrice is the OpenRails-side view of a price. Per issue #208 the
// declarative `providers` shape is the only surface; the legacy raw
// `processors` map and the per-processor `link|create` request shape are
// removed entirely (no transitional fields, no compat helpers).
type CatalogPrice struct {
	ID               uuid.UUID `json:"id"`
	ProductID        uuid.UUID `json:"product_id"`
	DisplayName      string    `json:"display_name"`
	IsActive         bool      `json:"is_active"`
	UnitAmount       int64     `json:"unit_amount"`
	Currency         string    `json:"currency"`
	BillingCycleDays *int      `json:"billing_cycle_days,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`

	// Providers carries the typed per-provider attachment state for every
	// processor this price is linked to. Always populated when at least one
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
	ProductID        uuid.UUID `json:"product_id"`
	DisplayName      string    `json:"display_name"`
	UnitAmount       int64     `json:"unit_amount"`
	Currency         string    `json:"currency"`
	BillingCycleDays *int      `json:"billing_cycle_days,omitempty"`

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

	IsActive *bool `json:"is_active,omitempty"`

	// LookupKey, when non-empty, is set on the Stripe Price for the
	// auto-create path and used to find-or-attach to an existing Stripe Price
	// under the same key (strongly consistent lookup via Stripe's
	// list-by-lookup_keys API). Recommended format:
	// "<app_namespace>.<tier_group>.<product>.<price>.<currency>.<amount>.<interval>.<count>".
	LookupKey string `json:"lookup_key,omitempty"`
}

func (s *Service) CreatePrice(ctx context.Context, req CreatePriceRequest) (*CatalogPrice, error) {
	products, prices, err := s.requireCatalogServices()
	if err != nil {
		return nil, err
	}
	if req.ProductID == uuid.Nil {
		return nil, fmt.Errorf("product_id required")
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Currency = strings.ToLower(strings.TrimSpace(req.Currency))
	if req.DisplayName == "" {
		return nil, fmt.Errorf("display_name required")
	}
	if req.UnitAmount <= 0 {
		return nil, fmt.Errorf("unit_amount must be positive")
	}
	if req.Currency == "" {
		return nil, fmt.Errorf("currency required")
	}

	// Validate product exists.
	product, err := products.GetByID(ctx, req.ProductID)
	if err != nil {
		return nil, fmt.Errorf("product not found")
	}

	priceID := uuid.New()
	processors, providerStates, pending, err := s.resolveProviders(ctx, product, req, priceID)
	if err != nil {
		return nil, err
	}

	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}
	now := time.Now().UTC()
	price := &models.Price{
		ID:               priceID,
		ProductID:        req.ProductID,
		DisplayName:      req.DisplayName,
		IsActive:         active,
		Amount:           req.UnitAmount,
		Currency:         req.Currency,
		BillingCycleDays: req.BillingCycleDays,
		Processors:       processors,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := prices.Create(ctx, price); err != nil {
		return nil, err
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

// UpdatePriceRequest is the declarative-shape PATCH for a price (issue #208).
// The legacy `processors` / `set_processors` fields are gone; the only way to
// add or rotate provider links is via `provider_links` (partial merge into the
// existing map). To clear a provider entirely, supply an empty inner map for
// it and set ReplaceProviderLinks=true.
type UpdatePriceRequest struct {
	DisplayName *string `json:"display_name,omitempty"`

	// ProviderLinks merges per-provider link maps into the existing processors
	// map. Supply only the providers you want to add or rotate. Each map's
	// values are validated through the matching provider adapter's Attach.
	ProviderLinks map[string]map[string]string `json:"provider_links,omitempty"`

	// ReplaceProviderLinks, when true, replaces the entire processors map
	// rather than merging — useful for clearing a provider. When false (the
	// default) supplied entries are merged into the existing map and providers
	// not mentioned are left alone.
	ReplaceProviderLinks bool `json:"replace_provider_links,omitempty"`

	IsActive *bool `json:"is_active,omitempty"`

	// See UpdateProductRequest.SkipProcessorSync.
	SkipProcessorSync bool `json:"skip_processor_sync,omitempty"`
}

func (s *Service) UpdatePrice(ctx context.Context, priceID uuid.UUID, req UpdatePriceRequest) (*CatalogPrice, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if priceID == uuid.Nil {
		return nil, fmt.Errorf("price_id required")
	}
	if req.DisplayName != nil {
		if err := prices.UpdateDisplayName(ctx, priceID, strings.TrimSpace(*req.DisplayName)); err != nil {
			return nil, err
		}
	}
	// Declarative provider link rotation. ReplaceProviderLinks=true overwrites
	// the entire processors map; otherwise the supplied entries are merged
	// into the existing map (partial PATCH). Empty inner maps clear a provider.
	if req.ProviderLinks != nil {
		var next map[string]map[string]string
		if req.ReplaceProviderLinks {
			next = map[string]map[string]string{}
		} else {
			existing, getErr := prices.GetByID(ctx, priceID)
			if getErr != nil {
				return nil, getErr
			}
			next = cloneProcessors(existing.Processors)
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
			ids, attachErr := adapter.Attach(ctx, normalized)
			if attachErr != nil {
				return nil, fmt.Errorf("%s: %w", provider, attachErr)
			}
			next[provider] = ids
		}
		if err := prices.UpdateProcessors(ctx, priceID, next); err != nil {
			return nil, err
		}
	}
	if req.IsActive != nil {
		if *req.IsActive {
			if err := prices.Activate(ctx, priceID); err != nil {
				return nil, err
			}
		} else {
			if err := prices.Deactivate(ctx, priceID); err != nil {
				return nil, err
			}
		}
	}
	updated, err := prices.GetByID(ctx, priceID)
	if err != nil {
		return nil, err
	}

	// Propagate mutable changes to every attached provider via its adapter.
	// Only when the caller did not opt out via SkipProcessorSync. Failures are
	// logged-and-swallowed: drift will surface on the next ?verify=true read.
	if !req.SkipProcessorSync && (req.DisplayName != nil || req.IsActive != nil) {
		mutable := mutableUpdate{
			DisplayName: req.DisplayName,
			IsActive:    req.IsActive,
		}
		adapters := s.providerAdapters()
		for provider, ids := range updated.Processors {
			adapter, ok := adapters[strings.ToLower(strings.TrimSpace(provider))]
			if !ok {
				continue
			}
			_ = adapter.Update(ctx, ids, mutable)
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
		ID:               p.ID,
		ProductID:        p.ProductID,
		DisplayName:      p.DisplayName,
		IsActive:         p.IsActive,
		UnitAmount:       p.Amount,
		Currency:         p.Currency,
		BillingCycleDays: p.BillingCycleDays,
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
	}
	if len(p.Processors) == 0 {
		return cp
	}
	cp.Providers = make(map[string]ProviderState, len(p.Processors))
	for name, ids := range p.Processors {
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
