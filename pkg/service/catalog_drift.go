package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Catalog reconciliation loop (issue #209).
//
// This file implements an alert-only, pull-based drift + orphan detector for
// BOTH the Stripe catalog and the NMI recurring-plan catalog. It NEVER mutates
// Stripe, NMI, or the OpenRails catalog rows — it only records divergence into
// openrails.catalog_drift_events. Operators resolve drift via the existing
// per-price reconcile action (POST /merchant/catalog/prices/:id/reconcile from
// issue #205), which auto-closes matching open drift events.
//
// CCBill is intentionally NOT reconciled: CCBill has no catalog-list API
// (FlexForms are write-only redirect URLs, webhooks are inbound, DataLink only
// exports members/subscriptions — not catalog forms), so enumeration is
// structurally impossible. CCBill catalog links stay manual-only forever.
//
// The diffs (computeCatalogDrift / computeNMICatalogDrift) are pure functions
// over fetched inputs so they are unit-testable without a Stripe/NMI account or
// a database. The DB I/O (fetch local rows, persist events, dedupe) is layered
// on top, and a single pass runs both providers (each independently skipped if
// that rail is unconfigured).

// stripeProductLister is the subset of StripeCatalogService the loop needs.
// Defining it as an interface lets unit tests inject fixture pages without a
// live Stripe account.
type stripeProductLister interface {
	ListProducts(ctx context.Context, startingAfter string) ([]catalog.StripeProduct, string, error)
	ListPrices(ctx context.Context, startingAfter string) ([]catalog.StripePrice, string, error)
}

// nmiPlanLister is the subset of the NMI client the loop needs. Defining it as
// an interface lets unit tests inject fixture plans without a live NMI account.
type nmiPlanLister interface {
	ListRecurringPlans() ([]nmi.V5Plan, error)
}

// localCatalogSnapshot is the OpenRails-side view the diffs compare against.
// Built from the products + prices tables.
//
// Stripe matching is CONTENT-keyed: Stripe objects are reverse-matched to
// OpenRails rows by the product key (from metadata openrails_product_key) and
// the price's financial content key — product key + immutable money terms
// (from the Stripe price lookup_key / metadata openrails_price_key), NOT by the
// row UUIDs. This makes drift matching survive a DB rebuild that regenerates UUIDs.
type localCatalogSnapshot struct {
	// productByID maps OpenRails product UUID (string) -> product row.
	productByID map[string]*models.Product
	// priceByID maps OpenRails price UUID (string) -> price row.
	priceByID map[string]*models.Price
	// productByKey maps OpenRails product key -> product row (content key).
	productByKey map[string]*models.Product
	// priceByContentKey maps the financial content key
	// "<product_key>.<currency>.<unit_amount>.<cycle>" -> price row.
	priceByContentKey map[string]*models.Price
	// stripeProductIDs is the set of stripe product ids OpenRails believes it owns.
	stripeProductIDs map[string]string // stripe product id -> openrails product id
	// stripePriceIDs is the set of stripe price ids OpenRails believes it owns.
	stripePriceIDs map[string]string // stripe price id -> openrails price id
	// nmiPlanIDByOpenRailsPrice maps OpenRails price id -> stored nmi plan_id.
	nmiPlanIDByOpenRailsPrice map[string]string
}

// ---------------------------------------------------------------------------
// Stripe pass
// ---------------------------------------------------------------------------

// computeCatalogDrift is the pure Stripe diff: given the full Stripe
// product+price lists and the OpenRails snapshot, it returns the set of Stripe
// drift events that should be open. Idempotent and side-effect-free.
func computeCatalogDrift(
	stripeProducts []catalog.StripeProduct,
	stripePrices []catalog.StripePrice,
	snap localCatalogSnapshot,
	now time.Time,
) []models.CatalogDriftEvent {
	var events []models.CatalogDriftEvent

	// Track which stripe ids we observed so we can compute missing_in_stripe.
	seenStripeProductIDs := make(map[string]struct{}, len(stripeProducts))
	seenStripePriceIDs := make(map[string]struct{}, len(stripePrices))

	// --- Stripe Products: orphan + field drift ---
	// Match on the CONTENT key (product key from openrails_product_key) so the
	// match survives a DB rebuild that regenerates row UUIDs.
	for _, sp := range stripeProducts {
		seenStripeProductIDs[sp.ID] = struct{}{}
		productKey := strings.TrimSpace(sp.Metadata[catalog.StripeMetadataOpenRailsProductKey])
		if productKey == "" {
			// No marker at all -> a Stripe-native product OpenRails doesn't track.
			events = append(events, models.CatalogDriftEvent{
				Provider:              models.CatalogDriftProviderStripe,
				Kind:                  models.CatalogDriftOrphanInStripe,
				OpenRailsResourceType: models.CatalogDriftResourceProduct,
				ExternalResourceID:    sp.ID,
				DetectedAt:            now,
			})
			continue
		}
		local, ok := snap.productByKey[productKey]
		if !ok {
			// Marker points at an OpenRails product key that doesn't exist.
			events = append(events, models.CatalogDriftEvent{
				Provider:              models.CatalogDriftProviderStripe,
				Kind:                  models.CatalogDriftOrphanInStripe,
				OpenRailsResourceType: models.CatalogDriftResourceProduct,
				OpenRailsResourceID:   productKey,
				ExternalResourceID:    sp.ID,
				DetectedAt:            now,
			})
			continue
		}
		events = append(events, diffProductFields(local, sp, now)...)
	}

	// --- Stripe Prices: orphan + field drift ---
	// Match on the CONTENT key: prefer the price's lookup_key
	// ("openrails.<product_key>.<price_terms>"), falling back to the
	// openrails_price_key metadata ("<product_key>.<price_terms>").
	for _, sp := range stripePrices {
		seenStripePriceIDs[sp.ID] = struct{}{}
		priceKey := stripePriceContentKey(sp)
		if priceKey == "" {
			events = append(events, models.CatalogDriftEvent{
				Provider:              models.CatalogDriftProviderStripe,
				Kind:                  models.CatalogDriftOrphanInStripe,
				OpenRailsResourceType: models.CatalogDriftResourcePrice,
				ExternalResourceID:    sp.ID,
				DetectedAt:            now,
			})
			continue
		}
		local, ok := snap.priceByContentKey[priceKey]
		if !ok {
			events = append(events, models.CatalogDriftEvent{
				Provider:              models.CatalogDriftProviderStripe,
				Kind:                  models.CatalogDriftOrphanInStripe,
				OpenRailsResourceType: models.CatalogDriftResourcePrice,
				OpenRailsResourceID:   priceKey,
				ExternalResourceID:    sp.ID,
				DetectedAt:            now,
			})
			continue
		}
		events = append(events, diffPriceFields(local, sp, now)...)
	}

	// --- OpenRails rows with a stored stripe id absent from the pull -> missing_in_stripe ---
	for stripeProductID, orProductID := range snap.stripeProductIDs {
		if _, seen := seenStripeProductIDs[stripeProductID]; !seen {
			events = append(events, models.CatalogDriftEvent{
				Provider:              models.CatalogDriftProviderStripe,
				Kind:                  models.CatalogDriftMissingInStripe,
				OpenRailsResourceType: models.CatalogDriftResourceProduct,
				OpenRailsResourceID:   orProductID,
				ExternalResourceID:    stripeProductID,
				DetectedAt:            now,
			})
		}
	}
	for stripePriceID, orPriceID := range snap.stripePriceIDs {
		if _, seen := seenStripePriceIDs[stripePriceID]; !seen {
			events = append(events, models.CatalogDriftEvent{
				Provider:              models.CatalogDriftProviderStripe,
				Kind:                  models.CatalogDriftMissingInStripe,
				OpenRailsResourceType: models.CatalogDriftResourcePrice,
				OpenRailsResourceID:   orPriceID,
				ExternalResourceID:    stripePriceID,
				DetectedAt:            now,
			})
		}
	}

	return events
}

// stripePriceContentKey extracts the OpenRails financial content key
// ("<product_key>.<currency>.<unit_amount>.<cycle>") from a Stripe Price for
// reverse-matching. It prefers the openrails_price_key metadata, falling back to
// deriving it from the lookup_key ("openrails.<content_key>" -> strip the
// prefix). Returns "" when neither is present (a Stripe-native price OpenRails
// doesn't own).
// Canonical implementation: catalog.RemoteStripePriceContentKey (shared with
// the #358 archive-intent relevance checks).
func stripePriceContentKey(sp catalog.StripePrice) string {
	return catalog.RemoteStripePriceContentKey(sp)
}

// diffProductFields compares an OpenRails product against its Stripe mirror and
// emits one field_drift event per diverging mutable field.
func diffProductFields(local *models.Product, sp catalog.StripeProduct, now time.Time) []models.CatalogDriftEvent {
	var out []models.CatalogDriftEvent
	emit := func(field, orVal, stripeVal string) {
		out = append(out, models.CatalogDriftEvent{
			Provider:              models.CatalogDriftProviderStripe,
			Kind:                  models.CatalogDriftFieldDrift,
			OpenRailsResourceType: models.CatalogDriftResourceProduct,
			OpenRailsResourceID:   local.ID.String(),
			ExternalResourceID:    sp.ID,
			Field:                 field,
			OpenRailsValue:        orVal,
			ExternalValue:         stripeVal,
			DetectedAt:            now,
		})
	}
	if strings.TrimSpace(local.DisplayName) != strings.TrimSpace(sp.Name) {
		emit("name", local.DisplayName, sp.Name)
	}
	if strings.TrimSpace(local.Description) != strings.TrimSpace(sp.Description) {
		emit("description", local.Description, sp.Description)
	}
	if localActive := local.IsPurchasable(); localActive != sp.Active {
		emit("active", strconv.FormatBool(localActive), strconv.FormatBool(sp.Active))
	}
	return out
}

// diffPriceFields compares an OpenRails price against its Stripe mirror.
func diffPriceFields(local *models.Price, sp catalog.StripePrice, now time.Time) []models.CatalogDriftEvent {
	var out []models.CatalogDriftEvent
	emit := func(field, orVal, stripeVal string) {
		out = append(out, models.CatalogDriftEvent{
			Provider:              models.CatalogDriftProviderStripe,
			Kind:                  models.CatalogDriftFieldDrift,
			OpenRailsResourceType: models.CatalogDriftResourcePrice,
			OpenRailsResourceID:   local.ID.String(),
			ExternalResourceID:    sp.ID,
			Field:                 field,
			OpenRailsValue:        orVal,
			ExternalValue:         stripeVal,
			DetectedAt:            now,
		})
	}
	remoteUnitAmountMicros := moneyutil.CentsToMicros(sp.UnitAmount)
	if local.Amount != remoteUnitAmountMicros {
		emit("unit_amount", strconv.FormatInt(local.Amount, 10), strconv.FormatInt(remoteUnitAmountMicros, 10))
	}
	if !strings.EqualFold(strings.TrimSpace(local.Currency), strings.TrimSpace(sp.Currency)) {
		emit("currency", local.Currency, sp.Currency)
	}
	if localActive := local.IsPurchasable(); localActive != sp.Active {
		emit("active", strconv.FormatBool(localActive), strconv.FormatBool(sp.Active))
	}
	return out
}

// ---------------------------------------------------------------------------
// NMI pass
// ---------------------------------------------------------------------------

// nmiPlan is the parsed shape of one NMI recurring plan from the Query API
// (recurring_plans report). NMI's plan model is flat — there is no separate
// product object — so the drift surface is just name + amount. Amount is parsed
// from NMI's dollar string into integer cents, then converted to local micros
// when compared to OpenRails prices.
type nmiPlan struct {
	PlanID      string
	PlanName    string
	AmountCents int64
}

// mapNMIPlans converts the typed v5 plan list into the diff's nmiPlan shape.
func mapNMIPlans(plans []nmi.V5Plan) []nmiPlan {
	out := make([]nmiPlan, 0, len(plans))
	for _, p := range plans {
		out = append(out, nmiPlan{
			PlanID:      strings.TrimSpace(p.ID),
			PlanName:    p.PlanName,
			AmountCents: dollarStringToCents(p.PlanAmount),
		})
	}
	return out
}

// dollarStringToCents converts an NMI dollar amount string (e.g. "9.99") into
// integer cents, rounding to the nearest cent. Unparseable values yield 0.
func dollarStringToCents(dollars string) int64 {
	trimmed := strings.TrimSpace(dollars)
	if trimmed == "" {
		return 0
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0
	}
	if f < 0 {
		return -int64(-f*100 + 0.5)
	}
	return int64(f*100 + 0.5)
}

// computeNMICatalogDrift is the pure NMI diff: given the full list of NMI
// recurring plans and the OpenRails snapshot, it returns the NMI drift events
// that should be open. NMI plans are matched to OpenRails prices by the stored
// nmi plan_id. Idempotent and side-effect-free.
func computeNMICatalogDrift(
	plans []nmiPlan,
	snap localCatalogSnapshot,
	now time.Time,
) []models.CatalogDriftEvent {
	var events []models.CatalogDriftEvent

	// Build a lookup of plan_id -> OpenRails price id from the snapshot.
	priceByPlanID := make(map[string]string, len(snap.nmiPlanIDByOpenRailsPrice))
	for orPriceID, planID := range snap.nmiPlanIDByOpenRailsPrice {
		if planID != "" {
			priceByPlanID[planID] = orPriceID
		}
	}

	seenPlanIDs := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		if plan.PlanID == "" {
			continue
		}
		seenPlanIDs[plan.PlanID] = struct{}{}
		orPriceID, matched := priceByPlanID[plan.PlanID]
		if !matched {
			// Plan on the NMI account with no matching OpenRails price. Generated
			// plan_ids are content-addressed (no "openrails-"/UUID prefix), so the
			// plan_id no longer encodes a price UUID to recover — the orphan is
			// reported with the external plan_id only and no OpenRailsResourceID.
			events = append(events, models.CatalogDriftEvent{
				Provider:              models.CatalogDriftProviderNMI,
				Kind:                  models.CatalogDriftOrphanInNMI,
				OpenRailsResourceType: models.CatalogDriftResourcePrice,
				ExternalResourceID:    plan.PlanID,
				DetectedAt:            now,
			})
			continue
		}
		// Matched: diff amount only (frequency is immutable, not drift).
		local := snap.priceByID[orPriceID]
		if local == nil {
			continue
		}
		remoteAmountMicros := moneyutil.CentsToMicros(plan.AmountCents)
		if local.Amount != remoteAmountMicros {
			events = append(events, nmiFieldDriftEvent(local.ID.String(), plan.PlanID, "plan_amount", strconv.FormatInt(local.Amount, 10), strconv.FormatInt(remoteAmountMicros, 10), now))
		}
	}

	// --- OpenRails prices referencing a plan_id absent from the pull -> missing_in_nmi ---
	for orPriceID, planID := range snap.nmiPlanIDByOpenRailsPrice {
		if planID == "" {
			continue
		}
		if _, seen := seenPlanIDs[planID]; !seen {
			events = append(events, models.CatalogDriftEvent{
				Provider:              models.CatalogDriftProviderNMI,
				Kind:                  models.CatalogDriftMissingInNMI,
				OpenRailsResourceType: models.CatalogDriftResourcePrice,
				OpenRailsResourceID:   orPriceID,
				ExternalResourceID:    planID,
				DetectedAt:            now,
			})
		}
	}

	return events
}

func nmiFieldDriftEvent(orPriceID, planID, field, orVal, nmiVal string, now time.Time) models.CatalogDriftEvent {
	return models.CatalogDriftEvent{
		Provider:              models.CatalogDriftProviderNMI,
		Kind:                  models.CatalogDriftFieldDrift,
		OpenRailsResourceType: models.CatalogDriftResourcePrice,
		OpenRailsResourceID:   orPriceID,
		ExternalResourceID:    planID,
		Field:                 field,
		OpenRailsValue:        orVal,
		ExternalValue:         nmiVal,
		DetectedAt:            now,
	}
}

// ---------------------------------------------------------------------------
// Dedupe + persistence
// ---------------------------------------------------------------------------

// driftDedupeKey is the natural key used to dedupe events across reruns. Two
// events with the same key represent the same divergence and must collapse to
// one open row. The provider is part of the key so the shared field_drift kind
// is unambiguous across Stripe and NMI.
func driftDedupeKey(e models.CatalogDriftEvent) string {
	return strings.Join([]string{
		string(e.Provider),
		string(e.Kind),
		string(e.OpenRailsResourceType),
		e.OpenRailsResourceID,
		e.ExternalResourceID,
		e.Field,
	}, "|")
}

// CatalogDriftReport is the result of a reconciliation pass.
type CatalogDriftReport struct {
	// ScannedProducts / ScannedPrices is the number of Stripe objects examined.
	ScannedProducts int `json:"scanned_products"`
	ScannedPrices   int `json:"scanned_prices"`
	// ScannedNMIPlans is the number of NMI recurring plans examined.
	ScannedNMIPlans int `json:"scanned_nmi_plans"`
	// ScannedSolanaPlans is the number of OpenRails prices with an on-chain Solana
	// plan handle that were read back from chain and checked for drift.
	ScannedSolanaPlans int `json:"scanned_solana_plans"`
	// OpenEvents is the full set of currently-open drift events after the pass.
	OpenEvents []CatalogDriftEventView `json:"open_events"`
	// NewEvents is how many events this pass inserted (idempotency signal).
	NewEvents int `json:"new_events"`
	// ResolvedEvents is how many previously-open events this pass auto-closed
	// because the divergence is gone.
	ResolvedEvents int `json:"resolved_events"`
}

// CatalogDriftEventView is the API-facing shape of a drift event.
type CatalogDriftEventView struct {
	ID                    uuid.UUID  `json:"id"`
	Provider              string     `json:"provider"`
	Kind                  string     `json:"kind"`
	OpenRailsResourceType string     `json:"openrails_resource_type"`
	OpenRailsResourceID   string     `json:"openrails_resource_id,omitempty"`
	ExternalResourceID    string     `json:"external_resource_id,omitempty"`
	Field                 string     `json:"field,omitempty"`
	OpenRailsValue        string     `json:"openrails_value,omitempty"`
	ExternalValue         string     `json:"external_value,omitempty"`
	DetectedAt            time.Time  `json:"detected_at"`
	ResolvedAt            *time.Time `json:"resolved_at,omitempty"`
}

func driftEventToView(e *models.CatalogDriftEvent) CatalogDriftEventView {
	return CatalogDriftEventView{
		ID:                    e.ID,
		Provider:              string(e.Provider),
		Kind:                  string(e.Kind),
		OpenRailsResourceType: string(e.OpenRailsResourceType),
		OpenRailsResourceID:   e.OpenRailsResourceID,
		ExternalResourceID:    e.ExternalResourceID,
		Field:                 e.Field,
		OpenRailsValue:        e.OpenRailsValue,
		ExternalValue:         e.ExternalValue,
		DetectedAt:            e.DetectedAt,
		ResolvedAt:            e.ResolvedAt,
	}
}

// buildLocalCatalogSnapshot loads the full OpenRails catalog (products + prices)
// into the in-memory snapshot the diffs consume.
func (s *Service) buildLocalCatalogSnapshot(ctx context.Context) (localCatalogSnapshot, error) {
	products, prices, err := s.requireCatalogServices()
	if err != nil {
		return localCatalogSnapshot{}, err
	}
	productRows, err := products.GetAll(ctx)
	if err != nil {
		return localCatalogSnapshot{}, fmt.Errorf("load products: %w", err)
	}
	priceRows, err := prices.GetAll(ctx)
	if err != nil {
		return localCatalogSnapshot{}, fmt.Errorf("load prices: %w", err)
	}
	return buildSnapshotFromRows(productRows, priceRows), nil
}

// buildSnapshotFromRows is the pure builder shared by the service and tests.
func buildSnapshotFromRows(productRows []*models.Product, priceRows []*models.Price) localCatalogSnapshot {
	snap := localCatalogSnapshot{
		productByID:               make(map[string]*models.Product, len(productRows)),
		priceByID:                 make(map[string]*models.Price, len(priceRows)),
		productByKey:              make(map[string]*models.Product, len(productRows)),
		priceByContentKey:         make(map[string]*models.Price, len(priceRows)),
		stripeProductIDs:          make(map[string]string),
		stripePriceIDs:            make(map[string]string),
		nmiPlanIDByOpenRailsPrice: make(map[string]string),
	}
	for _, p := range productRows {
		snap.productByID[p.ID.String()] = p
		if key := strings.TrimSpace(p.Key); key != "" {
			snap.productByKey[key] = p
		}
	}
	for _, pr := range priceRows {
		snap.priceByID[pr.ID.String()] = pr
		// Content key = "<product_key>.<currency>.<unit_amount>.<cycle>"; needs
		// the owning product for its key.
		if prod := snap.productByID[pr.ProductID.String()]; prod != nil {
			if key := strings.TrimSpace(prod.Key); key != "" {
				ck := openRailsPriceContentKey(prod.Key, pr.Currency, pr.Amount, pr.RecurringCycleDays())
				snap.priceByContentKey[ck] = pr
			}
		}
		if stripe := pr.GetRailConfig(models.RailStripe); stripe != nil {
			if id := strings.TrimSpace(stripe[models.RailKeyStripePriceID]); id != "" {
				snap.stripePriceIDs[id] = pr.ID.String()
			}
			if id := strings.TrimSpace(stripe[models.RailKeyStripeProductID]); id != "" {
				// Map back to the OpenRails product the price belongs to.
				snap.stripeProductIDs[id] = pr.ProductID.String()
			}
		}
		// Any NMI account's plan link registers the price.
		for _, nmiLink := range pr.RailAccountConfigs(models.RailNMI) {
			if planID := strings.TrimSpace(nmiLink[models.RailKeyPlanID]); planID != "" {
				snap.nmiPlanIDByOpenRailsPrice[pr.ID.String()] = planID
			}
		}
	}
	return snap
}

// fetchStripeCatalog pages through all Stripe products and prices.
func fetchStripeCatalog(ctx context.Context, lister stripeProductLister) ([]catalog.StripeProduct, []catalog.StripePrice, error) {
	var products []catalog.StripeProduct
	cursor := ""
	for {
		page, next, err := lister.ListProducts(ctx, cursor)
		if err != nil {
			return nil, nil, fmt.Errorf("list stripe products: %w", err)
		}
		products = append(products, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	var prices []catalog.StripePrice
	cursor = ""
	for {
		page, next, err := lister.ListPrices(ctx, cursor)
		if err != nil {
			return nil, nil, fmt.Errorf("list stripe prices: %w", err)
		}
		prices = append(prices, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return products, prices, nil
}

// fetchNMIPlans pulls all NMI recurring plans (GET /v5/plans).
func fetchNMIPlans(lister nmiPlanLister) ([]nmiPlan, error) {
	plans, err := lister.ListRecurringPlans()
	if err != nil {
		return nil, fmt.Errorf("list nmi recurring plans: %w", err)
	}
	return mapNMIPlans(plans), nil
}

// RunCatalogReconciliation performs one full pull-and-diff pass across both the
// Stripe and NMI catalogs, diffs them against OpenRails, and persists drift
// events (inserting new divergences, auto-resolving ones that have disappeared).
// Each provider pass is independently skipped when that rail is
// unconfigured. It is idempotent — a second consecutive run with no underlying
// change inserts zero new rows. Alert-only: it never mutates Stripe, NMI, or the
// catalog rows.
func (s *Service) RunCatalogReconciliation(ctx context.Context) (*CatalogDriftReport, error) {
	cfg, err := s.requireConfig()
	if err != nil {
		return nil, err
	}

	var stripeLister stripeProductLister
	if s.rt != nil && s.railArmed(ctx, string(models.RailStripe)) {
		stripeLister = &catalog.StripeCatalogService{Config: cfg, Rails: s.rt.RailConfigs}
	}

	var nmiLister nmiPlanLister
	if client := s.resolveNMIClientForMerchant(ctx); client != nil {
		nmiLister = client
	}

	solanaConfigured := s.rt != nil && s.railArmed(ctx, string(models.RailSolana))
	if stripeLister == nil && nmiLister == nil && !solanaConfigured {
		return nil, fmt.Errorf("no catalog provider (stripe/nmi/solana) is configured")
	}
	return s.runCatalogReconciliationWith(ctx, stripeLister, nmiLister)
}

// runCatalogReconciliationWith is the testable core: the Stripe + NMI listers
// are injected so unit tests can supply fixture data. A nil lister skips that
// provider's pass.
func (s *Service) runCatalogReconciliationWith(ctx context.Context, stripeLister stripeProductLister, nmiLister nmiPlanLister) (*CatalogDriftReport, error) {
	snap, err := s.buildLocalCatalogSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()

	var (
		desired            []models.CatalogDriftEvent
		scannedProducts    int
		scannedPrices      int
		scannedNMIPlans    int
		scannedSolanaPlans int
	)

	if stripeLister != nil {
		products, prices, ferr := fetchStripeCatalog(ctx, stripeLister)
		if ferr != nil {
			return nil, ferr
		}
		scannedProducts = len(products)
		scannedPrices = len(prices)
		desired = append(desired, computeCatalogDrift(products, prices, snap, now)...)
	}

	if nmiLister != nil {
		plans, ferr := fetchNMIPlans(nmiLister)
		if ferr != nil {
			return nil, ferr
		}
		scannedNMIPlans = len(plans)
		desired = append(desired, computeNMICatalogDrift(plans, snap, now)...)
	}

	// Solana has no "list all plans" API, so its pass is a per-stored-price
	// read-back (gated on a configured RPC). Reuses the solana provider adapter's
	// Verify so the decode+diff logic lives in one place.
	if s.rt != nil && s.railArmed(ctx, string(models.RailSolana)) {
		solEvents, solScanned := s.computeSolanaCatalogDrift(ctx, snap, now)
		scannedSolanaPlans = solScanned
		desired = append(desired, solEvents...)
	}

	for i := range desired {
		e := desired[i]
		log.WithContext(ctx).WithFields(log.Fields{
			"event":                   "catalog_drift",
			"provider":                string(e.Provider),
			"kind":                    string(e.Kind),
			"openrails_resource_type": string(e.OpenRailsResourceType),
			"openrails_resource_id":   e.OpenRailsResourceID,
			"external_resource_id":    e.ExternalResourceID,
			"field":                   e.Field,
		}).Warn("catalog reconciliation detected drift")
	}

	newCount, resolvedCount, err := s.persistCatalogDrift(ctx, desired, now)
	if err != nil {
		return nil, err
	}
	open, err := s.listOpenDriftEvents(ctx, CatalogDriftFilter{})
	if err != nil {
		return nil, err
	}
	report := &CatalogDriftReport{
		ScannedProducts:    scannedProducts,
		ScannedPrices:      scannedPrices,
		ScannedNMIPlans:    scannedNMIPlans,
		ScannedSolanaPlans: scannedSolanaPlans,
		OpenEvents:         open,
		NewEvents:          newCount,
		ResolvedEvents:     resolvedCount,
	}
	return report, nil
}

// computeSolanaCatalogDrift checks every OpenRails price that carries an on-chain
// Solana plan handle (rails["solana"]), reading the Plan account back from
// chain and recording drift. Unlike Stripe/NMI there is no remote catalog to
// enumerate (the Subscriptions program has no list API), so this is a
// per-stored-price verification, and there is no orphan kind. It reuses the solana
// provider adapter's Verify (decode + diff) so the drift logic lives in one place.
// Returns the drift events plus the number of Solana-backed prices scanned.
func (s *Service) computeSolanaCatalogDrift(ctx context.Context, snap localCatalogSnapshot, now time.Time) ([]models.CatalogDriftEvent, int) {
	adapter := &solanaAdapter{svc: s}
	var events []models.CatalogDriftEvent
	scanned := 0
	for _, pr := range snap.priceByID {
		cfg := pr.GetRailConfig(models.RailSolana)
		if len(cfg) == 0 {
			continue
		}
		scanned++
		local := &priceVerifyContext{
			IsActive:         !pr.Archived,
			UnitAmount:       pr.Amount,
			Currency:         pr.Currency,
			BillingCycleDays: pr.RecurringCycleDays(),
		}
		drift, missing, err := adapter.Verify(ctx, cfg, local)
		if err != nil {
			log.WithContext(ctx).WithFields(log.Fields{
				"event":    "catalog_drift",
				"provider": "solana",
				"price_id": pr.ID.String(),
			}).WithError(err).Warn("solana catalog drift check failed")
			continue
		}
		planPDA := strings.TrimSpace(cfg[solanaKeyPlanPDA])
		if missing {
			events = append(events, models.CatalogDriftEvent{
				Provider:              models.CatalogDriftProviderSolana,
				Kind:                  models.CatalogDriftMissingInSolana,
				OpenRailsResourceType: models.CatalogDriftResourcePrice,
				OpenRailsResourceID:   pr.ID.String(),
				ExternalResourceID:    planPDA,
				DetectedAt:            now,
			})
			continue
		}
		for _, d := range drift {
			events = append(events, models.CatalogDriftEvent{
				Provider:              models.CatalogDriftProviderSolana,
				Kind:                  models.CatalogDriftFieldDrift,
				OpenRailsResourceType: models.CatalogDriftResourcePrice,
				OpenRailsResourceID:   pr.ID.String(),
				ExternalResourceID:    planPDA,
				Field:                 d.Field,
				OpenRailsValue:        d.OpenRailsValue,
				ExternalValue:         d.RemoteValue,
				DetectedAt:            now,
			})
		}
	}
	return events, scanned
}

// persistCatalogDrift reconciles the desired open-event set against the DB:
// inserts events whose dedupe key has no open row, and auto-resolves open rows
// whose divergence is no longer present. Returns (inserted, resolved) counts.
func (s *Service) persistCatalogDrift(ctx context.Context, desired []models.CatalogDriftEvent, now time.Time) (int, int, error) {
	dbi, err := s.requireDB()
	if err != nil {
		return 0, 0, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, 0, err
	}
	q := dbi.Gen(ctx)

	openRows, err := q.ListOpenCatalogDriftEvents(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("load open drift events: %w", err)
	}
	existing := make([]models.CatalogDriftEvent, 0, len(openRows))
	for _, r := range openRows {
		existing = append(existing, driftEventFromGen(r))
	}
	existingByKey := make(map[string]*models.CatalogDriftEvent, len(existing))
	for i := range existing {
		existingByKey[driftDedupeKey(existing[i])] = &existing[i]
	}

	desiredKeys := make(map[string]struct{}, len(desired))
	inserted := 0
	for i := range desired {
		key := driftDedupeKey(desired[i])
		desiredKeys[key] = struct{}{}
		if _, ok := existingByKey[key]; ok {
			continue // already open; idempotent no-op
		}
		row := desired[i]
		if row.ID == uuid.Nil {
			row.ID = uuidutil.NewV7()
		}
		if err := q.InsertCatalogDriftEvent(ctx, gen.InsertCatalogDriftEventParams{
			ID:                    row.ID,
			MerchantID:            tid.UUID(),
			Rail:                  string(row.Provider),
			Kind:                  string(row.Kind),
			OpenrailsResourceType: string(row.OpenRailsResourceType),
			OpenrailsResourceID:   nilIfEmptyText(row.OpenRailsResourceID),
			ExternalResourceID:    nilIfEmptyText(row.ExternalResourceID),
			Field:                 nilIfEmptyText(row.Field),
			OpenrailsValue:        nilIfEmptyText(row.OpenRailsValue),
			ExternalValue:         nilIfEmptyText(row.ExternalValue),
			DetectedAt:            row.DetectedAt,
		}); err != nil {
			return inserted, 0, fmt.Errorf("insert drift event: %w", err)
		}
		inserted++
		existingByKey[key] = &row
	}

	resolved := 0
	for key, row := range existingByKey {
		if _, stillDesired := desiredKeys[key]; stillDesired {
			continue
		}
		// This open event's divergence is gone -> auto-resolve.
		if err := q.ResolveCatalogDriftEvent(ctx, gen.ResolveCatalogDriftEventParams{
			ID: row.ID, ResolvedAt: &now,
		}); err != nil {
			return inserted, resolved, fmt.Errorf("resolve drift event: %w", err)
		}
		resolved++
	}
	return inserted, resolved, nil
}

// driftEventFromGen maps the generated row onto the domain model (NULL text
// columns flatten to "" like the bun nullzero tags did).
func driftEventFromGen(r gen.OpenrailsCatalogDriftEvent) models.CatalogDriftEvent {
	return models.CatalogDriftEvent{
		ID:                    r.ID,
		Provider:              models.CatalogDriftProvider(r.Rail),
		Kind:                  models.CatalogDriftKind(r.Kind),
		OpenRailsResourceType: models.CatalogDriftResourceType(r.OpenrailsResourceType),
		OpenRailsResourceID:   derefText(r.OpenrailsResourceID),
		ExternalResourceID:    derefText(r.ExternalResourceID),
		Field:                 derefText(r.Field),
		OpenRailsValue:        derefText(r.OpenrailsValue),
		ExternalValue:         derefText(r.ExternalValue),
		DetectedAt:            r.DetectedAt,
		ResolvedAt:            r.ResolvedAt,
	}
}

func nilIfEmptyText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// CatalogDriftFilter narrows the open-drift listing.
type CatalogDriftFilter struct {
	Rail         string
	Kind         string
	ResourceType string
	Limit        int
	Offset       int
}

// ListCatalogDrift returns open (unresolved) drift events with pagination and
// optional provider / kind / resource_type filters. Total is the unpaginated count.
func (s *Service) ListCatalogDrift(ctx context.Context, filter CatalogDriftFilter) (items []CatalogDriftEventView, total int64, err error) {
	dbi, err := s.requireDB()
	if err != nil {
		return nil, 0, err
	}
	q := dbi.Gen(ctx)
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	provider := nilIfEmptyText(strings.TrimSpace(filter.Rail))
	kind := nilIfEmptyText(strings.TrimSpace(filter.Kind))
	resourceType := nilIfEmptyText(strings.TrimSpace(filter.ResourceType))

	total, err = q.CountOpenCatalogDriftFiltered(ctx, gen.CountOpenCatalogDriftFilteredParams{
		Rail: provider, Kind: kind, ResourceType: resourceType,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("count drift events: %w", err)
	}
	rows, err := q.ListOpenCatalogDriftFiltered(ctx, gen.ListOpenCatalogDriftFilteredParams{
		Rail: provider, Kind: kind, ResourceType: resourceType,
		Column1: int32(limit), Column2: int32(offset),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list drift events: %w", err)
	}
	out := make([]CatalogDriftEventView, 0, len(rows))
	for i := range rows {
		ev := driftEventFromGen(rows[i])
		out = append(out, driftEventToView(&ev))
	}
	return out, total, nil
}

// listOpenDriftEvents is an internal helper returning just the open events
// (used by the report builder). It applies the same filters as ListCatalogDrift.
func (s *Service) listOpenDriftEvents(ctx context.Context, filter CatalogDriftFilter) ([]CatalogDriftEventView, error) {
	items, _, err := s.ListCatalogDrift(ctx, filter)
	return items, err
}

// ResolveDriftForResource auto-closes any open drift events tied to a specific
// OpenRails resource. Called by the per-price reconcile path so resolving a
// price clears its drift from the active list. Safe to call when no events
// match (no-op). Returns the number of rows closed.
func (s *Service) ResolveDriftForResource(ctx context.Context, resourceType models.CatalogDriftResourceType, openRailsResourceID string) (int, error) {
	openRailsResourceID = strings.TrimSpace(openRailsResourceID)
	if openRailsResourceID == "" {
		return 0, nil
	}
	dbi, err := s.requireDB()
	if err != nil {
		return 0, err
	}
	now := s.now().UTC()
	n, err := dbi.Gen(ctx).ResolveCatalogDriftForResource(ctx, gen.ResolveCatalogDriftForResourceParams{
		ResolvedAt:            &now,
		OpenrailsResourceType: string(resourceType),
		OpenrailsResourceID:   &openRailsResourceID,
	})
	if err != nil {
		return 0, fmt.Errorf("resolve drift for resource: %w", err)
	}
	return int(n), nil
}

// CountOpenDriftByKind returns open drift counts grouped by (provider, kind),
// for the openrails_catalog_drift_open_count{provider,kind} metric. The map key
// is "<provider>/<kind>".
func (s *Service) CountOpenDriftByKind(ctx context.Context) (map[string]int64, error) {
	dbi, err := s.requireDB()
	if err != nil {
		return nil, err
	}
	rows, err := dbi.Gen(ctx).CountOpenCatalogDriftByKind(ctx)
	if err != nil {
		return nil, fmt.Errorf("count open drift: %w", err)
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Rail+"/"+r.Kind] = r.N
	}
	return out, nil
}

// compile-time assertions that the concrete clients satisfy the lister contracts.
var (
	_ stripeProductLister = (*catalog.StripeCatalogService)(nil)
	_ nmiPlanLister       = (*nmi.NMIClient)(nil)
)
