package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// Catalog reconciliation (issue #209) runs the shared catalog.RunDriftPass:
// alert-only standing findings per immutable PSP account. Operators resolve
// drift through per-price/product reconcile, which closes only findings of an
// account it verified in sync.

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
