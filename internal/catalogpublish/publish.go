package catalogpublish

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Publish is the single complete declaration path for HTTP and runtime tooling.
// The planner/executor interface is private: no caller can omit billing definitions.
func Publish(ctx context.Context, svc *service.Service, req openrails.CatalogPublishRequest) (*openrails.CatalogPublishResponse, error) {
	if err := req.Catalog.Validate(); err != nil {
		return nil, apperr.Invalidf("%s", err)
	}
	applier := svc
	planned, err := plan(ctx, applier, &req.Catalog)
	if err != nil {
		return nil, fmt.Errorf("plan catalog: %w", err)
	}
	billing, err := catalogBilling(&req.Catalog)
	if err != nil {
		return nil, err
	}
	planned.MetersChanged, planned.RateCardsChanged, err = svc.PlanCatalogBilling(ctx, billing)
	if err != nil {
		return nil, fmt.Errorf("plan catalog billing: %w", err)
	}
	response := &openrails.CatalogPublishResponse{Plan: planned}
	if !req.Insert && !req.Overwrite && !req.Prune {
		return response, nil
	}
	response.Result, err = applyWithOptions(ctx, applier, planned, ApplyOptions{req.Insert, req.Overwrite, req.Prune})
	if err != nil {
		return nil, fmt.Errorf("apply catalog: %w", err)
	}
	err = svc.SyncCatalogSidecars(ctx, billing, service.CatalogMutationOptions{Insert: req.Insert, Overwrite: req.Overwrite, Prune: req.Prune})
	if err != nil {
		return nil, fmt.Errorf("apply catalog billing: %w", err)
	}
	return response, nil
}
