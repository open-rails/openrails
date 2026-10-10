package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// ListProductAccess returns one page of product-access windows, newest
// first: bought, subscribed and granted, of params.CustomerIDs and
// params.ProductIDs (each 1 to billing.MaxBatchItems; none: any).
// params.LiveOnly keeps those live now. params.IDs instead reads named
// windows.
func (c *Client) ListProductAccess(ctx context.Context, params billing.ProductAccessListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.ProductAccessGrant], error) {
	query := pageValues(nil, params.PageRequest)
	if err := setIDs(query, params.IDs); err != nil {
		return nil, err
	}
	if err := setIDList(query, "customer_id", params.CustomerIDs); err != nil {
		return nil, err
	}
	if err := setIDList(query, "product_id", params.ProductIDs); err != nil {
		return nil, err
	}
	if params.LiveOnly {
		query.Set("live", "true")
	}
	var out billing.ListPage[billing.ProductAccessGrant]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/product-access?"+query.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// setIDList sets a comma-separated id filter of 1 to billing.MaxBatchItems
// ids; nil sets none.
func setIDList[T wireID](q url.Values, name string, ids []T) error {
	if ids == nil {
		return nil
	}
	if err := batchIDs(name, ids, billing.MaxBatchItems); err != nil {
		return err
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.String()
	}
	q.Set(name, strings.Join(parts, ","))
	return nil
}

// CreateProductAccess grants 1 to billing.MaxBatchItems products free, across
// any customers, all or none; the answer is in request order. Each grant is a
// window of its product: the customer holds the product's current keys while
// it is live. With params.IdempotencyKey, a retry replays the first answer.
func (c *Client) CreateProductAccess(ctx context.Context, params billing.CreateProductAccessBatchParams, requestOptions ...RequestOption) ([]billing.ProductAccessGrant, error) {
	if err := batchSize(len(params.Items), billing.MaxBatchItems); err != nil {
		return nil, err
	}
	for _, item := range params.Items {
		if item.CustomerID.IsZero() {
			return nil, invalidErr("customer_id is required")
		}
		if item.ProductID.IsZero() {
			return nil, invalidErr("product_id is required")
		}
	}
	var headers http.Header
	if key := strings.TrimSpace(params.IdempotencyKey); key != "" {
		headers = http.Header{"Idempotency-Key": {key}}
	}
	var out billing.CreateProductAccessBatchResult
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/admin/product-access", params, &out, headers, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// DeleteProductAccess revokes one of the customer's product-access grants.
func (c *Client) DeleteProductAccess(ctx context.Context, customerID billing.CustomerID, id billing.ProductAccessID, requestOptions ...RequestOption) error {
	path, err := customerIDPath(customerID)
	if err != nil {
		return err
	}
	grant, err := requireTypedID("product_access_id", id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path+"/product-access/"+grant, nil, nil, requestOptions...)
}
