package riverjobs

import (
	"context"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// KindCatalogReconciliationPull is the River kind for the alert-only catalog
// reconciliation loop (issue #209). It pulls the full Stripe catalog AND the
// NMI recurring-plan catalog, diffs them against the OpenRails DB, and records
// drift + orphan events in openrails.catalog_drift_events. It NEVER mutates
// Stripe, NMI, or the catalog rows.
//
// CCBill is intentionally NOT reconciled here: CCBill has no catalog-list API,
// so enumeration is structurally impossible (see the package doc on
// pkg/service/catalog_drift.go). CCBill links stay manual-only.
//
// This worker mirrors the logic in pkg/service.RunCatalogReconciliation, but
// lives in the river package because pkg/service imports internal/river (so the
// worker cannot import back into pkg/service).
const KindCatalogReconciliationPull = "openrails.catalog_reconciliation_pull"

type CatalogReconciliationPullArgs struct{}

func (CatalogReconciliationPullArgs) Kind() string { return KindCatalogReconciliationPull }

// CatalogReconciliationPullWorker runs one pull-and-diff pass across both Stripe
// and NMI. Each provider pass is independently skipped if that processor is
// unconfigured.
type CatalogReconciliationPullWorker struct {
	river.WorkerDefaults[CatalogReconciliationPullArgs]
	DB         *db.DB
	Config     *config.Config
	NMIClients map[string]*nmi.NMIClient
}

func (CatalogReconciliationPullWorker) Kind() string { return KindCatalogReconciliationPull }

func (w CatalogReconciliationPullWorker) Work(ctx context.Context, job *river.Job[CatalogReconciliationPullArgs]) error {
	if w.DB == nil {
		return fmt.Errorf("catalog reconciliation: db not configured")
	}
	if w.Config == nil {
		return fmt.Errorf("catalog reconciliation: config not configured")
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
	var desired []models.CatalogDriftEvent
	scannedProducts, scannedPrices, scannedPlans := 0, 0, 0

	// --- Stripe pass (skipped if unconfigured) ---
	if stripeProc := w.Config.GetStripeProcessor(); stripeProc != nil && stripeProc.SecretKey != "" {
		stripeSvc := &catalog.StripeCatalogService{Config: w.Config}
		products, err := listAllStripeProducts(ctx, stripeSvc)
		if err != nil {
			return err
		}
		prices, err := listAllStripePrices(ctx, stripeSvc)
		if err != nil {
			return err
		}
		scannedProducts, scannedPrices = len(products), len(prices)
		desired = append(desired, computeStripeDriftJob(products, prices, productRows, priceRows, now)...)
	} else {
		log.WithContext(ctx).Info("CatalogReconciliation: stripe not configured; skipping stripe pass")
	}

	// --- NMI pass (skipped if unconfigured) ---
	if client, ok := w.NMIClients["mobius"]; ok && client != nil {
		raw, err := client.GetRecurringPlanData()
		if err != nil {
			return fmt.Errorf("catalog reconciliation: list nmi recurring plans: %w", err)
		}
		plans, err := parseNMIPlansJob(raw)
		if err != nil {
			return err
		}
		scannedPlans = len(plans)
		desired = append(desired, computeNMIDriftJob(plans, priceRows, now)...)
	} else {
		log.WithContext(ctx).Info("CatalogReconciliation: nmi (mobius) not configured; skipping nmi pass")
	}

	for _, e := range desired {
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

	inserted, resolved, err := persistCatalogDriftJob(ctx, w.DB, desired, now)
	if err != nil {
		return err
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"scanned_products":  scannedProducts,
		"scanned_prices":    scannedPrices,
		"scanned_nmi_plans": scannedPlans,
		"new_events":        inserted,
		"resolved_events":   resolved,
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

// computeStripeDriftJob is the worker-side mirror of
// pkg/service.computeCatalogDrift. Kept in sync with that function; the
// duplicate exists only because the import graph (pkg/service -> internal/river)
// forbids the worker from calling back into pkg/service.
func computeStripeDriftJob(
	stripeProducts []catalog.StripeProduct,
	stripePrices []catalog.StripePrice,
	productRows []*models.Product,
	priceRows []*models.Price,
	now time.Time,
) []models.CatalogDriftEvent {
	productByID := make(map[string]*models.Product, len(productRows))
	productBySlug := make(map[string]*models.Product, len(productRows))
	for _, p := range productRows {
		productByID[p.ID.String()] = p
		if slug := strings.TrimSpace(p.Slug); slug != "" {
			productBySlug[slug] = p
		}
	}
	priceByID := make(map[string]*models.Price, len(priceRows))
	priceByContentKey := make(map[string]*models.Price, len(priceRows))
	stripeProductIDs := make(map[string]string)
	stripePriceIDs := make(map[string]string)
	for _, pr := range priceRows {
		priceByID[pr.ID.String()] = pr
		// Content key = "<product_slug>.<currency>.<unit_amount>.<cycle>"; needs
		// the owning product for its slug.
		if prod := productByID[pr.ProductID.String()]; prod != nil {
			if slug := strings.TrimSpace(prod.Slug); slug != "" {
				ck := openRailsPriceContentKeyJob(prod.Slug, pr.Currency, pr.Amount, pr.BillingCycleDays)
				priceByContentKey[ck] = pr
			}
		}
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

	// Match on the CONTENT key (product slug) so it survives DB rebuilds.
	for _, sp := range stripeProducts {
		seenProducts[sp.ID] = struct{}{}
		productKey := strings.TrimSpace(sp.Metadata[catalog.StripeMetadataOpenRailsProductKey])
		if productKey == "" {
			events = append(events, stripeOrphan(models.CatalogDriftResourceProduct, "", sp.ID, now))
			continue
		}
		local, ok := productBySlug[productKey]
		if !ok {
			events = append(events, stripeOrphan(models.CatalogDriftResourceProduct, productKey, sp.ID, now))
			continue
		}
		if strings.TrimSpace(local.DisplayName) != strings.TrimSpace(sp.Name) {
			events = append(events, stripeFieldDrift(models.CatalogDriftResourceProduct, local.ID.String(), sp.ID, "name", local.DisplayName, sp.Name, now))
		}
		if strings.TrimSpace(local.Description) != strings.TrimSpace(sp.Description) {
			events = append(events, stripeFieldDrift(models.CatalogDriftResourceProduct, local.ID.String(), sp.ID, "description", local.Description, sp.Description, now))
		}
		if localActive := local.IsPurchasable(); localActive != sp.Active {
			events = append(events, stripeFieldDrift(models.CatalogDriftResourceProduct, local.ID.String(), sp.ID, "active", strconv.FormatBool(localActive), strconv.FormatBool(sp.Active), now))
		}
	}

	// Match on the CONTENT key (lookup_key / openrails_price_key).
	for _, sp := range stripePrices {
		seenPrices[sp.ID] = struct{}{}
		priceKey := stripePriceContentKeyJob(sp)
		if priceKey == "" {
			events = append(events, stripeOrphan(models.CatalogDriftResourcePrice, "", sp.ID, now))
			continue
		}
		local, ok := priceByContentKey[priceKey]
		if !ok {
			events = append(events, stripeOrphan(models.CatalogDriftResourcePrice, priceKey, sp.ID, now))
			continue
		}
		if local.Amount != sp.UnitAmount {
			events = append(events, stripeFieldDrift(models.CatalogDriftResourcePrice, local.ID.String(), sp.ID, "unit_amount", strconv.FormatInt(local.Amount, 10), strconv.FormatInt(sp.UnitAmount, 10), now))
		}
		if !strings.EqualFold(strings.TrimSpace(local.Currency), strings.TrimSpace(sp.Currency)) {
			events = append(events, stripeFieldDrift(models.CatalogDriftResourcePrice, local.ID.String(), sp.ID, "currency", local.Currency, sp.Currency, now))
		}
		if localActive := local.IsPurchasable(); localActive != sp.Active {
			events = append(events, stripeFieldDrift(models.CatalogDriftResourcePrice, local.ID.String(), sp.ID, "active", strconv.FormatBool(localActive), strconv.FormatBool(sp.Active), now))
		}
	}

	for stripeProductID, orID := range stripeProductIDs {
		if _, seen := seenProducts[stripeProductID]; !seen {
			events = append(events, models.CatalogDriftEvent{Provider: models.CatalogDriftProviderStripe, Kind: models.CatalogDriftMissingInStripe, OpenRailsResourceType: models.CatalogDriftResourceProduct, OpenRailsResourceID: orID, ExternalResourceID: stripeProductID, DetectedAt: now})
		}
	}
	for stripePriceID, orID := range stripePriceIDs {
		if _, seen := seenPrices[stripePriceID]; !seen {
			events = append(events, models.CatalogDriftEvent{Provider: models.CatalogDriftProviderStripe, Kind: models.CatalogDriftMissingInStripe, OpenRailsResourceType: models.CatalogDriftResourcePrice, OpenRailsResourceID: orID, ExternalResourceID: stripePriceID, DetectedAt: now})
		}
	}
	return events
}

// openRailsPriceContentKeyJob mirrors pkg/service.openRailsPriceContentKey:
// the financial content key "<product_slug>.<currency>.<unit_amount>.<cycle>"
// where <cycle> is the billing_cycle_days for a recurring price or "onetime".
func openRailsPriceContentKeyJob(productSlug, currency string, unitAmount int64, billingCycleDays *int) string {
	cycle := "onetime"
	if billingCycleDays != nil && *billingCycleDays > 0 {
		cycle = strconv.Itoa(*billingCycleDays)
	}
	return strings.Join([]string{
		strings.TrimSpace(productSlug),
		strings.ToLower(strings.TrimSpace(currency)),
		strconv.FormatInt(unitAmount, 10),
		cycle,
	}, ".")
}

// stripePriceContentKeyJob mirrors pkg/service.stripePriceContentKey: extract
// the OpenRails financial content key from a Stripe Price, preferring the
// openrails_price_key metadata and falling back to deriving it from the
// lookup_key ("openrails.<content_key>"). Returns "" for prices OpenRails
// doesn't own.
func stripePriceContentKeyJob(sp catalog.StripePrice) string {
	if k := strings.TrimSpace(sp.Metadata[catalog.StripeMetadataOpenRailsPriceKey]); k != "" {
		return k
	}
	const lookupPrefix = "openrails."
	if lk := strings.TrimSpace(sp.LookupKey); strings.HasPrefix(lk, lookupPrefix) {
		return strings.TrimPrefix(lk, lookupPrefix)
	}
	return ""
}

func stripeOrphan(rt models.CatalogDriftResourceType, orID, stripeID string, now time.Time) models.CatalogDriftEvent {
	return models.CatalogDriftEvent{
		Provider:              models.CatalogDriftProviderStripe,
		Kind:                  models.CatalogDriftOrphanInStripe,
		OpenRailsResourceType: rt,
		OpenRailsResourceID:   orID,
		ExternalResourceID:    stripeID,
		DetectedAt:            now,
	}
}

func stripeFieldDrift(rt models.CatalogDriftResourceType, orID, stripeID, field, orVal, stripeVal string, now time.Time) models.CatalogDriftEvent {
	return models.CatalogDriftEvent{
		Provider:              models.CatalogDriftProviderStripe,
		Kind:                  models.CatalogDriftFieldDrift,
		OpenRailsResourceType: rt,
		OpenRailsResourceID:   orID,
		ExternalResourceID:    stripeID,
		Field:                 field,
		OpenRailsValue:        orVal,
		ExternalValue:         stripeVal,
		DetectedAt:            now,
	}
}

// nmiPlanJob is the parsed shape of one NMI recurring plan.
type nmiPlanJob struct {
	PlanID      string
	PlanName    string
	AmountCents int64
}

type nmiRecurringPlansXMLJob struct {
	XMLName xml.Name `xml:"nm_response"`
	Plans   []struct {
		PlanID     string `xml:"plan_id"`
		PlanName   string `xml:"plan_name"`
		PlanAmount string `xml:"plan_amount"`
	} `xml:"plan"`
}

func parseNMIPlansJob(raw string) ([]nmiPlanJob, error) {
	var parsed nmiRecurringPlansXMLJob
	if err := xml.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("catalog reconciliation: parse nmi recurring plans: %w", err)
	}
	out := make([]nmiPlanJob, 0, len(parsed.Plans))
	for _, p := range parsed.Plans {
		out = append(out, nmiPlanJob{
			PlanID:      strings.TrimSpace(p.PlanID),
			PlanName:    p.PlanName,
			AmountCents: dollarStringToCentsJob(p.PlanAmount),
		})
	}
	return out, nil
}

func dollarStringToCentsJob(dollars string) int64 {
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

// computeNMIDriftJob is the worker-side mirror of
// pkg/service.computeNMICatalogDrift.
func computeNMIDriftJob(plans []nmiPlanJob, priceRows []*models.Price, now time.Time) []models.CatalogDriftEvent {
	priceByID := make(map[string]*models.Price, len(priceRows))
	planIDByOpenRailsPrice := make(map[string]string)
	priceByPlanID := make(map[string]string)
	for _, pr := range priceRows {
		priceByID[pr.ID.String()] = pr
		mobius := pr.Processors[string(models.ProcessorMobius)]
		if mobius == nil {
			continue
		}
		if planID := strings.TrimSpace(mobius[models.ProcessorKeyPlanID]); planID != "" {
			planIDByOpenRailsPrice[pr.ID.String()] = planID
			priceByPlanID[planID] = pr.ID.String()
		}
	}

	var events []models.CatalogDriftEvent
	seenPlanIDs := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		if plan.PlanID == "" {
			continue
		}
		seenPlanIDs[plan.PlanID] = struct{}{}
		orPriceID, matched := priceByPlanID[plan.PlanID]
		if !matched {
			// Content-addressed plan_ids carry no price UUID to recover; report
			// the orphan with the external plan_id only (mirror of the service
			// path computeNMICatalogDrift).
			events = append(events, models.CatalogDriftEvent{
				Provider:              models.CatalogDriftProviderNMI,
				Kind:                  models.CatalogDriftOrphanInNMI,
				OpenRailsResourceType: models.CatalogDriftResourcePrice,
				ExternalResourceID:    plan.PlanID,
				DetectedAt:            now,
			})
			continue
		}
		local := priceByID[orPriceID]
		if local == nil {
			continue
		}
		if local.Amount != plan.AmountCents {
			events = append(events, nmiFieldDriftJob(local.ID.String(), plan.PlanID, "plan_amount", strconv.FormatInt(local.Amount, 10), strconv.FormatInt(plan.AmountCents, 10), now))
		}
	}

	for orPriceID, planID := range planIDByOpenRailsPrice {
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

func nmiFieldDriftJob(orPriceID, planID, field, orVal, nmiVal string, now time.Time) models.CatalogDriftEvent {
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

func driftDedupeKeyJob(e models.CatalogDriftEvent) string {
	return strings.Join([]string{string(e.Provider), string(e.Kind), string(e.OpenRailsResourceType), e.OpenRailsResourceID, e.ExternalResourceID, e.Field}, "|")
}

// persistCatalogDriftJob inserts new divergences and auto-resolves open rows
// whose divergence is gone. Idempotent. Returns (inserted, resolved).
func persistCatalogDriftJob(ctx context.Context, database *db.DB, desired []models.CatalogDriftEvent, now time.Time) (int, int, error) {
	q := database.Gen(ctx)

	openRows, err := q.ListOpenCatalogDriftEvents(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("load open drift events: %w", err)
	}
	existing := make([]models.CatalogDriftEvent, 0, len(openRows))
	for _, r := range openRows {
		existing = append(existing, catalogDriftEventFromGen(r))
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
			row.ID = uuidutil.NewV7()
		}
		if err := q.InsertCatalogDriftEvent(ctx, gen.InsertCatalogDriftEventParams{
			ID:                    row.ID,
			Provider:              string(row.Provider),
			Kind:                  string(row.Kind),
			OpenrailsResourceType: string(row.OpenRailsResourceType),
			OpenrailsResourceID:   nilIfEmptyStr(row.OpenRailsResourceID),
			ExternalResourceID:    nilIfEmptyStr(row.ExternalResourceID),
			Field:                 nilIfEmptyStr(row.Field),
			OpenrailsValue:        nilIfEmptyStr(row.OpenRailsValue),
			ExternalValue:         nilIfEmptyStr(row.ExternalValue),
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
		if err := q.ResolveCatalogDriftEvent(ctx, gen.ResolveCatalogDriftEventParams{
			ID: row.ID, ResolvedAt: &now,
		}); err != nil {
			return inserted, resolved, fmt.Errorf("resolve drift event: %w", err)
		}
		resolved++
	}
	return inserted, resolved, nil
}

// catalogDriftEventFromGen maps the generated row onto the domain model
// (NULL text columns flatten to "" like the bun nullzero tags did).
func catalogDriftEventFromGen(r gen.BillingCatalogDriftEvent) models.CatalogDriftEvent {
	return models.CatalogDriftEvent{
		ID:                    r.ID,
		Provider:              models.CatalogDriftProvider(r.Provider),
		Kind:                  models.CatalogDriftKind(r.Kind),
		OpenRailsResourceType: models.CatalogDriftResourceType(r.OpenrailsResourceType),
		OpenRailsResourceID:   derefStr(r.OpenrailsResourceID),
		ExternalResourceID:    derefStr(r.ExternalResourceID),
		Field:                 derefStr(r.Field),
		OpenRailsValue:        derefStr(r.OpenrailsValue),
		ExternalValue:         derefStr(r.ExternalValue),
		DetectedAt:            r.DetectedAt,
		ResolvedAt:            r.ResolvedAt,
	}
}

func nilIfEmptyStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
