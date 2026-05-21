package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
)

// Issue #208 declarative-provider primitives.
//
// This file defines the small interface every catalog provider implements, the
// dispatch helpers that route a CreatePrice/UpdatePrice/Verify call across the
// configured providers, and the shared types those calls speak (mutableUpdate,
// priceVerifyContext, PendingAction).
//
// Each provider lives in its own catalog_provider_<name>.go file and contributes
// one implementation of providerAdapter.

// PendingAction describes a manual step the operator must complete to bring a
// pending_manual_link provider to linked status. Surfaced on CreatePrice and on
// GetPrice/Reconcile responses when at least one provider is still pending.
type PendingAction struct {
	Provider      string                                  `json:"provider"`
	Action        string                                  `json:"action"`
	Hint          string                                  `json:"hint"`
	PatchRequired map[string]map[string]map[string]string `json:"patch_required,omitempty"`
}

// providerLookupKey is the conventional key under which an adapter stores its
// canonical lookup key on the processors[provider] map (when one exists).
// Stripe is the canonical example; other providers may or may not populate it.
const providerLookupKey = "lookup_key"

// errPendingManualLink is the sentinel an adapter returns from AutoCreate when
// it cannot mint a remote object on its own. The dispatcher catches it and
// converts the provider's slot to a pending_manual_link status (with the
// adapter's PendingAction template) rather than failing the whole call.
var errPendingManualLink = errors.New("provider requires a manual link")

// mutableUpdate carries the post-create mutable fields that Update propagates
// to attached providers (active flag). Currency / amount / billing cycle are
// immutable in OpenRails so they're not represented here.
type mutableUpdate struct {
	IsActive *bool
}

// priceVerifyContext is the OpenRails-side snapshot used by Verify to compute
// per-field drift against the remote object. Adapters compare each populated
// field on this struct against the corresponding remote field.
type priceVerifyContext struct {
	IsActive         bool
	UnitAmount       int64
	Currency         string
	BillingCycleDays *int
}

// providerAdapter is the per-provider interface dispatched by resolveProviders
// and VerifyPriceSync. Adapters are stateless; per-call state is passed in.
type providerAdapter interface {
	// Name returns the canonical provider name (e.g. "stripe", "ccbill",
	// "mobius"). Used by the dispatcher for diagnostics only — the map key in
	// providerAdapters is the source of truth.
	Name() string

	// Attach validates and stores caller-supplied link ids on a freshly created
	// OpenRails price. Returns the canonical IDs that should be written to the
	// processors[provider] map. Called when the caller provided
	// provider_links[provider] in the create/update request.
	Attach(ctx context.Context, link map[string]string) (map[string]string, error)

	// AutoCreate mints a brand-new remote object for this price and returns the
	// canonical IDs to store. Called when the caller listed this provider in
	// `providers` but did not supply pre-existing IDs. Adapters that cannot
	// auto-create return errPendingManualLink and the dispatcher converts the
	// provider's slot to pending_manual_link with the adapter's
	// PendingAction template (see PendingActionTemplate).
	AutoCreate(ctx context.Context, ctxData autoCreateContext) (map[string]string, error)

	// Verify performs a live retrieve against the remote object and computes
	// drift vs. the OpenRails-side snapshot. Returns drift fields (empty when
	// in sync), missing=true when the remote object 404s, and any transport /
	// adapter-specific error. Adapters without a read API return
	// (nil, false, nil) to signal sync_disabled (the dispatcher maps this to
	// SyncStatusSyncDisabled).
	Verify(ctx context.Context, ids map[string]string, local *priceVerifyContext) (drift []DriftField, missing bool, err error)

	// Update propagates mutable fields to the remote object. Adapters without a
	// write API return nil (no-op).
	Update(ctx context.Context, ids map[string]string, mutable mutableUpdate) error

	// PendingActionTemplate returns the manual-action surface for this adapter
	// when AutoCreate is not supported. Called by the dispatcher to populate
	// the pending_manual_link state + the response's pending_manual_actions
	// list. Returning a zero PendingAction means no template (the dispatcher
	// emits a generic "supply link via PATCH" hint).
	PendingActionTemplate(priceID uuid.UUID) PendingAction
}

// autoCreateContext is the input to AutoCreate. It carries enough OpenRails-side
// context for an adapter to mint a coherent remote object — money fields, the
// OpenRails price/product IDs (for metadata stamping and product-derived labels),
// and the optional lookup_key.
type autoCreateContext struct {
	PriceID   uuid.UUID
	ProductID uuid.UUID
	Product   *models.Product
	// ProductSlug / PriceSlug are the CONTENT identity of this price. They (not
	// the row UUIDs) drive the deterministic Stripe lookup_key + the metadata
	// content keys, so find-or-create survives a DB rebuild.
	ProductSlug      string
	PriceSlug        string
	UnitAmount       int64
	Currency         string
	BillingCycleDays *int
	LookupKey        string
}

// providerAdapters returns the dispatch table keyed by canonical provider name.
// Adapters are cheap value types holding a reference to the runtime; we build a
// fresh map per call rather than caching on Service to keep tests
// straightforward.
func (s *Service) providerAdapters() map[string]providerAdapter {
	return map[string]providerAdapter{
		"stripe": &stripeAdapter{svc: s},
		"ccbill": &ccbillAdapter{},
		"mobius": &mobiusAdapter{svc: s},
	}
}

// internalStripeLookupKey is the deterministic Stripe lookup_key OpenRails
// assigns to every price it auto-creates. It is content-addressed — derived
// from the OpenRails product slug + price slug, NOT the row UUIDs — so it is
// stable across DB rebuilds, collision-free within an account, and
// reconstructable without the caller supplying anything. Dots are safe because
// slugs are constrained to [a-z0-9-]. Callers never pass a lookup_key.
//
// Format: "openrails.<product_slug>.<price_slug>".
func internalStripeLookupKey(productSlug, priceSlug string) string {
	return "openrails." + strings.TrimSpace(productSlug) + "." + strings.TrimSpace(priceSlug)
}

// openRailsPriceContentKey is the symmetric price content key stamped on the
// Stripe Price's metadata (StripeMetadataOpenRailsPriceKey):
// "<product_slug>.<price_slug>".
func openRailsPriceContentKey(productSlug, priceSlug string) string {
	return strings.TrimSpace(productSlug) + "." + strings.TrimSpace(priceSlug)
}

// resolveProviders walks the declared `providers` + `provider_links` from a
// CreatePrice request through every adapter, dispatches Attach / AutoCreate as
// appropriate, and returns the raw processors map to persist, the typed
// per-provider response states, and any aggregated pending-manual-action items.
//
// A pending_manual_link result is NOT an error: the price is still created in
// OpenRails with the corresponding provider slot empty, and the response carries
// a PendingAction telling the operator what to do.
//
// Unknown providers are silently dropped (the same way an unknown processor in
// processors[] was previously ignored by the catalog layer).
func (s *Service) resolveProviders(ctx context.Context, product *models.Product, req CreatePriceRequest, priceID uuid.UUID) (
	processors map[string]map[string]string,
	states map[string]ProviderState,
	pending []PendingAction,
	err error,
) {
	adapters := s.providerAdapters()

	// Build the unique attach set: union of req.Providers and the keys of
	// req.ProviderLinks. Lowercase + trimmed for stable dispatch.
	want := map[string]struct{}{}
	for _, p := range req.Providers {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		want[p] = struct{}{}
	}
	for p := range req.ProviderLinks {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		want[p] = struct{}{}
	}

	processors = map[string]map[string]string{}
	states = map[string]ProviderState{}

	// Sort for deterministic ordering in tests / pending action lists.
	names := make([]string, 0, len(want))
	for p := range want {
		names = append(names, p)
	}
	sort.Strings(names)

	for _, name := range names {
		adapter, ok := adapters[name]
		if !ok {
			// Unknown provider: silently drop. Matches pre-#208 catalog tolerance.
			continue
		}
		link := req.ProviderLinks[name]
		// Try the user-supplied attach path first.
		if len(normalizeLinkMap(link)) > 0 {
			ids, attachErr := adapter.Attach(ctx, link)
			if attachErr != nil {
				return nil, nil, nil, fmt.Errorf("%s: %w", name, attachErr)
			}
			if ids == nil {
				ids = map[string]string{}
			}
			processors[name] = ids
			states[name] = ProviderState{
				Status:     ProviderStatusLinked,
				IDs:        copyStringMap(ids),
				LookupKey:  ids[providerLookupKey],
				SyncStatus: SyncStatusUnknown,
			}
			continue
		}
		// Otherwise dispatch AutoCreate. The lookup_key + content keys are
		// derived from the catalog slugs (product + price), not the row UUIDs,
		// so re-syncing after a DB wipe finds the same Stripe objects.
		productSlug := ""
		if product != nil {
			productSlug = strings.TrimSpace(product.Slug)
		}
		priceSlug := strings.TrimSpace(req.Slug)
		ids, createErr := adapter.AutoCreate(ctx, autoCreateContext{
			PriceID:          priceID,
			ProductID:        req.ProductID,
			Product:          product,
			ProductSlug:      productSlug,
			PriceSlug:        priceSlug,
			UnitAmount:       req.UnitAmount,
			Currency:         req.Currency,
			BillingCycleDays: req.BillingCycleDays,
			LookupKey:        internalStripeLookupKey(productSlug, priceSlug),
		})
		switch {
		case errors.Is(createErr, errPendingManualLink):
			template := adapter.PendingActionTemplate(priceID)
			states[name] = ProviderState{
				Status:     ProviderStatusPendingManualLink,
				SyncStatus: SyncStatusNeverSynced,
				Message:    template.Hint,
			}
			if template.Provider == "" {
				template.Provider = name
			}
			pending = append(pending, template)
		case createErr != nil:
			return nil, nil, nil, fmt.Errorf("%s: %w", name, createErr)
		default:
			if ids == nil {
				ids = map[string]string{}
			}
			processors[name] = ids
			states[name] = ProviderState{
				Status:     ProviderStatusLinked,
				IDs:        copyStringMap(ids),
				LookupKey:  ids[providerLookupKey],
				SyncStatus: SyncStatusUnknown,
			}
		}
	}
	return processors, states, pending, nil
}

// normalizeLinkMap trims whitespace from each key/value and drops empty pairs.
// Returns nil when no entries survive (i.e. caller "supplied" link is effectively empty).
func normalizeLinkMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range in {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// copyStringMap is a tiny defensive copy so response structs don't alias the
// processors map stored on the DB row.
func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
