package openrails

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/open-rails/openrails/billing"
)

func setBool(q url.Values, name string, value *bool) {
	if value != nil {
		q.Set(name, strconv.FormatBool(*value))
	}
}

func setID(q url.Values, name string, id wireID) {
	if !id.IsZero() {
		q.Set(name, id.String())
	}
}

// CreateProduct creates a product.
func (c *Client) CreateProduct(ctx context.Context, params billing.CreateProductParams, requestOptions ...RequestOption) (*billing.Product, error) {
	var out billing.Product
	if err := c.do(ctx, http.MethodPost, "/v1/admin/catalog/products", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// EnsureProduct creates the product under params.Key unless one exists; an
// existing product is returned unchanged.
func (c *Client) EnsureProduct(ctx context.Context, params billing.CreateProductParams, requestOptions ...RequestOption) (*billing.Product, error) {
	key, err := pathID("key", params.Key)
	if err != nil {
		return nil, err
	}
	var out billing.Product
	if err := c.do(ctx, http.MethodPut, "/v1/admin/catalog/products/by-key/"+key, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetProduct reads a product with its current prices.
func (c *Client) GetProduct(ctx context.Context, id billing.ProductID, requestOptions ...RequestOption) (*billing.Product, error) {
	path, err := requireTypedID("product_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Product
	if err := c.do(ctx, http.MethodGet, "/v1/admin/catalog/products/"+path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListProducts returns one page of products, newest first, each with its
// current prices. params.Keys reads products by key.
func (c *Client) ListProducts(ctx context.Context, params billing.ProductListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Product], error) {
	q := pageValues(nil, params.PageRequest)
	setBool(q, "archived", params.Archived)
	if params.TierGroup != "" {
		q.Set("tier_group", params.TierGroup)
	}
	if len(params.Keys) > billing.MaxBatchItems || len(params.Entitlements) > billing.MaxBatchItems {
		return nil, invalidErr(fmt.Sprintf("keys and entitlements each name at most %d values", billing.MaxBatchItems))
	}
	for _, key := range params.Keys {
		q.Add("keys", key)
	}
	for _, key := range params.Entitlements {
		q.Add("entitlement", key)
	}
	setBool(q, "for_sale", params.ForSale)
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.Product]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/catalog/products?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateProduct changes a product's fields; omitted ones keep their values.
func (c *Client) UpdateProduct(ctx context.Context, id billing.ProductID, params billing.UpdateProductParams, requestOptions ...RequestOption) (*billing.Product, error) {
	path, err := requireTypedID("product_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Product
	if err := c.do(ctx, http.MethodPatch, "/v1/admin/catalog/products/"+path, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreatePrice creates a price on the product named by exactly one of
// ProductID, ProductKey and ProductData. A price's terms never change;
// selling on other terms means another price.
func (c *Client) CreatePrice(ctx context.Context, params billing.CreatePriceParams, requestOptions ...RequestOption) (*billing.Price, error) {
	selectors := 0
	for _, set := range []bool{!params.ProductID.IsZero(), params.ProductKey != "", params.ProductData != nil} {
		if set {
			selectors++
		}
	}
	if selectors != 1 {
		return nil, invalidErr("exactly one of product_id, product_key and product_data is required")
	}
	var out billing.Price
	if err := c.do(ctx, http.MethodPost, "/v1/admin/catalog/prices", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPrice reads a price; params.Verify also reads each linked PSP's copy and
// reports its drift.
func (c *Client) GetPrice(ctx context.Context, id billing.PriceID, params billing.GetPriceParams, requestOptions ...RequestOption) (*billing.Price, error) {
	path, err := requireTypedID("price_id", id)
	if err != nil {
		return nil, err
	}
	if params.Verify {
		path += "?verify=true"
	}
	var out billing.Price
	if err := c.do(ctx, http.MethodGet, "/v1/admin/catalog/prices/"+path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPrices returns one page of prices, newest first. params.ProductKey and
// params.Key read prices by key: a key's current price is its one not
// archived.
func (c *Client) ListPrices(ctx context.Context, params billing.PriceListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Price], error) {
	q := pageValues(nil, params.PageRequest)
	setID(q, "product_id", params.ProductID)
	if params.ProductKey != "" {
		q.Set("product_key", params.ProductKey)
	}
	if params.Key != "" {
		q.Set("key", params.Key)
	}
	if params.Currency != "" {
		q.Set("currency", normalizeCurrency(params.Currency))
	}
	setBool(q, "recurring", params.Recurring)
	setBool(q, "archived", params.Archived)
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.Price]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/catalog/prices?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPriceHistory returns one page of the history of a price's key, most
// recent first: when the key moved to which price, or was retired.
func (c *Client) ListPriceHistory(ctx context.Context, id billing.PriceID, page billing.PageRequest, requestOptions ...RequestOption) (*billing.ListPage[billing.PriceKeyMovement], error) {
	path, err := requireTypedID("price_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.PriceKeyMovement]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/catalog/prices/"+path+"/history?"+pageValues(nil, page).Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdatePrice archives or restores a price, or changes its PSP links.
// Omitted fields keep their values; the key and financial terms are immutable.
func (c *Client) UpdatePrice(ctx context.Context, id billing.PriceID, params billing.UpdatePriceParams, requestOptions ...RequestOption) (*billing.Price, error) {
	path, err := requireTypedID("price_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Price
	if err := c.do(ctx, http.MethodPatch, "/v1/admin/catalog/prices/"+path, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
