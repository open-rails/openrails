package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// Catalog reconciliation loop (issue #209).
//
// This file implements an alert-only, pull-based drift + orphan detector for
// the Stripe catalog. It NEVER mutates Stripe or the OpenRails catalog rows —
// it only records divergence into billing.catalog_drift_events. Operators
// resolve drift via the existing per-price reconcile action
// (POST /admin/catalog/prices/:id/reconcile from issue #205), which auto-closes
// matching open drift events.
//
// The diff itself (computeCatalogDrift) is a pure function over fetched inputs
// so it is unit-testable without a Stripe account or a database. The DB I/O
// (fetch local rows, persist events, dedupe) is layered on top.

// stripeProductLister is the subset of StripeCatalogService the loop needs.
// Defining it as an interface lets unit tests inject fixture pages without a
// live Stripe account.
type stripeProductLister interface {
	ListProducts(ctx context.Context, startingAfter string) ([]catalog.StripeProduct, string, error)
	ListPrices(ctx context.Context, startingAfter string) ([]catalog.StripePrice, string, error)
}

// localCatalogSnapshot is the OpenRails-side view the diff compares against.
// Built from the products + prices tables.
type localCatalogSnapshot struct {
	// productByID maps OpenRails product UUID (string) -> product row.
	productByID map[string]*models.Product
	// priceByID maps OpenRails price UUID (string) -> price row.
	priceByID map[string]*models.Price
	// stripeProductIDByOpenRailsPrice maps OpenRails price id -> stored stripe product id.
	// stripeProductIDs is the set of stripe product ids OpenRails believes it owns.
	stripeProductIDs map[string]string // stripe product id -> openrails product id
	// stripePriceIDs is the set of stripe price ids OpenRails believes it owns.
	stripePriceIDs map[string]string // stripe price id -> openrails price id
}

// computeCatalogDrift is the pure diff: given the full Stripe product+price
// lists and the OpenRails snapshot, it returns the set of drift events that
// should be open. Idempotent and side-effect-free.
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
	for _, sp := range stripeProducts {
		seenStripeProductIDs[sp.ID] = struct{}{}
		orProductID := strings.TrimSpace(sp.Metadata[catalog.StripeMetadataOpenRailsProductID])
		if orProductID == "" {
			// No marker at all -> a Stripe-native product OpenRails doesn't track.
			events = append(events, models.CatalogDriftEvent{
				Kind:                  models.CatalogDriftOrphanInStripe,
				OpenRailsResourceType: models.CatalogDriftResourceProduct,
				StripeResourceID:      sp.ID,
				DetectedAt:            now,
			})
			continue
		}
		local, ok := snap.productByID[orProductID]
		if !ok {
			// Marker points at an OpenRails product that doesn't exist.
			events = append(events, models.CatalogDriftEvent{
				Kind:                  models.CatalogDriftOrphanInStripe,
				OpenRailsResourceType: models.CatalogDriftResourceProduct,
				OpenRailsResourceID:   orProductID,
				StripeResourceID:      sp.ID,
				DetectedAt:            now,
			})
			continue
		}
		events = append(events, diffProductFields(local, sp, now)...)
	}

	// --- Stripe Prices: orphan + field drift ---
	for _, sp := range stripePrices {
		seenStripePriceIDs[sp.ID] = struct{}{}
		orPriceID := strings.TrimSpace(sp.Metadata[catalog.StripeMetadataOpenRailsPriceID])
		if orPriceID == "" {
			events = append(events, models.CatalogDriftEvent{
				Kind:                  models.CatalogDriftOrphanInStripe,
				OpenRailsResourceType: models.CatalogDriftResourcePrice,
				StripeResourceID:      sp.ID,
				DetectedAt:            now,
			})
			continue
		}
		local, ok := snap.priceByID[orPriceID]
		if !ok {
			events = append(events, models.CatalogDriftEvent{
				Kind:                  models.CatalogDriftOrphanInStripe,
				OpenRailsResourceType: models.CatalogDriftResourcePrice,
				OpenRailsResourceID:   orPriceID,
				StripeResourceID:      sp.ID,
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
				Kind:                  models.CatalogDriftMissingInStripe,
				OpenRailsResourceType: models.CatalogDriftResourceProduct,
				OpenRailsResourceID:   orProductID,
				StripeResourceID:      stripeProductID,
				DetectedAt:            now,
			})
		}
	}
	for stripePriceID, orPriceID := range snap.stripePriceIDs {
		if _, seen := seenStripePriceIDs[stripePriceID]; !seen {
			events = append(events, models.CatalogDriftEvent{
				Kind:                  models.CatalogDriftMissingInStripe,
				OpenRailsResourceType: models.CatalogDriftResourcePrice,
				OpenRailsResourceID:   orPriceID,
				StripeResourceID:      stripePriceID,
				DetectedAt:            now,
			})
		}
	}

	return events
}

// diffProductFields compares an OpenRails product against its Stripe mirror and
// emits one field_drift event per diverging mutable field.
func diffProductFields(local *models.Product, sp catalog.StripeProduct, now time.Time) []models.CatalogDriftEvent {
	var out []models.CatalogDriftEvent
	emit := func(field, orVal, stripeVal string) {
		out = append(out, models.CatalogDriftEvent{
			Kind:                  models.CatalogDriftFieldDrift,
			OpenRailsResourceType: models.CatalogDriftResourceProduct,
			OpenRailsResourceID:   local.ID.String(),
			StripeResourceID:      sp.ID,
			Field:                 field,
			OpenRailsValue:        orVal,
			StripeValue:           stripeVal,
			DetectedAt:            now,
		})
	}
	if strings.TrimSpace(local.DisplayName) != strings.TrimSpace(sp.Name) {
		emit("name", local.DisplayName, sp.Name)
	}
	if strings.TrimSpace(local.Description) != strings.TrimSpace(sp.Description) {
		emit("description", local.Description, sp.Description)
	}
	if local.IsActive != sp.Active {
		emit("active", strconv.FormatBool(local.IsActive), strconv.FormatBool(sp.Active))
	}
	return out
}

// diffPriceFields compares an OpenRails price against its Stripe mirror.
func diffPriceFields(local *models.Price, sp catalog.StripePrice, now time.Time) []models.CatalogDriftEvent {
	var out []models.CatalogDriftEvent
	emit := func(field, orVal, stripeVal string) {
		out = append(out, models.CatalogDriftEvent{
			Kind:                  models.CatalogDriftFieldDrift,
			OpenRailsResourceType: models.CatalogDriftResourcePrice,
			OpenRailsResourceID:   local.ID.String(),
			StripeResourceID:      sp.ID,
			Field:                 field,
			OpenRailsValue:        orVal,
			StripeValue:           stripeVal,
			DetectedAt:            now,
		})
	}
	if local.Amount != sp.UnitAmount {
		emit("unit_amount", strconv.FormatInt(local.Amount, 10), strconv.FormatInt(sp.UnitAmount, 10))
	}
	if !strings.EqualFold(strings.TrimSpace(local.Currency), strings.TrimSpace(sp.Currency)) {
		emit("currency", local.Currency, sp.Currency)
	}
	if local.IsActive != sp.Active {
		emit("active", strconv.FormatBool(local.IsActive), strconv.FormatBool(sp.Active))
	}
	if strings.TrimSpace(local.DisplayName) != strings.TrimSpace(sp.Nickname) {
		emit("nickname", local.DisplayName, sp.Nickname)
	}
	return out
}

// driftDedupeKey is the natural key used to dedupe events across reruns. Two
// events with the same key represent the same divergence and must collapse to
// one open row.
func driftDedupeKey(e models.CatalogDriftEvent) string {
	return strings.Join([]string{
		string(e.Kind),
		string(e.OpenRailsResourceType),
		e.OpenRailsResourceID,
		e.StripeResourceID,
		e.Field,
	}, "|")
}

// CatalogDriftReport is the result of a reconciliation pass.
type CatalogDriftReport struct {
	// ScannedProducts / ScannedPrices is the number of Stripe objects examined.
	ScannedProducts int `json:"scanned_products"`
	ScannedPrices   int `json:"scanned_prices"`
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
	Kind                  string     `json:"kind"`
	OpenRailsResourceType string     `json:"openrails_resource_type"`
	OpenRailsResourceID   string     `json:"openrails_resource_id,omitempty"`
	StripeResourceID      string     `json:"stripe_resource_id,omitempty"`
	Field                 string     `json:"field,omitempty"`
	OpenRailsValue        string     `json:"openrails_value,omitempty"`
	StripeValue           string     `json:"stripe_value,omitempty"`
	DetectedAt            time.Time  `json:"detected_at"`
	ResolvedAt            *time.Time `json:"resolved_at,omitempty"`
}

func driftEventToView(e *models.CatalogDriftEvent) CatalogDriftEventView {
	return CatalogDriftEventView{
		ID:                    e.ID,
		Kind:                  string(e.Kind),
		OpenRailsResourceType: string(e.OpenRailsResourceType),
		OpenRailsResourceID:   e.OpenRailsResourceID,
		StripeResourceID:      e.StripeResourceID,
		Field:                 e.Field,
		OpenRailsValue:        e.OpenRailsValue,
		StripeValue:           e.StripeValue,
		DetectedAt:            e.DetectedAt,
		ResolvedAt:            e.ResolvedAt,
	}
}

// buildLocalCatalogSnapshot loads the full OpenRails catalog (products + prices)
// into the in-memory snapshot the diff consumes.
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

	snap := localCatalogSnapshot{
		productByID:      make(map[string]*models.Product, len(productRows)),
		priceByID:        make(map[string]*models.Price, len(priceRows)),
		stripeProductIDs: make(map[string]string),
		stripePriceIDs:   make(map[string]string),
	}
	for _, p := range productRows {
		snap.productByID[p.ID.String()] = p
	}
	for _, pr := range priceRows {
		snap.priceByID[pr.ID.String()] = pr
		stripe := pr.Processors["stripe"]
		if stripe == nil {
			continue
		}
		if id := strings.TrimSpace(stripe[models.ProcessorKeyStripePriceID]); id != "" {
			snap.stripePriceIDs[id] = pr.ID.String()
		}
		if id := strings.TrimSpace(stripe[models.ProcessorKeyStripeProductID]); id != "" {
			// Map back to the OpenRails product the price belongs to.
			snap.stripeProductIDs[id] = pr.ProductID.String()
		}
	}
	return snap, nil
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

// RunCatalogReconciliation performs one full pull-and-diff pass: it enumerates
// the Stripe catalog, diffs against OpenRails, and persists drift events
// (inserting new divergences, auto-resolving ones that have disappeared). It is
// idempotent — a second consecutive run with no underlying change inserts zero
// new rows. Alert-only: it never mutates Stripe or the catalog rows.
func (s *Service) RunCatalogReconciliation(ctx context.Context) (*CatalogDriftReport, error) {
	cfg, err := s.requireConfig()
	if err != nil {
		return nil, err
	}
	stripeProc := cfg.GetStripeProcessor()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, fmt.Errorf("stripe is not configured")
	}
	lister := &catalog.StripeCatalogService{Config: cfg}
	return s.runCatalogReconciliationWith(ctx, lister)
}

// runCatalogReconciliationWith is the testable core: the Stripe lister is
// injected so unit tests can supply fixture pages.
func (s *Service) runCatalogReconciliationWith(ctx context.Context, lister stripeProductLister) (*CatalogDriftReport, error) {
	products, prices, err := fetchStripeCatalog(ctx, lister)
	if err != nil {
		return nil, err
	}
	snap, err := s.buildLocalCatalogSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	desired := computeCatalogDrift(products, prices, snap, now)

	for i := range desired {
		e := desired[i]
		log.WithContext(ctx).WithFields(log.Fields{
			"event":                   "catalog_drift",
			"kind":                    string(e.Kind),
			"openrails_resource_type": string(e.OpenRailsResourceType),
			"openrails_resource_id":   e.OpenRailsResourceID,
			"stripe_resource_id":      e.StripeResourceID,
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
		ScannedProducts: len(products),
		ScannedPrices:   len(prices),
		OpenEvents:      open,
		NewEvents:       newCount,
		ResolvedEvents:  resolvedCount,
	}
	return report, nil
}

// persistCatalogDrift reconciles the desired open-event set against the DB:
// inserts events whose dedupe key has no open row, and auto-resolves open rows
// whose divergence is no longer present. Returns (inserted, resolved) counts.
func (s *Service) persistCatalogDrift(ctx context.Context, desired []models.CatalogDriftEvent, now time.Time) (int, int, error) {
	dbi, err := s.requireDB()
	if err != nil {
		return 0, 0, err
	}
	idb := dbi.GetDB()

	var existing []models.CatalogDriftEvent
	if err := idb.NewSelect().Model(&existing).Where("resolved_at IS NULL").Scan(ctx); err != nil {
		return 0, 0, fmt.Errorf("load open drift events: %w", err)
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
			row.ID = uuid.New()
		}
		if _, err := idb.NewInsert().Model(&row).Exec(ctx); err != nil {
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
		if _, err := idb.NewUpdate().Model((*models.CatalogDriftEvent)(nil)).
			Set("resolved_at = ?", now).
			Where("id = ? AND resolved_at IS NULL", row.ID).
			Exec(ctx); err != nil {
			return inserted, resolved, fmt.Errorf("resolve drift event: %w", err)
		}
		resolved++
	}
	return inserted, resolved, nil
}

// CatalogDriftFilter narrows the open-drift listing.
type CatalogDriftFilter struct {
	Kind         string
	ResourceType string
	Limit        int
	Offset       int
}

// ListCatalogDrift returns open (unresolved) drift events with pagination and
// optional kind / resource_type filters. Total is the unpaginated count.
func (s *Service) ListCatalogDrift(ctx context.Context, filter CatalogDriftFilter) (items []CatalogDriftEventView, total int64, err error) {
	dbi, err := s.requireDB()
	if err != nil {
		return nil, 0, err
	}
	idb := dbi.GetDB()
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	q := idb.NewSelect().Model((*models.CatalogDriftEvent)(nil)).Where("resolved_at IS NULL")
	if k := strings.TrimSpace(filter.Kind); k != "" {
		q = q.Where("kind = ?", k)
	}
	if rt := strings.TrimSpace(filter.ResourceType); rt != "" {
		q = q.Where("openrails_resource_type = ?", rt)
	}

	var rows []models.CatalogDriftEvent
	count, err := q.Order("detected_at DESC").Limit(limit).Offset(offset).ScanAndCount(ctx, &rows)
	if err != nil {
		return nil, 0, fmt.Errorf("list drift events: %w", err)
	}
	out := make([]CatalogDriftEventView, 0, len(rows))
	for i := range rows {
		out = append(out, driftEventToView(&rows[i]))
	}
	return out, int64(count), nil
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
	idb := dbi.GetDB()
	res, err := idb.NewUpdate().Model((*models.CatalogDriftEvent)(nil)).
		Set("resolved_at = ?", s.now().UTC()).
		Where("resolved_at IS NULL").
		Where("openrails_resource_type = ?", string(resourceType)).
		Where("openrails_resource_id = ?", openRailsResourceID).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve drift for resource: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CountOpenDriftByKind returns open drift counts grouped by kind, for the
// openrails_catalog_drift_open_count{kind} metric.
func (s *Service) CountOpenDriftByKind(ctx context.Context) (map[string]int64, error) {
	dbi, err := s.requireDB()
	if err != nil {
		return nil, err
	}
	idb := dbi.GetDB()
	var rows []struct {
		Kind string `bun:"kind"`
		N    int64  `bun:"n"`
	}
	if err := idb.NewSelect().Model((*models.CatalogDriftEvent)(nil)).
		Column("kind").
		ColumnExpr("count(*) AS n").
		Where("resolved_at IS NULL").
		Group("kind").
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("count open drift: %w", err)
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Kind] = r.N
	}
	return out, nil
}

// compile-time assertion that StripeCatalogService satisfies the lister contract.
var _ stripeProductLister = (*catalog.StripeCatalogService)(nil)
