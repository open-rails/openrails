package riverjobs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// KindCatalogReconciliationPull is the River kind for the alert-only Stripe
// catalog reconciliation loop (issue #209). It pulls the full Stripe catalog,
// diffs it against the OpenRails DB, and records drift + orphan events in
// billing.catalog_drift_events. It NEVER mutates Stripe or the catalog rows.
const KindCatalogReconciliationPull = "billing.catalog_reconciliation_pull"

type CatalogReconciliationPullArgs struct{}

func (CatalogReconciliationPullArgs) Kind() string { return KindCatalogReconciliationPull }

// CatalogReconciliationPullWorker runs one pull-and-diff pass. It mirrors the
// logic in pkg/service.RunCatalogReconciliation, but lives in the river package
// because pkg/service imports internal/river (so the worker cannot import back).
type CatalogReconciliationPullWorker struct {
	river.WorkerDefaults[CatalogReconciliationPullArgs]
	DB     *db.DB
	Config *config.Config
}

func (CatalogReconciliationPullWorker) Kind() string { return KindCatalogReconciliationPull }

func (w CatalogReconciliationPullWorker) Work(ctx context.Context, job *river.Job[CatalogReconciliationPullArgs]) error {
	if w.DB == nil {
		return fmt.Errorf("catalog reconciliation: db not configured")
	}
	if w.Config == nil {
		return fmt.Errorf("catalog reconciliation: config not configured")
	}
	stripeProc := w.Config.GetStripeProcessor()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		log.WithContext(ctx).Info("CatalogReconciliation: stripe not configured; skipping")
		return nil
	}

	stripeSvc := &catalog.StripeCatalogService{Config: w.Config}
	products, err := listAllStripeProducts(ctx, stripeSvc)
	if err != nil {
		return err
	}
	prices, err := listAllStripePrices(ctx, stripeSvc)
	if err != nil {
		return err
	}

	productSvc := catalog.NewProductService(w.DB)
	priceSvc := catalog.NewPriceService(w.DB)
	productRows, err := productSvc.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("catalog reconciliation: load products: %w", err)
	}
	priceRows, err := priceSvc.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("catalog reconciliation: load prices: %w", err)
	}

	now := time.Now().UTC()
	desired := computeCatalogDriftJob(products, prices, productRows, priceRows, now)

	for _, e := range desired {
		log.WithContext(ctx).WithFields(log.Fields{
			"event":                   "catalog_drift",
			"kind":                    string(e.Kind),
			"openrails_resource_type": string(e.OpenRailsResourceType),
			"openrails_resource_id":   e.OpenRailsResourceID,
			"stripe_resource_id":      e.StripeResourceID,
			"field":                   e.Field,
		}).Warn("catalog reconciliation detected drift")
	}

	inserted, resolved, err := persistCatalogDriftJob(ctx, w.DB, desired, now)
	if err != nil {
		return err
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"scanned_products": len(products),
		"scanned_prices":   len(prices),
		"new_events":       inserted,
		"resolved_events":  resolved,
	}).Info("CatalogReconciliation: completed pull-and-diff pass")
	return nil
}

func listAllStripeProducts(ctx context.Context, svc *catalog.StripeCatalogService) ([]catalog.StripeProduct, error) {
	var out []catalog.StripeProduct
	cursor := ""
	for {
		page, next, err := svc.ListProducts(ctx, cursor)
		if err != nil {
			return nil, fmt.Errorf("catalog reconciliation: list stripe products: %w", err)
		}
		out = append(out, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

func listAllStripePrices(ctx context.Context, svc *catalog.StripeCatalogService) ([]catalog.StripePrice, error) {
	var out []catalog.StripePrice
	cursor := ""
	for {
		page, next, err := svc.ListPrices(ctx, cursor)
		if err != nil {
			return nil, fmt.Errorf("catalog reconciliation: list stripe prices: %w", err)
		}
		out = append(out, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// computeCatalogDriftJob is the worker-side mirror of
// pkg/service.computeCatalogDrift. Kept in sync with that function; the duplicate
// exists only because the import graph (pkg/service -> internal/river) forbids
// the worker from calling back into pkg/service.
func computeCatalogDriftJob(
	stripeProducts []catalog.StripeProduct,
	stripePrices []catalog.StripePrice,
	productRows []*models.Product,
	priceRows []*models.Price,
	now time.Time,
) []models.CatalogDriftEvent {
	productByID := make(map[string]*models.Product, len(productRows))
	for _, p := range productRows {
		productByID[p.ID.String()] = p
	}
	priceByID := make(map[string]*models.Price, len(priceRows))
	stripeProductIDs := make(map[string]string)
	stripePriceIDs := make(map[string]string)
	for _, pr := range priceRows {
		priceByID[pr.ID.String()] = pr
		stripe := pr.Processors["stripe"]
		if stripe == nil {
			continue
		}
		if id := strings.TrimSpace(stripe[models.ProcessorKeyStripePriceID]); id != "" {
			stripePriceIDs[id] = pr.ID.String()
		}
		if id := strings.TrimSpace(stripe[models.ProcessorKeyStripeProductID]); id != "" {
			stripeProductIDs[id] = pr.ProductID.String()
		}
	}

	var events []models.CatalogDriftEvent
	seenProducts := make(map[string]struct{}, len(stripeProducts))
	seenPrices := make(map[string]struct{}, len(stripePrices))

	for _, sp := range stripeProducts {
		seenProducts[sp.ID] = struct{}{}
		orID := strings.TrimSpace(sp.Metadata[catalog.StripeMetadataOpenRailsProductID])
		if orID == "" {
			events = append(events, models.CatalogDriftEvent{Kind: models.CatalogDriftOrphanInStripe, OpenRailsResourceType: models.CatalogDriftResourceProduct, StripeResourceID: sp.ID, DetectedAt: now})
			continue
		}
		local, ok := productByID[orID]
		if !ok {
			events = append(events, models.CatalogDriftEvent{Kind: models.CatalogDriftOrphanInStripe, OpenRailsResourceType: models.CatalogDriftResourceProduct, OpenRailsResourceID: orID, StripeResourceID: sp.ID, DetectedAt: now})
			continue
		}
		if strings.TrimSpace(local.DisplayName) != strings.TrimSpace(sp.Name) {
			events = append(events, fieldDriftEvent(models.CatalogDriftResourceProduct, local.ID.String(), sp.ID, "name", local.DisplayName, sp.Name, now))
		}
		if strings.TrimSpace(local.Description) != strings.TrimSpace(sp.Description) {
			events = append(events, fieldDriftEvent(models.CatalogDriftResourceProduct, local.ID.String(), sp.ID, "description", local.Description, sp.Description, now))
		}
		if local.IsActive != sp.Active {
			events = append(events, fieldDriftEvent(models.CatalogDriftResourceProduct, local.ID.String(), sp.ID, "active", strconv.FormatBool(local.IsActive), strconv.FormatBool(sp.Active), now))
		}
	}

	for _, sp := range stripePrices {
		seenPrices[sp.ID] = struct{}{}
		orID := strings.TrimSpace(sp.Metadata[catalog.StripeMetadataOpenRailsPriceID])
		if orID == "" {
			events = append(events, models.CatalogDriftEvent{Kind: models.CatalogDriftOrphanInStripe, OpenRailsResourceType: models.CatalogDriftResourcePrice, StripeResourceID: sp.ID, DetectedAt: now})
			continue
		}
		local, ok := priceByID[orID]
		if !ok {
			events = append(events, models.CatalogDriftEvent{Kind: models.CatalogDriftOrphanInStripe, OpenRailsResourceType: models.CatalogDriftResourcePrice, OpenRailsResourceID: orID, StripeResourceID: sp.ID, DetectedAt: now})
			continue
		}
		if local.Amount != sp.UnitAmount {
			events = append(events, fieldDriftEvent(models.CatalogDriftResourcePrice, local.ID.String(), sp.ID, "unit_amount", strconv.FormatInt(local.Amount, 10), strconv.FormatInt(sp.UnitAmount, 10), now))
		}
		if !strings.EqualFold(strings.TrimSpace(local.Currency), strings.TrimSpace(sp.Currency)) {
			events = append(events, fieldDriftEvent(models.CatalogDriftResourcePrice, local.ID.String(), sp.ID, "currency", local.Currency, sp.Currency, now))
		}
		if local.IsActive != sp.Active {
			events = append(events, fieldDriftEvent(models.CatalogDriftResourcePrice, local.ID.String(), sp.ID, "active", strconv.FormatBool(local.IsActive), strconv.FormatBool(sp.Active), now))
		}
		if strings.TrimSpace(local.DisplayName) != strings.TrimSpace(sp.Nickname) {
			events = append(events, fieldDriftEvent(models.CatalogDriftResourcePrice, local.ID.String(), sp.ID, "nickname", local.DisplayName, sp.Nickname, now))
		}
	}

	for stripeProductID, orID := range stripeProductIDs {
		if _, seen := seenProducts[stripeProductID]; !seen {
			events = append(events, models.CatalogDriftEvent{Kind: models.CatalogDriftMissingInStripe, OpenRailsResourceType: models.CatalogDriftResourceProduct, OpenRailsResourceID: orID, StripeResourceID: stripeProductID, DetectedAt: now})
		}
	}
	for stripePriceID, orID := range stripePriceIDs {
		if _, seen := seenPrices[stripePriceID]; !seen {
			events = append(events, models.CatalogDriftEvent{Kind: models.CatalogDriftMissingInStripe, OpenRailsResourceType: models.CatalogDriftResourcePrice, OpenRailsResourceID: orID, StripeResourceID: stripePriceID, DetectedAt: now})
		}
	}
	return events
}

func fieldDriftEvent(rt models.CatalogDriftResourceType, orID, stripeID, field, orVal, stripeVal string, now time.Time) models.CatalogDriftEvent {
	return models.CatalogDriftEvent{
		Kind:                  models.CatalogDriftFieldDrift,
		OpenRailsResourceType: rt,
		OpenRailsResourceID:   orID,
		StripeResourceID:      stripeID,
		Field:                 field,
		OpenRailsValue:        orVal,
		StripeValue:           stripeVal,
		DetectedAt:            now,
	}
}

func driftDedupeKeyJob(e models.CatalogDriftEvent) string {
	return strings.Join([]string{string(e.Kind), string(e.OpenRailsResourceType), e.OpenRailsResourceID, e.StripeResourceID, e.Field}, "|")
}

// persistCatalogDriftJob inserts new divergences and auto-resolves open rows
// whose divergence is gone. Idempotent. Returns (inserted, resolved).
func persistCatalogDriftJob(ctx context.Context, database *db.DB, desired []models.CatalogDriftEvent, now time.Time) (int, int, error) {
	idb := database.GetDB().(*bun.DB)

	var existing []models.CatalogDriftEvent
	if err := idb.NewSelect().Model(&existing).Where("resolved_at IS NULL").Scan(ctx); err != nil {
		return 0, 0, fmt.Errorf("load open drift events: %w", err)
	}
	existingByKey := make(map[string]*models.CatalogDriftEvent, len(existing))
	for i := range existing {
		existingByKey[driftDedupeKeyJob(existing[i])] = &existing[i]
	}

	desiredKeys := make(map[string]struct{}, len(desired))
	inserted := 0
	for i := range desired {
		key := driftDedupeKeyJob(desired[i])
		desiredKeys[key] = struct{}{}
		if _, ok := existingByKey[key]; ok {
			continue
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
