package service

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Catalog reconciliation (issue #209) runs the shared catalog.RunDriftPass:
// alert-only standing findings per immutable PSP account. Operators resolve
// drift through per-price/product reconcile, which closes only findings of an
// account it verified in sync.

// CatalogDriftReport is the result of a reconciliation pass.
type CatalogDriftReport struct {
	ScannedProducts    int                     `json:"scanned_products"`
	ScannedPrices      int                     `json:"scanned_prices"`
	ScannedNMIPlans    int                     `json:"scanned_nmi_plans"`
	ScannedSolanaPlans int                     `json:"scanned_solana_plans"`
	OpenEvents         []CatalogDriftEventView `json:"open_events"`
	// NewEvents counts findings this pass opened; ignored identities do not count.
	NewEvents int `json:"new_events"`
	// ResolvedEvents counts open findings a complete provider read proved gone.
	ResolvedEvents int `json:"resolved_events"`
}

// CatalogDriftEventView is the API-facing shape of a drift finding.
type CatalogDriftEventView struct {
	ID                    uuid.UUID  `json:"id"`
	PSPID                 uuid.UUID  `json:"psp_id"`
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

// RunCatalogReconciliation reads the active Stripe and NMI accounts completely
// and verifies stored Solana plans, then persists standing findings. Idempotent
// and alert-only.
func (s *Service) RunCatalogReconciliation(ctx context.Context) (*CatalogDriftReport, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()
	cfg, err := s.requireConfig()
	if err != nil {
		return nil, err
	}
	dbi, err := s.requireDB()
	if err != nil {
		return nil, err
	}
	var sources catalog.DriftSources
	if s.rt.RailConfigs != nil {
		if stripe, ok, err := catalog.ActiveDriftPSP(ctx, s.rt.RailConfigs, models.RailStripe); err != nil {
			return nil, err
		} else if ok {
			sources.StripePSPID = stripe.ID
			sources.Stripe = catalog.PinnedStripeLister{PSPID: stripe.ID, Service: &catalog.StripeCatalogService{StripeClients: s.rt.StripeClients, Config: cfg, Rails: s.rt.RailConfigs}}
		}
		if account, ok, err := catalog.ActiveDriftPSP(ctx, s.rt.RailConfigs, models.RailNMI); err != nil {
			return nil, err
		} else if ok && s.rt.CollectionResolver != nil {
			mid, err := merchant.Require(ctx)
			if err != nil {
				return nil, err
			}
			client, armed, err := s.rt.CollectionResolver.ResolveNMIClient(ctx, mid.UUID(), &account.ID)
			if err != nil {
				return nil, fmt.Errorf("catalog drift: resolve nmi client: %w", err)
			}
			if armed {
				sources.NMIPSPID, sources.NMI = account.ID, client
			}
		}
		if s.railArmed(ctx, string(models.RailSolana)) {
			adapter := &solanaAdapter{svc: s}
			sources.Solana = func(ctx context.Context, link map[string]string, _ *models.Price) ([]catalog.DriftFieldValue, bool, error) {
				fields, missing, err := adapter.Verify(ctx, link, nil)
				out := make([]catalog.DriftFieldValue, 0, len(fields))
				for _, f := range fields {
					out = append(out, catalog.DriftFieldValue{Field: f.Field, OpenRailsValue: f.OpenRailsValue, ExternalValue: f.RemoteValue})
				}
				return out, missing, err
			}
		}
	}
	if sources.Stripe == nil && sources.NMI == nil && sources.Solana == nil {
		return nil, fmt.Errorf("no catalog provider (stripe/nmi/solana) is configured")
	}
	pass, err := catalog.RunDriftPass(ctx, dbi, sources, s.now().UTC())
	if err != nil {
		return nil, err
	}
	open, _, err := s.ListCatalogDrift(ctx, CatalogDriftFilter{})
	if err != nil {
		return nil, err
	}
	return &CatalogDriftReport{
		ScannedProducts: pass.ScannedProducts, ScannedPrices: pass.ScannedPrices,
		ScannedNMIPlans: pass.ScannedNMIPlans, ScannedSolanaPlans: pass.ScannedSolanaPlans,
		OpenEvents: open, NewEvents: pass.NewEvents, ResolvedEvents: pass.ResolvedEvents,
	}, nil
}

func driftEventFromGen(r gen.OpenrailsCatalogDriftEvent) CatalogDriftEventView {
	view := CatalogDriftEventView{
		ID: r.ID, Provider: r.Rail, Kind: r.Kind, OpenRailsResourceType: r.OpenrailsResourceType,
		OpenRailsResourceID: derefText(r.OpenrailsResourceID), ExternalResourceID: derefText(r.ExternalResourceID),
		Field: derefText(r.Field), OpenRailsValue: derefText(r.OpenrailsValue), ExternalValue: derefText(r.ExternalValue),
		DetectedAt: r.DetectedAt, ResolvedAt: r.ResolvedAt,
	}
	if r.PspID != nil {
		view.PSPID = *r.PspID
	}
	return view
}

func driftPageInt32(v int) int32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
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

// ListCatalogDrift returns open drift findings with pagination and optional
// provider / kind / resource_type filters. Total is the unpaginated count.
func (s *Service) ListCatalogDrift(ctx context.Context, filter CatalogDriftFilter) (items []CatalogDriftEventView, total int64, err error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, 0, pinErr
	}
	defer release()
	dbi, err := s.requireDB()
	if err != nil {
		return nil, 0, err
	}
	q := dbi.Gen(ctx)
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	offset := max(filter.Offset, 0)
	provider := nilIfEmptyText(strings.TrimSpace(filter.Rail))
	kind := nilIfEmptyText(strings.TrimSpace(filter.Kind))
	resourceType := nilIfEmptyText(strings.TrimSpace(filter.ResourceType))
	total, err = q.CountOpenCatalogDriftFiltered(ctx, gen.CountOpenCatalogDriftFilteredParams{Rail: provider, Kind: kind, ResourceType: resourceType})
	if err != nil {
		return nil, 0, fmt.Errorf("count drift events: %w", err)
	}
	rows, err := q.ListOpenCatalogDriftFiltered(ctx, gen.ListOpenCatalogDriftFilteredParams{
		Rail: provider, Kind: kind, ResourceType: resourceType, Column1: driftPageInt32(limit), Column2: driftPageInt32(offset),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list drift events: %w", err)
	}
	out := make([]CatalogDriftEventView, 0, len(rows))
	for _, row := range rows {
		out = append(out, driftEventFromGen(row))
	}
	return out, total, nil
}

// ResolveDriftForResource closes open findings of one PSP account for a local
// resource after reconcile verified that account in sync. Returns rows closed.
func (s *Service) ResolveDriftForResource(ctx context.Context, pspID uuid.UUID, resourceType models.CatalogDriftResourceType, openRailsResourceID string) (int, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return 0, pinErr
	}
	defer release()
	openRailsResourceID = strings.TrimSpace(openRailsResourceID)
	if openRailsResourceID == "" || pspID == uuid.Nil {
		return 0, nil
	}
	dbi, err := s.requireDB()
	if err != nil {
		return 0, err
	}
	n, err := dbi.Gen(ctx).ResolveCatalogDriftForResource(ctx, gen.ResolveCatalogDriftForResourceParams{
		ResolvedAt: s.now().UTC(), PspID: pspID, OpenrailsResourceType: string(resourceType), OpenrailsResourceID: openRailsResourceID,
	})
	if err != nil {
		return 0, fmt.Errorf("resolve drift for resource: %w", err)
	}
	return int(n), nil
}

// CountOpenDriftByKind returns open drift counts keyed "<provider>/<kind>" for
// the openrails_catalog_drift_open_count metric.
func (s *Service) CountOpenDriftByKind(ctx context.Context) (map[string]int64, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()
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
