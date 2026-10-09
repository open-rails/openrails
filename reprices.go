package openrails

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// CreateRepriceBatch schedules every active subscription on a prior version
// of a price key to move to the key's current price.
func (c *Client) CreateRepriceBatch(ctx context.Context, params billing.CreateRepriceBatchParams, requestOptions ...RequestOption) (*billing.RepriceBatchResult, error) {
	if strings.TrimSpace(params.PriceKey) == "" || strings.TrimSpace(params.ProductKey) == "" {
		return nil, invalidErr("product_key and price_key are required")
	}
	if params.EffectiveAt.IsZero() {
		return nil, invalidErr("effective_at is required")
	}
	var out billing.RepriceBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/admin/reprice-batches", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// PreviewRepriceBatch counts the subscribers a batch for a price key would
// move, without writing anything.
func (c *Client) PreviewRepriceBatch(ctx context.Context, params billing.PreviewRepriceBatchParams, requestOptions ...RequestOption) (*billing.RepriceBatchPreview, error) {
	if strings.TrimSpace(params.PriceKey) == "" || strings.TrimSpace(params.ProductKey) == "" {
		return nil, invalidErr("product_key and price_key are required")
	}
	var out billing.RepriceBatchPreview
	if err := c.do(ctx, http.MethodPost, "/v1/admin/reprice-batches/preview", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRepriceBatches lists reprice batches and plan migrations, newest first.
func (c *Client) ListRepriceBatches(ctx context.Context, params billing.RepriceBatchListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.RepriceBatch], error) {
	q := pageValues(nil, params.PageRequest)
	if key := strings.TrimSpace(params.PriceKey); key != "" {
		q.Set("price_key", key)
		q.Set("product_key", params.ProductKey)
	}
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.RepriceBatch]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/reprice-batches?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRepriceBatch reads a batch with its reprices counted by status.
func (c *Client) GetRepriceBatch(ctx context.Context, id billing.RepriceBatchID, requestOptions ...RequestOption) (*billing.RepriceBatch, error) {
	batch, err := requireTypedID("reprice_batch_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.RepriceBatch
	if err := c.do(ctx, http.MethodGet, "/v1/admin/reprice-batches/"+batch, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelRepriceBatch cancels the batch's still-scheduled reprices.
func (c *Client) CancelRepriceBatch(ctx context.Context, id billing.RepriceBatchID, requestOptions ...RequestOption) (*billing.RepriceBatchCancel, error) {
	batch, err := requireTypedID("reprice_batch_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.RepriceBatchCancel
	if err := c.do(ctx, http.MethodPost, "/v1/admin/reprice-batches/"+batch+"/cancel", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// PreviewPlanMigration classifies the affected subscriptions without writing.
func (c *Client) PreviewPlanMigration(ctx context.Context, request billing.CreatePlanMigrationParams, requestOptions ...RequestOption) (*billing.PlanMigrationResult, error) {
	if err := requirePlanMigrationPrices(request); err != nil {
		return nil, err
	}
	var out billing.PlanMigrationResult
	if err := c.do(ctx, http.MethodPost, "/v1/admin/plan-migrations/preview", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreatePlanMigration schedules the migration and records its batch.
func (c *Client) CreatePlanMigration(ctx context.Context, request billing.CreatePlanMigrationParams, requestOptions ...RequestOption) (*billing.PlanMigrationResult, error) {
	if err := requirePlanMigrationPrices(request); err != nil {
		return nil, err
	}
	var out billing.PlanMigrationResult
	if err := c.do(ctx, http.MethodPost, "/v1/admin/plan-migrations", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// requirePlanMigrationPrices is the server's first check, applied before any I/O.
func requirePlanMigrationPrices(r billing.CreatePlanMigrationParams) error {
	if strings.TrimSpace(r.SourcePrice) == "" || strings.TrimSpace(r.TargetPrice) == "" {
		return invalidErr("source_price and target_price required")
	}
	return nil
}

// ListReprices lists scheduled, applied, canceled and blocked reprices,
// newest first.
func (c *Client) ListReprices(ctx context.Context, params billing.RepriceListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Reprice], error) {
	q := pageValues(nil, params.PageRequest)
	if !params.SubscriptionID.IsZero() {
		q.Set("subscription_id", params.SubscriptionID.String())
	}
	if !params.RepriceBatchID.IsZero() {
		q.Set("reprice_batch_id", params.RepriceBatchID.String())
	}
	if params.Status != "" {
		q.Set("status", string(params.Status))
	}
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.Reprice]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/reprices?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetReprice reads one reprice.
func (c *Client) GetReprice(ctx context.Context, id billing.RepriceID, requestOptions ...RequestOption) (*billing.Reprice, error) {
	reprice, err := requireTypedID("reprice_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Reprice
	if err := c.do(ctx, http.MethodGet, "/v1/admin/reprices/"+reprice, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelReprice cancels a scheduled reprice.
func (c *Client) CancelReprice(ctx context.Context, id billing.RepriceID, requestOptions ...RequestOption) (*billing.Reprice, error) {
	reprice, err := requireTypedID("reprice_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Reprice
	if err := c.do(ctx, http.MethodPost, "/v1/admin/reprices/"+reprice+"/cancel", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
