package openrails

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
)

// PreviewPlanMigration classifies the affected subscriptions without writing.
func (c *Client) PreviewPlanMigration(ctx context.Context, request billing.PlanMigrationRequest, requestOptions ...RequestOption) (*billing.PlanMigrationResult, error) {
	if err := requirePlanMigrationPrices(request); err != nil {
		return nil, err
	}
	var out billing.PlanMigrationResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/plan-migrations/preview", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreatePlanMigration schedules the migration and records its batch.
func (c *Client) CreatePlanMigration(ctx context.Context, request billing.PlanMigrationRequest, requestOptions ...RequestOption) (*billing.PlanMigrationResult, error) {
	if err := requirePlanMigrationPrices(request); err != nil {
		return nil, err
	}
	var out billing.PlanMigrationResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/plan-migrations", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// requirePlanMigrationPrices is the server's first check, applied before any I/O.
func requirePlanMigrationPrices(r billing.PlanMigrationRequest) error {
	if strings.TrimSpace(r.SourcePrice) == "" || strings.TrimSpace(r.TargetPrice) == "" {
		return invalidErr("source_price and target_price required")
	}
	return nil
}

// CancelPlanMigration cancels the batch's still-scheduled subscriptions.
func (c *Client) CancelPlanMigration(ctx context.Context, batchID uuid.UUID, requestOptions ...RequestOption) (*billing.PlanMigrationCancelResult, error) {
	batch, err := requireUUID("batch_id", batchID)
	if err != nil {
		return nil, err
	}
	var out billing.PlanMigrationCancelResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/plan-migrations/"+batch+"/cancel", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
