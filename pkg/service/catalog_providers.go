package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
	"sort"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

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
// canonical lookup key on the rails[provider] map (when one exists).
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
// provider slot converges on a later push-catalog once writes are allowed.
// Verification reads always run.
var errRemoteWritesDisabled = errors.New("catalog provider writes are disabled (mode=limited/readonly)")

// remoteWritesDisabledMessage is the pending_manual_link message used when the
// operating mode (not a missing capability) deferred the provider write.
const remoteWritesDisabledMessage = "provider writes disabled (mode=limited/readonly): remote object creation deferred; re-apply once writes are allowed"

// mutableUpdate carries the post-create mutable fields that Update propagates
// to attached providers (active flag). Currency, amount, and provider cadence are
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
	// Name returns the canonical rail name (e.g. "stripe", "ccbill", "nmi").
	// Used by the dispatcher for diagnostics only — the map key in
	// providerAdapters is the source of truth.
	Name() string

	// Attach interprets caller-supplied provider link/config on a freshly created
	// OpenRails price. It validates an existing remote object or find-or-creates
	// one from a client-declarative key, then returns the canonical IDs to write
	// to rails[provider]. Called when the caller provided psp_links[provider] in
	// the create/update request.
	//
	// in carries the OpenRails price substance (product key + immutable money terms) so
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
	// ProductKey is the product's stable identity. Together with the immutable
	// money terms (UnitAmount / Currency / provider-day cadence for day-granularity
	// providers, AccessDurationHours for hour-granularity providers) it forms the
	// CONTENT identity of this price, which drives deterministic provider keys
	// and metadata content keys — so find-or-create survives a DB rebuild that
	// regenerates row UUIDs.
	ProductKey          string
	UnitAmount          int64
	Currency            string
	BillingCycleDays    *int
	AccessDurationHours *int
	LookupKey           string

	// RemoteWritesDisabled tells adapters that catalog provider WRITES are
	// blocked by the operating mode (mode=limited/readonly, #346). Adapters
	// must return errRemoteWritesDisabled at their write points (creating a
	// missing object inside Attach, AutoCreate) and still perform read-only
	// verification normally.
	RemoteWritesDisabled bool

	// TargetAccountID (#641) pins AutoCreate to a specific account by account_id
	// instead of the rail's primary (set when syncing secondaries); empty = primary.
	TargetAccountID string
}

// providerAdapters returns the dispatch table keyed by canonical provider name.
// Adapters are cheap value types holding a reference to the runtime; we build a
// fresh map per call rather than caching on Service to keep tests
// straightforward.
func (s *Service) providerAdapters() map[string]providerAdapter {
	return map[string]providerAdapter{
		string(models.RailStripe): &stripeAdapter{svc: s},
		string(models.RailCCBill): &ccbillAdapter{},
		string(models.RailNMI):    &nmiAdapter{svc: s},
		string(models.RailSolana): &solanaAdapter{svc: s},
	}
}

func sortedAdapterNames(adapters map[string]providerAdapter) []string {
	names := make([]string, 0, len(adapters))
	for name := range adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// openRailsPriceContentKey is the content key derived from a price's FINANCIAL
// SUBSTANCE — the product's stable key plus the immutable money terms
// (currency, unit amount, provider cadence). It is NOT derived from any row UUID,
// so it survives a DB wipe + resync: re-seeding the same product+price terms
// reproduces byte-identical keys and re-attaches to the existing provider
// objects rather than duplicating them.
//
// Because the amount lives IN the key, a different amount yields a different key
// and therefore a different price — a price change is modeled as create-new +
// archive-old, never an in-place mutation/transfer.
//
// Format: "<product_key>.<currency>.<unit_amount>.<cycle>" where <cycle> is the
// provider day cadence for a recurring price or "onetime" for a one-time price.
// Example: "pro.usd.2900.30" or "pro.usd.500.onetime".
// Canonical implementation: catalog.OpenRailsPriceContentKey (shared with the
// #358 archive-intent relevance checks).
func openRailsPriceContentKey(productKey, currency string, unitAmount int64, billingCycleDays *int) string {
	return catalog.OpenRailsPriceContentKey(productKey, currency, unitAmount, billingCycleDays)
}

// internalStripeLookupKey is the deterministic Stripe lookup_key OpenRails
// assigns to every price it auto-creates. It is the price content key prefixed
// with "openrails." so OpenRails-owned prices are unmistakable on the Stripe
// account. Content-addressed (see openRailsPriceContentKey), so it is stable
// across DB rebuilds and reconstructable without the caller supplying anything.
//
// Format: "openrails.<product_key>.<currency>.<unit_amount>.<cycle>".
// Example: "openrails.pro.usd.2900.30".
func internalStripeLookupKey(productKey, currency string, unitAmount int64, billingCycleDays *int) string {
	return "openrails." + openRailsPriceContentKey(productKey, currency, unitAmount, billingCycleDays)
}

// resolveProviders walks the declared `providers` + `provider_links` from a
// CreatePrice request through every adapter, dispatches Attach / AutoCreate as
// appropriate, and returns the raw rails map to persist, the typed
// per-provider response states, and any aggregated pending-manual-action items.
//
// A pending_manual_link result is NOT an error: the price is still created in
// OpenRails with the corresponding provider slot empty, and the response carries
// a PendingAction telling the operator what to do.
//
// Unknown providers are silently dropped (the same way an unknown rail in
// rails[] was previously ignored by the catalog layer).
func (s *Service) resolveProviders(ctx context.Context, product *models.Product, req CreatePriceRequest, priceID uuid.UUID) (
	rails map[string]map[string]string,
	states map[string]ProviderState,
	pending []PendingAction,
	err error,
) {
	adapters := s.providerAdapters()

	// Build the unique attach set: union of req.PSPs and the keys of
	// req.PSPLinks. Lowercase + trimmed for stable dispatch.
	want := map[string]struct{}{}
	for _, p := range req.PSPs {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		want[p] = struct{}{}
	}
	for p := range req.PSPLinks {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		want[p] = struct{}{}
	}

	rails = map[string]map[string]string{}
	states = map[string]ProviderState{}

	// Sort for deterministic ordering in tests / pending action lists.
	names := make([]string, 0, len(want))
	for p := range want {
		names = append(names, p)
	}
	sort.Strings(names)

	// The manifest speaks the operator's ACCOUNT vocabulary ("mobius"), not
	// rail names: resolve each declared name to its rail via the merchant's
	// declared accounts (a rail name itself also resolves — the reserved
	// gateways are their own account names). Unknown names fail loudly: a
	// silently dropped name loses its provider links. Several accounts on one
	// rail are fine — rails[] is keyed by the ACCOUNT key and each entry
	// records its rail.
	type attachTarget struct {
		declared  string // manifest name = ProviderLinks key = rails[]/states[] key
		rail      string // adapters index; recorded in the entry's RailKeyRail
		accountID string // rail-native account id; pins Attach/AutoCreate to the account
		adapter   providerAdapter
	}
	accountRails := s.merchantAccountRails(ctx)
	targets := make([]attachTarget, 0, len(names))
	for _, name := range names {
		t := attachTarget{declared: name, rail: name}
		var ok bool
		t.adapter, ok = adapters[name]
		if !ok {
			if acct, found := accountRails[name]; found {
				t.rail, t.accountID = acct.rail, acct.accountID
				t.adapter, ok = adapters[acct.rail]
			}
		}
		if !ok {
			return nil, nil, nil, fmt.Errorf(
				"unknown provider %q in providers/provider_links: not a rail (%s) or a declared merchant account key",
				name, strings.Join(sortedAdapterNames(adapters), ", "))
		}
		targets = append(targets, t)
	}

	// The price substance (product key + immutable money terms) is the same for the
	// Attach (link-validation) and AutoCreate (mint) paths. lookup_key + content
	// keys are derived from the product key + money terms, not the row UUIDs, so
	// re-syncing after a DB wipe finds the same provider objects.
	productKey := ""
	if product != nil {
		productKey = strings.TrimSpace(product.Key)
	}
	remoteWritesDisabled := s.rt != nil && s.rt.Config != nil && s.rt.Config.IsLimitedMode()
	// Provider objects key on the RECURRING cadence (nil for one-off / finite
	// windows — those settle as one-time charges; the access window is OpenRails-side).
	reqCycle := req.RecurringCycleDays()
	pctx := autoCreateContext{
		PriceID:              priceID,
		ProductID:            req.ProductID,
		Product:              product,
		ProductKey:           productKey,
		UnitAmount:           req.UnitAmount,
		Currency:             req.Currency,
		BillingCycleDays:     reqCycle,
		AccessDurationHours:  req.AccessDurationHours,
		LookupKey:            internalStripeLookupKey(productKey, req.Currency, req.UnitAmount, reqCycle),
		RemoteWritesDisabled: remoteWritesDisabled,
	}

	// Every entry records its rail; readers match on RailKeyRail only.
	stampRail := func(ids map[string]string, t attachTarget) map[string]string {
		if ids == nil {
			ids = map[string]string{}
		}
		ids[models.RailKeyRail] = t.rail
		return ids
	}

	// deferPending parks this target's slot on a pending manual action: the
	// declared key gets a pending state and the adapter's action template is
	// queued for the operator. An empty message means "use the template hint";
	// callers pass remoteWritesDisabledMessage when the block is the operating
	// mode rather than the provider itself.
	deferPending := func(t attachTarget, message string) {
		template := t.adapter.PendingActionTemplate(priceID)
		if template.Provider == "" {
			template.Provider = t.rail
		}
		if message == "" {
			message = template.Hint
		}
		states[t.declared] = ProviderState{
			Status:     ProviderStatusPendingManualLink,
			SyncStatus: SyncStatusNeverSynced,
			Message:    message,
		}
		pending = append(pending, template)
	}

	for _, t := range targets {
		link := req.PSPLinks[t.declared]
		// Pin provider calls to THIS account's credentials; empty means the
		// rail's armed default.
		tctx := pctx
		tctx.TargetAccountID = t.accountID
		// Try the user-supplied link/config path first. Attach verifies an exact
		// remote id or find-or-creates from a declarative key such as lookup_key,
		// plan_id, or a Solana token.
		if len(normalizeLinkMap(link)) > 0 {
			ids, attachErr := t.adapter.Attach(ctx, link, tctx)
			if errors.Is(attachErr, errPendingManualLink) {
				deferPending(t, "")
				continue
			}
			if errors.Is(attachErr, errRemoteWritesDisabled) {
				// The link verified as MISSING remotely and creating it is blocked
				// by the operating mode: defer, don't fail the apply. The slot
				// converges on a later apply once writes are allowed.
				deferPending(t, remoteWritesDisabledMessage)
				continue
			}
			if attachErr != nil {
				return nil, nil, nil, fmt.Errorf("%s: %w", t.declared, attachErr)
			}
			ids = stampRail(ids, t)
			rails[t.declared] = ids
			states[t.declared] = ProviderState{
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
			deferPending(t, remoteWritesDisabledMessage)
			continue
		}
		ids, createErr := t.adapter.AutoCreate(ctx, tctx)
		switch {
		case errors.Is(createErr, errPendingManualLink):
			deferPending(t, "")
		case createErr != nil:
			return nil, nil, nil, fmt.Errorf("%s: %w", t.declared, createErr)
		default:
			ids = stampRail(ids, t)
			rails[t.declared] = ids
			states[t.declared] = ProviderState{
				Status:     ProviderStatusLinked,
				IDs:        copyStringMap(ids),
				LookupKey:  ids[providerLookupKey],
				SyncStatus: SyncStatusUnknown,
			}
		}
	}
	// Keep every non-archived account in sync (best-effort); archived is
	// skipped. Once per RAIL — the sync itself enumerates the rail's accounts.
	if !remoteWritesDisabled {
		syncedRails := map[string]struct{}{}
		for _, t := range targets {
			if _, done := syncedRails[t.rail]; done {
				continue
			}
			syncedRails[t.rail] = struct{}{}
			s.syncSecondaryCatalogAccounts(ctx, t.rail, pctx, t.adapter)
		}
	}
	return rails, states, pending, nil
}

// railAccountRef is one declared merchant account: its rail plus the
// rail-native account id (pins provider calls to that account's credentials).
type railAccountRef struct {
	rail      string
	accountID string
}

// merchantAccountRails maps the ctx merchant's declared account keys
// (psps.key — the manifest `psps.<key>` name,
// e.g. "mobius") to their rails. Best-effort: no merchant ctx / DB means only
// rail names resolve.
func (s *Service) merchantAccountRails(ctx context.Context) map[string]railAccountRef {
	out := map[string]railAccountRef{}
	if s.rt == nil || s.rt.DB == nil {
		return out
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return out
	}
	rows, err := s.rt.DB.Gen(ctx).ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{
		MerchantID: mid.UUID(),
	})
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("catalog: list declared rail accounts failed; only rail names resolve")
		return out
	}
	for _, row := range rows {
		if row.Archived || row.Key == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(*row.Key))
		if name == "" {
			continue
		}
		out[name] = railAccountRef{
			rail:      strings.ToLower(strings.TrimSpace(row.Rail)),
			accountID: strings.TrimSpace(row.AccountID),
		}
	}
	return out
}

// syncSecondaryCatalogAccounts best-effort find-or-creates the price in each
// of the ctx merchant's non-archived declared accounts on a rail (#788: the
// armed psps state, never a boot artifact). No links
// stored; find-or-create re-discovers by content key and failures are logged.
func (s *Service) syncSecondaryCatalogAccounts(ctx context.Context, rail string, pctx autoCreateContext, adapter providerAdapter) {
	if s.rt == nil || s.rt.DB == nil {
		return
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return
	}
	railName := strings.ToLower(strings.TrimSpace(rail))
	rows, err := s.rt.DB.Gen(ctx).ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{
		MerchantID: mid.UUID(),
		Rail:       &railName,
	})
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("rail", rail).
			Warn("secondary catalog sync: list declared accounts failed")
		return
	}
	for _, acct := range rows {
		if acct.Archived {
			continue
		}
		sctx := pctx
		sctx.TargetAccountID = strings.TrimSpace(acct.AccountID)
		if _, err := adapter.AutoCreate(ctx, sctx); err != nil && !errors.Is(err, errPendingManualLink) {
			log.WithContext(ctx).WithError(err).
				WithField("rail", rail).
				WithField("psp_id", sctx.TargetAccountID).
				Warn("secondary catalog sync failed (best-effort); drift surfaces on reconcile")
		}
	}
}

// priceLinkContext builds the substance context (product key + immutable money terms)
// used by adapter.Attach to validate an operator-supplied provider link against
// an EXISTING price (the admin PATCH / link-rotation path). It resolves the
// product key via the product service; an unresolved product yields an empty key
// (validators tolerate a blank key — they still check amount/cycle/mint).
func (s *Service) priceLinkContext(ctx context.Context, price *models.Price) (autoCreateContext, error) {
	if price == nil {
		return autoCreateContext{}, fmt.Errorf("price required")
	}
	pctx := autoCreateContext{
		PriceID:             price.ID,
		ProductID:           price.ProductID,
		UnitAmount:          price.Amount,
		Currency:            price.Currency,
		BillingCycleDays:    price.RecurringCycleDays(),
		AccessDurationHours: price.AccessDurationHours,
		// Attach can publish (e.g. a Solana plan from token), so the
		// link-rotation path must carry the same write gate resolveProviders
		// does — otherwise limited/readonly deployments submit provider writes.
		RemoteWritesDisabled: s.catalogRemoteWritesDisabled(),
	}
	if products, err := s.requireProductService(); err == nil {
		if product, err := products.GetByID(ctx, price.ProductID); err == nil && product != nil {
			pctx.Product = product
			pctx.ProductKey = strings.TrimSpace(product.Key)
		}
	}
	pctx.LookupKey = internalStripeLookupKey(pctx.ProductKey, pctx.Currency, pctx.UnitAmount, pctx.BillingCycleDays)
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
// rails map stored on the DB row.
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
