package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Catalog reconciliation (issue #209) runs the shared catalog.RunDriftPass:
// alert-only standing findings per immutable PSP account. Operators resolve
// drift through per-price/product reconcile, which closes only findings of an
// account it verified in sync.

// RunCatalogReconciliation reads the active Stripe and NMI accounts completely
// and verifies stored Solana plans, then persists standing findings. Idempotent
// and alert-only.
func (s *Service) RunCatalogReconciliation(ctx context.Context) (*billing.CatalogDriftRefresh, error) {
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
	open, err := dbi.Gen(ctx).CountOpenCatalogDriftFiltered(ctx, gen.CountOpenCatalogDriftFilteredParams{})
	if err != nil {
		return nil, fmt.Errorf("count drift findings: %w", err)
	}
	return &billing.CatalogDriftRefresh{
		ScannedProducts: pass.ScannedProducts, ScannedPrices: pass.ScannedPrices,
		ScannedNMIPlans: pass.ScannedNMIPlans, ScannedSolanaPlans: pass.ScannedSolanaPlans,
		OpenedFindings: pass.NewEvents, ResolvedFindings: pass.ResolvedEvents, OpenFindings: int(open),
	}, nil
}

// catalogDrift reads one catalog.* reconciliation finding.
func catalogDrift(r gen.BillingReconciliationFinding) billing.CatalogDrift {
	view := billing.CatalogDrift{
		ID: billing.FindingID(r.ID), Rail: derefText(r.Rail), Kind: strings.TrimPrefix(r.FindingType, "catalog."), ResourceType: derefText(r.OpenrailsResourceType),
		ResourceID: derefText(r.OpenrailsResourceID), ExternalResourceID: derefText(r.ExternalResourceID),
		Field: derefText(r.Field), OpenRailsValue: derefText(r.OpenrailsValue), ExternalValue: derefText(r.ExternalValue),
		DetectedAt: r.CreatedAt, ResolvedAt: r.ResolvedAt,
	}
	if r.PspID != nil {
		view.PSPID = billing.PSPID(*r.PspID)
	}
	return view
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

// ListCatalogDrift returns one page of open drift findings, newest first.
func (s *Service) ListCatalogDrift(ctx context.Context, params billing.CatalogDriftListParams) (billing.ListPage[billing.CatalogDrift], error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return billing.ListPage[billing.CatalogDrift]{}, pinErr
	}
	defer release()
	dbi, err := s.requireDB()
	if err != nil {
		return billing.ListPage[billing.CatalogDrift]{}, err
	}
	if params.IDs != nil {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return billing.ListPage[billing.CatalogDrift]{}, err
		}
		rows, err := dbi.Gen(ctx).ListCatalogDriftByIDs(ctx, gen.ListCatalogDriftByIDsParams{MerchantID: mid.UUID(), Ids: uuidutil.Of(params.IDs)})
		if err != nil {
			return billing.ListPage[billing.CatalogDrift]{}, fmt.Errorf("list drift findings: %w", err)
		}
		return pagination.Map(billing.ListPage[gen.BillingReconciliationFinding]{Items: rows}, catalogDrift), nil
	}
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.CatalogDrift]{}, err
	}
	afterAt, afterID, err := pagination.After(params.Cursor)
	if err != nil {
		return billing.ListPage[billing.CatalogDrift]{}, err
	}
	rows, err := dbi.Gen(ctx).ListOpenCatalogDriftFiltered(ctx, gen.ListOpenCatalogDriftFilteredParams{
		Rail: nilIfEmptyText(strings.TrimSpace(params.Rail)), Kind: nilIfEmptyText(strings.TrimSpace(params.Kind)),
		ResourceType: nilIfEmptyText(strings.TrimSpace(params.ResourceType)),
		AfterAt:      afterAt, AfterID: afterID, FetchLimit: pagination.Fetch(limit),
	})
	if err != nil {
		return billing.ListPage[billing.CatalogDrift]{}, fmt.Errorf("list drift findings: %w", err)
	}
	page := pagination.Cut(rows, limit, func(r gen.BillingReconciliationFinding) any { return pagination.TimeID{At: r.CreatedAt, ID: r.ID} })
	return pagination.Map(page, catalogDrift), nil
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
		out[derefText(r.Rail)+"/"+r.Kind] = r.N
	}
	return out, nil
}
