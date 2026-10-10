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

func derefText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
