package openrails

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/open-rails/openrails/billing"
)

// CheckProductAccess reports, for each product named by exactly one of
// params.ProductIDs and params.ProductKeys, whether the customer has access to
// it now and the seats they hold. Keys of the result are the ids or keys the
// request named.
func (c *Client) CheckProductAccess(ctx context.Context, customerID billing.CustomerID, params billing.CheckProductAccessParams, requestOptions ...RequestOption) (*billing.ProductAccessCheck, error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	if (params.ProductIDs == nil) == (params.ProductKeys == nil) {
		return nil, invalidErr("exactly one of product_ids and product_keys is required")
	}
	if err := batchSize(len(params.ProductIDs)+len(params.ProductKeys), billing.MaxProductAccessChecks); err != nil {
		return nil, err
	}
	for _, key := range params.ProductKeys {
		if !validProductKey(key) {
			return nil, invalidErr("product_key is invalid")
		}
	}
	var out billing.ProductAccessCheck
	if err := c.do(ctx, http.MethodPost, path+"/product-access/check", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListProductAccess returns one page of the customer's product-access
// windows, newest first: bought, subscribed and granted. params.LiveOnly keeps
// those live now.
func (c *Client) ListProductAccess(ctx context.Context, customerID billing.CustomerID, params billing.ProductAccessListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.ProductAccessGrant], error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	query := pageValues(nil, params.PageRequest)
	if params.LiveOnly {
		query.Set("live", "true")
	}
	if err := setIDs(query, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.ProductAccessGrant]
	if err := c.do(ctx, http.MethodGet, path+"/product-access?"+query.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
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

func validProductKey(key string) bool {
	return strings.TrimSpace(key) != "" && utf8.ValidString(key) && !strings.ContainsRune(key, 0)
}
