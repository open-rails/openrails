package catalog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Catalog drift is alert-only: a pass reads provider catalogs, compares them
// with local products/prices and records standing findings. It never mutates a
// provider or the catalog. Every finding belongs to one immutable PSP account,
// and only a successful read of that account can resolve it. CCBill has no
// catalog-list API and is never compared.

// StripeCatalogLister pages a Stripe account's products and prices.
type StripeCatalogLister interface {
	ListProducts(ctx context.Context, startingAfter string) ([]StripeProduct, string, error)
	ListPrices(ctx context.Context, startingAfter string) ([]StripePrice, string, error)
}

// NMIPlanLister lists an NMI account's recurring plans.
type NMIPlanLister interface {
	ListRecurringPlans(ctx context.Context) ([]nmi.V5Plan, error)
}

// DriftFieldValue is one diverging field of a per-resource verification.
type DriftFieldValue struct {
	Field          string
	OpenRailsValue string
	ExternalValue  string
}

// SolanaPlanVerifier reads one stored on-chain plan. An error is an unknown
// outcome and is never treated as absence or agreement.
type SolanaPlanVerifier func(ctx context.Context, link map[string]string, price *models.Price) (fields []DriftFieldValue, missing bool, err error)

// DriftSources names the provider reads of one pass. A nil source is skipped:
// it can neither add nor resolve findings.
type DriftSources struct {
	StripePSPID uuid.UUID
	Stripe      StripeCatalogLister
	NMIPSPID    uuid.UUID
	NMI         NMIPlanLister
	Solana      SolanaPlanVerifier
}

// DriftReport summarizes one pass.
type DriftReport struct {
	ScannedProducts    int
	ScannedPrices      int
	ScannedNMIPlans    int
	ScannedSolanaPlans int
	NewEvents          int
	ResolvedEvents     int
}

// ActiveDriftPSP resolves the account that receives new catalog work on rail.
// Unarmed rails report ok=false; resolution failures are errors.
func ActiveDriftPSP(ctx context.Context, rails railresolve.Source, rail models.Rail) (*config.PSPConfig, bool, error) {
	if rails == nil {
		return nil, false, nil
	}
	armed, err := rails.Armed(ctx, string(rail))
	if err != nil {
		return nil, false, fmt.Errorf("catalog drift: resolve %s account: %w", rail, err)
	}
	if !armed {
		return nil, false, nil
	}
	proc, err := rails.RailConfig(ctx, string(rail), "")
	if errors.Is(err, railresolve.ErrRailNotArmed) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("catalog drift: resolve %s account: %w", rail, err)
	}
	if proc == nil || proc.ID == uuid.Nil {
		return nil, false, fmt.Errorf("catalog drift: %s account has no PSP identity", rail)
	}
	return proc, true, nil
}

// PinnedStripeLister reads exactly one Stripe account.
type PinnedStripeLister struct {
	PSPID   uuid.UUID
	Service *StripeCatalogService
}

func (l PinnedStripeLister) ListProducts(ctx context.Context, startingAfter string) ([]StripeProduct, string, error) {
	return l.Service.ListProducts(db.WithPSPID(ctx, l.PSPID), startingAfter)
}

func (l PinnedStripeLister) ListPrices(ctx context.Context, startingAfter string) ([]StripePrice, string, error) {
	return l.Service.ListPrices(db.WithPSPID(ctx, l.PSPID), startingAfter)
}

// RunDriftPass reads every configured source, then persists findings with the
// coverage those reads prove. A failed complete read aborts the pass.
func RunDriftPass(ctx context.Context, database *db.DB, sources DriftSources, now time.Time) (DriftReport, error) {
	var report DriftReport
	products, err := NewProductService(database).GetAll(ctx)
	if err != nil {
		return report, fmt.Errorf("catalog drift: load products: %w", err)
	}
	prices, err := NewPriceService(database).GetAll(ctx)
	if err != nil {
		return report, fmt.Errorf("catalog drift: load prices: %w", err)
	}
	var desired []models.CatalogDriftEvent
	var coverage []DriftCoverage
	if sources.Stripe != nil {
		if sources.StripePSPID == uuid.Nil {
			return report, errors.New("catalog drift: stripe source has no PSP identity")
		}
		remoteProducts, remotePrices, err := FetchStripeCatalog(ctx, sources.Stripe)
		if err != nil {
			return report, err
		}
		report.ScannedProducts, report.ScannedPrices = len(remoteProducts), len(remotePrices)
		snap := BuildDriftSnapshot(products, prices, sources.StripePSPID)
		desired = append(desired, forPSP(ComputeStripeDrift(remoteProducts, remotePrices, snap, now), sources.StripePSPID)...)
		coverage = append(coverage, DriftCoverage{PSPID: sources.StripePSPID})
	}
	if sources.NMI != nil {
		if sources.NMIPSPID == uuid.Nil {
			return report, errors.New("catalog drift: nmi source has no PSP identity")
		}
		remotePlans, err := sources.NMI.ListRecurringPlans(ctx)
		if err != nil {
			return report, fmt.Errorf("catalog drift: list nmi recurring plans: %w", err)
		}
		plans := MapNMIPlans(remotePlans)
		report.ScannedNMIPlans = len(plans)
		snap := BuildDriftSnapshot(products, prices, sources.NMIPSPID)
		desired = append(desired, forPSP(ComputeNMIDrift(plans, snap, now), sources.NMIPSPID)...)
		coverage = append(coverage, DriftCoverage{PSPID: sources.NMIPSPID})
	}
	if sources.Solana != nil {
		events, covered, scanned := verifySolanaPlans(ctx, sources.Solana, prices, now)
		desired = append(desired, events...)
		coverage = append(coverage, covered...)
		report.ScannedSolanaPlans = scanned
	}
	for _, e := range desired {
		log.WithContext(ctx).WithFields(log.Fields{
			"event": "catalog_drift", "psp_id": e.PSPID.String(), "provider": string(e.Provider), "kind": string(e.Kind),
			"openrails_resource_type": string(e.OpenRailsResourceType), "openrails_resource_id": e.OpenRailsResourceID,
			"external_resource_id": e.ExternalResourceID, "field": e.Field,
		}).Warn("catalog reconciliation detected drift")
	}
	report.NewEvents, report.ResolvedEvents, err = PersistDrift(ctx, database, desired, coverage, now)
	return report, err
}

func forPSP(events []models.CatalogDriftEvent, pspID uuid.UUID) []models.CatalogDriftEvent {
	for i := range events {
		events[i].PSPID = pspID
	}
	return events
}

// verifySolanaPlans checks each stored plan link. Solana has no plan listing,
// so a successful read proves only that one price on that account.
func verifySolanaPlans(ctx context.Context, verify SolanaPlanVerifier, prices []*models.Price, now time.Time) ([]models.CatalogDriftEvent, []DriftCoverage, int) {
	var events []models.CatalogDriftEvent
	var coverage []DriftCoverage
	scanned := 0
	for _, price := range prices {
		for _, link := range price.PSPLinksForRail(models.RailSolana) {
			pspID, err := uuid.Parse(strings.TrimSpace(link[models.RailKeyPSPID]))
			if err != nil || pspID == uuid.Nil {
				log.WithContext(ctx).WithField("price_id", price.ID.String()).Warn("catalog drift: solana link has no PSP identity")
				continue
			}
			scanned++
			fields, missing, err := verify(ctx, link, price)
			if err != nil {
				log.WithContext(ctx).WithFields(log.Fields{"event": "catalog_drift", "provider": "solana", "price_id": price.ID.String()}).
					WithError(err).Warn("solana catalog drift check failed")
				continue
			}
			resource := DriftResource{Type: models.CatalogDriftResourcePrice, ID: price.ID.String()}
			coverage = append(coverage, DriftCoverage{PSPID: pspID, Resource: &resource})
			planPDA := strings.TrimSpace(link["plan_pda"])
			if missing {
				events = append(events, models.CatalogDriftEvent{PSPID: pspID, Provider: models.CatalogDriftProviderSolana,
					Kind: models.CatalogDriftMissingInSolana, OpenRailsResourceType: models.CatalogDriftResourcePrice,
					OpenRailsResourceID: price.ID.String(), ExternalResourceID: planPDA, DetectedAt: now})
				continue
			}
			for _, field := range fields {
				events = append(events, models.CatalogDriftEvent{PSPID: pspID, Provider: models.CatalogDriftProviderSolana,
					Kind: models.CatalogDriftFieldDrift, OpenRailsResourceType: models.CatalogDriftResourcePrice,
					OpenRailsResourceID: price.ID.String(), ExternalResourceID: planPDA, Field: field.Field,
					OpenRailsValue: field.OpenRailsValue, ExternalValue: field.ExternalValue, DetectedAt: now})
			}
		}
	}
	return events, coverage, scanned
}

// DriftSnapshot is the local catalog as seen by one PSP account. Remote objects
// match by content key; stored provider ids come only from that account's links.
type DriftSnapshot struct {
	ProductByID       map[string]*models.Product
	PriceByID         map[string]*models.Price
	ProductByKey      map[string]*models.Product
	PriceByContentKey map[string]*models.Price
	StripeProductIDs  map[string]string // stripe product id -> local product id
	StripePriceIDs    map[string]string // stripe price id -> local price id
	NMIPlanByPriceID  map[string]string // local price id -> nmi plan id
}

// BuildDriftSnapshot is pure; tests use it without a database. A zero pspID
// includes links of every account (the catalog-extras view).
func BuildDriftSnapshot(products []*models.Product, prices []*models.Price, pspID uuid.UUID) DriftSnapshot {
	snap := DriftSnapshot{
		ProductByID:       make(map[string]*models.Product, len(products)),
		PriceByID:         make(map[string]*models.Price, len(prices)),
		ProductByKey:      make(map[string]*models.Product, len(products)),
		PriceByContentKey: make(map[string]*models.Price, len(prices)),
		StripeProductIDs:  map[string]string{},
		StripePriceIDs:    map[string]string{},
		NMIPlanByPriceID:  map[string]string{},
	}
	for _, p := range products {
		snap.ProductByID[p.ID.String()] = p
		if key := strings.TrimSpace(p.Key); key != "" {
			snap.ProductByKey[key] = p
		}
	}
	for _, pr := range prices {
		snap.PriceByID[pr.ID.String()] = pr
		if prod := snap.ProductByID[pr.ProductID.String()]; prod != nil && strings.TrimSpace(prod.Key) != "" {
			snap.PriceByContentKey[OpenRailsPriceContentKey(prod.Key, pr.Currency, pr.Amount, pr.RecurringCycleDays())] = pr
		}
		account := pr
		if pspID != uuid.Nil {
			account = pr.ForPSP(pspID)
		}
		if stripe := account.PSPLinkForRail(models.RailStripe); stripe != nil {
			if id := strings.TrimSpace(stripe[models.RailKeyStripePriceID]); id != "" {
				snap.StripePriceIDs[id] = pr.ID.String()
			}
			if id := strings.TrimSpace(stripe[models.RailKeyStripeProductID]); id != "" {
				snap.StripeProductIDs[id] = pr.ProductID.String()
			}
		}
		for _, link := range account.PSPLinksForRail(models.RailNMI) {
			if planID := strings.TrimSpace(link[models.RailKeyPlanID]); planID != "" {
				snap.NMIPlanByPriceID[pr.ID.String()] = planID
			}
		}
	}
	return snap
}

// FetchStripeCatalog pages through all products and prices of one account.
func FetchStripeCatalog(ctx context.Context, lister StripeCatalogLister) ([]StripeProduct, []StripePrice, error) {
	var products []StripeProduct
	for cursor := ""; ; {
		page, next, err := lister.ListProducts(ctx, cursor)
		if err != nil {
			return nil, nil, fmt.Errorf("catalog drift: list stripe products: %w", err)
		}
		products = append(products, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	var prices []StripePrice
	for cursor := ""; ; {
		page, next, err := lister.ListPrices(ctx, cursor)
		if err != nil {
			return nil, nil, fmt.Errorf("catalog drift: list stripe prices: %w", err)
		}
		prices = append(prices, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return products, prices, nil
}

// ComputeStripeDrift returns the Stripe findings that should be open for one
// complete account read. Pure and idempotent.
func ComputeStripeDrift(remoteProducts []StripeProduct, remotePrices []StripePrice, snap DriftSnapshot, now time.Time) []models.CatalogDriftEvent {
	var events []models.CatalogDriftEvent
	stripeEvent := func(kind models.CatalogDriftKind, resource models.CatalogDriftResourceType, localID, externalID string) models.CatalogDriftEvent {
		return models.CatalogDriftEvent{Provider: models.CatalogDriftProviderStripe, Kind: kind, OpenRailsResourceType: resource,
			OpenRailsResourceID: localID, ExternalResourceID: externalID, DetectedAt: now}
	}
	fieldDrift := func(resource models.CatalogDriftResourceType, localID, externalID, field, local, remote string) {
		e := stripeEvent(models.CatalogDriftFieldDrift, resource, localID, externalID)
		e.Field, e.OpenRailsValue, e.ExternalValue = field, local, remote
		events = append(events, e)
	}
	seenProducts := make(map[string]struct{}, len(remoteProducts))
	for _, sp := range remoteProducts {
		seenProducts[sp.ID] = struct{}{}
		productKey := strings.TrimSpace(sp.Metadata[StripeMetadataOpenRailsProductKey])
		local, ok := snap.ProductByKey[productKey]
		if productKey == "" || !ok {
			events = append(events, stripeEvent(models.CatalogDriftOrphanInStripe, models.CatalogDriftResourceProduct, productKey, sp.ID))
			continue
		}
		id := local.ID.String()
		if strings.TrimSpace(local.DisplayName) != strings.TrimSpace(sp.Name) {
			fieldDrift(models.CatalogDriftResourceProduct, id, sp.ID, "name", local.DisplayName, sp.Name)
		}
		if strings.TrimSpace(local.Description) != strings.TrimSpace(sp.Description) {
			fieldDrift(models.CatalogDriftResourceProduct, id, sp.ID, "description", local.Description, sp.Description)
		}
		if active := local.IsPurchasable(); active != sp.Active {
			fieldDrift(models.CatalogDriftResourceProduct, id, sp.ID, "active", strconv.FormatBool(active), strconv.FormatBool(sp.Active))
		}
	}
	seenPrices := make(map[string]struct{}, len(remotePrices))
	for _, sp := range remotePrices {
		seenPrices[sp.ID] = struct{}{}
		priceKey := RemoteStripePriceContentKey(sp)
		local, ok := snap.PriceByContentKey[priceKey]
		if priceKey == "" || !ok {
			events = append(events, stripeEvent(models.CatalogDriftOrphanInStripe, models.CatalogDriftResourcePrice, priceKey, sp.ID))
			continue
		}
		id := local.ID.String()
		remoteNative, err := moneyutil.RailMinorToNative(sp.Currency, moneyutil.Cents(sp.UnitAmount))
		if err != nil {
			// A provider currency that is not in the system registry cannot be
			// compared safely. Record the currency drift and leave the amount
			// unresolved rather than applying a USD/cent scale by guesswork.
			fieldDrift(models.CatalogDriftResourcePrice, id, sp.ID, "currency", local.Currency, sp.Currency)
		} else if local.Amount != remoteNative {
			fieldDrift(models.CatalogDriftResourcePrice, id, sp.ID, "unit_amount", strconv.FormatInt(local.Amount, 10), strconv.FormatInt(remoteNative, 10))
		}
		if !strings.EqualFold(strings.TrimSpace(local.Currency), strings.TrimSpace(sp.Currency)) {
			fieldDrift(models.CatalogDriftResourcePrice, id, sp.ID, "currency", local.Currency, sp.Currency)
		}
		if active := local.IsPurchasable(); active != sp.Active {
			fieldDrift(models.CatalogDriftResourcePrice, id, sp.ID, "active", strconv.FormatBool(active), strconv.FormatBool(sp.Active))
		}
	}
	for stripeID, localID := range snap.StripeProductIDs {
		if _, ok := seenProducts[stripeID]; !ok {
			events = append(events, stripeEvent(models.CatalogDriftMissingInStripe, models.CatalogDriftResourceProduct, localID, stripeID))
		}
	}
	for stripeID, localID := range snap.StripePriceIDs {
		if _, ok := seenPrices[stripeID]; !ok {
			events = append(events, stripeEvent(models.CatalogDriftMissingInStripe, models.CatalogDriftResourcePrice, localID, stripeID))
		}
	}
	return events
}

// NMIPlan is the comparable shape of one NMI recurring plan.
type NMIPlan struct {
	PlanID      string
	PlanName    string
	AmountCents int64
}

// MapNMIPlans parses provider amounts exactly. An unparseable amount skips the
// plan rather than becoming a zero-dollar plan.
func MapNMIPlans(plans []nmi.V5Plan) []NMIPlan {
	out := make([]NMIPlan, 0, len(plans))
	for _, p := range plans {
		cents, err := moneyutil.ParseDecimalToCents(p.PlanAmount)
		if err != nil {
			log.WithError(err).WithFields(log.Fields{"plan_id": p.ID, "plan_amount": p.PlanAmount}).
				Warn("nmi catalog drift: skipping plan with unparseable amount")
			continue
		}
		out = append(out, NMIPlan{PlanID: strings.TrimSpace(p.ID), PlanName: p.PlanName, AmountCents: int64(cents)})
	}
	return out
}

// ComputeNMIDrift returns the NMI findings that should be open for one complete
// account read. Plans match local prices by that account's stored plan id.
func ComputeNMIDrift(plans []NMIPlan, snap DriftSnapshot, now time.Time) []models.CatalogDriftEvent {
	priceByPlan := make(map[string]string, len(snap.NMIPlanByPriceID))
	for priceID, planID := range snap.NMIPlanByPriceID {
		priceByPlan[planID] = priceID
	}
	var events []models.CatalogDriftEvent
	seen := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		if plan.PlanID == "" {
			continue
		}
		seen[plan.PlanID] = struct{}{}
		priceID, ok := priceByPlan[plan.PlanID]
		if !ok {
			events = append(events, models.CatalogDriftEvent{Provider: models.CatalogDriftProviderNMI, Kind: models.CatalogDriftOrphanInNMI,
				OpenRailsResourceType: models.CatalogDriftResourcePrice, ExternalResourceID: plan.PlanID, DetectedAt: now})
			continue
		}
		local := snap.PriceByID[priceID]
		if local == nil {
			continue
		}
		remoteMicros := int64(moneyutil.CentsToMicros(moneyutil.Cents(plan.AmountCents)))
		if local.Amount != remoteMicros {
			events = append(events, models.CatalogDriftEvent{Provider: models.CatalogDriftProviderNMI, Kind: models.CatalogDriftFieldDrift,
				OpenRailsResourceType: models.CatalogDriftResourcePrice, OpenRailsResourceID: priceID, ExternalResourceID: plan.PlanID,
				Field: "plan_amount", OpenRailsValue: strconv.FormatInt(local.Amount, 10), ExternalValue: strconv.FormatInt(remoteMicros, 10), DetectedAt: now})
		}
	}
	for priceID, planID := range snap.NMIPlanByPriceID {
		if _, ok := seen[planID]; !ok {
			events = append(events, models.CatalogDriftEvent{Provider: models.CatalogDriftProviderNMI, Kind: models.CatalogDriftMissingInNMI,
				OpenRailsResourceType: models.CatalogDriftResourcePrice, OpenRailsResourceID: priceID, ExternalResourceID: planID, DetectedAt: now})
		}
	}
	return events
}
