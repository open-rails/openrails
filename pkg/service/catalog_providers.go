package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
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

// errRemoteWritesDisabled is the sentinel adapters return from a write point
// (find-or-create inside Attach, AutoCreate) when catalog provider writes are
// blocked by the operating mode (mode=limited/readonly, #346). The dispatcher
// converts it to pending_manual_link — the price still applies locally and the
// provider slot converges on a later apply once writes are allowed (the
// bootstrap manifest re-applies on every boot). Verification reads always run.
var errRemoteWritesDisabled = errors.New("catalog provider writes are disabled (mode=limited/readonly)")

// remoteWritesDisabledMessage is the pending_manual_link message used when the
// operating mode (not a missing capability) deferred the provider write.
const remoteWritesDisabledMessage = "provider writes disabled (mode=limited/readonly): remote object creation deferred; re-apply once writes are allowed"

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
	//
	// in carries the OpenRails price substance (slug + immutable money terms) so
	// the adapter can VERIFY the linked remote object actually exists AND matches
	// that substance (amount / currency / billing cycle / provider-specific
	// immutable terms). A missing or mismatched remote object is a loud error —
	// the operator linked the wrong id — not a silent accept. Adapters whose
	// provider has no read API (CCBill) store the supplied ids as-is.
	Attach(ctx context.Context, link map[string]string, in autoCreateContext) (map[string]string, error)

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
	// ProductSlug is the product's stable identity. Together with the immutable
	// money terms (UnitAmount / Currency / BillingCycleDays) it forms the CONTENT
	// identity of this price, which drives the deterministic Stripe lookup_key +
	// the metadata content keys — so find-or-create survives a DB rebuild that
	// regenerates row UUIDs.
	ProductSlug      string
	UnitAmount       int64
	Currency         string
	BillingCycleDays *int
	LookupKey        string

	// RemoteWritesDisabled tells adapters that catalog provider WRITES are
	// blocked by the operating mode (mode=limited/readonly, #346). Adapters
	// must return errRemoteWritesDisabled at their write points (creating a
	// missing object inside Attach, AutoCreate) and still perform read-only
	// verification normally.
	RemoteWritesDisabled bool
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
		"solana": &solanaAdapter{svc: s},
	}
}

// priceCycleToken renders the billing cycle component of a price content key.
// A recurring price (cycle > 0) renders the day count; a one-time price (nil or
// 0 cycle) renders the literal "onetime". This keeps the key unambiguous and
// human-readable while still distinguishing one_time from recurring.
func priceCycleToken(billingCycleDays *int) string {
	if billingCycleDays != nil && *billingCycleDays > 0 {
		return strconv.Itoa(*billingCycleDays)
	}
	return "onetime"
}

// openRailsPriceContentKey is the content key derived from a price's FINANCIAL
// SUBSTANCE — the product's stable slug plus the immutable money terms
// (currency, unit amount, billing cycle). It is NOT derived from any row UUID,
// so it survives a DB wipe + resync: re-seeding the same product+price terms
// reproduces byte-identical keys and re-attaches to the existing provider
// objects rather than duplicating them.
//
// Because the amount lives IN the key, a different amount yields a different key
// and therefore a different price — a price change is modeled as create-new +
// archive-old, never an in-place mutation/transfer.
//
// Format: "<product_slug>.<currency>.<unit_amount>.<cycle>" where <cycle> is the
// billing_cycle_days for a recurring price or "onetime" for a one-time price.
// Example: "pro.usd.2900.30" or "pro.usd.500.onetime".
// Canonical implementation: catalog.OpenRailsPriceContentKey (shared with the
// #358 archive-intent relevance checks).
func openRailsPriceContentKey(productSlug, currency string, unitAmount int64, billingCycleDays *int) string {
	return catalog.OpenRailsPriceContentKey(productSlug, currency, unitAmount, billingCycleDays)
}

// internalStripeLookupKey is the deterministic Stripe lookup_key OpenRails
// assigns to every price it auto-creates. It is the price content key prefixed
// with "openrails." so OpenRails-owned prices are unmistakable on the Stripe
// account. Content-addressed (see openRailsPriceContentKey), so it is stable
// across DB rebuilds and reconstructable without the caller supplying anything.
//
// Format: "openrails.<product_slug>.<currency>.<unit_amount>.<cycle>".
// Example: "openrails.pro.usd.2900.30".
func internalStripeLookupKey(productSlug, currency string, unitAmount int64, billingCycleDays *int) string {
	return "openrails." + openRailsPriceContentKey(productSlug, currency, unitAmount, billingCycleDays)
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

	// The price substance (slug + immutable money terms) is the same for the
	// Attach (link-validation) and AutoCreate (mint) paths. lookup_key + content
	// keys are derived from the product slug + money terms, not the row UUIDs, so
	// re-syncing after a DB wipe finds the same provider objects.
	productSlug := ""
	if product != nil {
		productSlug = strings.TrimSpace(product.Slug)
	}
	remoteWritesDisabled := s.rt != nil && s.rt.Config != nil && s.rt.Config.IsLimitedMode()
	pctx := autoCreateContext{
		PriceID:              priceID,
		ProductID:            req.ProductID,
		Product:              product,
		ProductSlug:          productSlug,
		UnitAmount:           req.UnitAmount,
		Currency:             req.Currency,
		BillingCycleDays:     req.BillingCycleDays,
		LookupKey:            internalStripeLookupKey(productSlug, req.Currency, req.UnitAmount, req.BillingCycleDays),
		RemoteWritesDisabled: remoteWritesDisabled,
	}

	for _, name := range names {
		adapter, ok := adapters[name]
		if !ok {
			// Unknown provider: silently drop. Matches pre-#208 catalog tolerance.
			continue
		}
		link := req.ProviderLinks[name]
		// Try the user-supplied attach path first. Attach VERIFIES the linked
		// remote object exists and matches the price substance (pctx) — a wrong
		// or missing link is a loud error, never a silent accept — and no provider
		// object is created when a valid link is supplied.
		if len(normalizeLinkMap(link)) > 0 {
			ids, attachErr := adapter.Attach(ctx, link, pctx)
			if errors.Is(attachErr, errRemoteWritesDisabled) {
				// The link verified as MISSING remotely and creating it is blocked
				// by the operating mode: defer, don't fail the apply. The slot
				// converges on a later apply once writes are allowed.
				template := adapter.PendingActionTemplate(priceID)
				if template.Provider == "" {
					template.Provider = name
				}
				states[name] = ProviderState{
					Status:     ProviderStatusPendingManualLink,
					SyncStatus: SyncStatusNeverSynced,
					Message:    remoteWritesDisabledMessage,
				}
				pending = append(pending, template)
				continue
			}
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
		// Otherwise dispatch AutoCreate to mint (or find-or-attach) the object.
		// Blocked outright when the operating mode disables provider writes —
		// the find-half of find-or-create is not worth a special case here; the
		// slot converges on a later apply.
		if pctx.RemoteWritesDisabled {
			template := adapter.PendingActionTemplate(priceID)
			if template.Provider == "" {
				template.Provider = name
			}
			states[name] = ProviderState{
				Status:     ProviderStatusPendingManualLink,
				SyncStatus: SyncStatusNeverSynced,
				Message:    remoteWritesDisabledMessage,
			}
			pending = append(pending, template)
			continue
		}
		ids, createErr := adapter.AutoCreate(ctx, pctx)
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

// priceLinkContext builds the substance context (slug + immutable money terms)
// used by adapter.Attach to validate an operator-supplied provider link against
// an EXISTING price (the admin PATCH / link-rotation path). It resolves the
// product slug via the product service; an unresolved product yields an empty
// slug (validators tolerate a blank slug — they still check amount/cycle/mint).
func (s *Service) priceLinkContext(ctx context.Context, price *models.Price) (autoCreateContext, error) {
	if price == nil {
		return autoCreateContext{}, fmt.Errorf("price required")
	}
	pctx := autoCreateContext{
		PriceID:          price.ID,
		ProductID:        price.ProductID,
		UnitAmount:       price.Amount,
		Currency:         price.Currency,
		BillingCycleDays: price.BillingCycleDays,
	}
	if products, err := s.requireProductService(); err == nil {
		if product, err := products.GetByID(ctx, price.ProductID); err == nil && product != nil {
			pctx.Product = product
			pctx.ProductSlug = strings.TrimSpace(product.Slug)
		}
	}
	pctx.LookupKey = internalStripeLookupKey(pctx.ProductSlug, pctx.Currency, pctx.UnitAmount, pctx.BillingCycleDays)
	return pctx, nil
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

// catalogRemoteWritesDisabled reports whether catalog provider writes
// (AutoCreate, Attach's find-or-create, Update propagation) are blocked by the
// operating mode (mode=limited/readonly, #346). Reads/verification stay on.
func (s *Service) catalogRemoteWritesDisabled() bool {
	return s != nil && s.rt != nil && s.rt.Config != nil && s.rt.Config.IsLimitedMode()
}
