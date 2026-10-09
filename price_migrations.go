package openrails

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// CreatePriceMigration moves the subscribers of one price, or of every
// version of a price key, to another price at each one's renewal.
func (c *Client) CreatePriceMigration(ctx context.Context, params billing.CreatePriceMigrationParams, requestOptions ...RequestOption) (*billing.PriceMigration, error) {
	if err := requirePriceMigrationSource(params); err != nil {
		return nil, err
	}
	if params.ToPriceID.IsZero() {
		return nil, invalidErr("to_price_id is required")
	}
	var out billing.PriceMigration
	if err := c.do(ctx, http.MethodPost, "/v1/admin/price-migrations", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// PreviewPriceMigration answers what CreatePriceMigration would do, writing
// nothing.
func (c *Client) PreviewPriceMigration(ctx context.Context, params billing.CreatePriceMigrationParams, requestOptions ...RequestOption) (*billing.PriceMigrationPreview, error) {
	if err := requirePriceMigrationSource(params); err != nil {
		return nil, err
	}
	var out billing.PriceMigrationPreview
	if err := c.do(ctx, http.MethodPost, "/v1/admin/price-migrations/preview", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func requirePriceMigrationSource(p billing.CreatePriceMigrationParams) error {
	byKey := strings.TrimSpace(p.ProductKey) != "" || strings.TrimSpace(p.PriceKey) != ""
	if byKey == !p.FromPriceID.IsZero() || (byKey && (strings.TrimSpace(p.ProductKey) == "" || strings.TrimSpace(p.PriceKey) == "")) {
		return invalidErr("name from_price_id, or product_key and price_key")
	}
	return nil
}

// ListPriceMigrations lists the merchant's price migrations, newest first.
func (c *Client) ListPriceMigrations(ctx context.Context, params billing.PriceMigrationListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.PriceMigration], error) {
	q := pageValues(nil, params.PageRequest)
	if key := strings.TrimSpace(params.PriceKey); key != "" {
		q.Set("price_key", key)
		q.Set("product_key", params.ProductKey)
	}
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.PriceMigration]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/price-migrations?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPriceMigration reads one price migration with its moves counted.
func (c *Client) GetPriceMigration(ctx context.Context, id billing.PriceMigrationID, requestOptions ...RequestOption) (*billing.PriceMigration, error) {
	migration, err := requireTypedID("price_migration_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.PriceMigration
	if err := c.do(ctx, http.MethodGet, "/v1/admin/price-migrations/"+migration, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelPriceMigration cancels the migration's still-scheduled moves.
func (c *Client) CancelPriceMigration(ctx context.Context, id billing.PriceMigrationID, requestOptions ...RequestOption) (*billing.PriceMigrationCancel, error) {
	migration, err := requireTypedID("price_migration_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.PriceMigrationCancel
	if err := c.do(ctx, http.MethodPost, "/v1/admin/price-migrations/"+migration+"/cancel", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
