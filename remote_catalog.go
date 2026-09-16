package openrails

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"
)

type CatalogPage[T any] struct {
	Items  []T   `json:"items"`
	Total  int64 `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

type ProductFilter struct {
	PageOptions
	ActiveOnly bool
	TierGroup  string
}

// PriceFilter selects prices. Archived nil lists every price; false lists
// live prices only; true lists archived prices only.
type PriceFilter struct {
	PageOptions
	ProductID *uuid.UUID
	Archived  *bool
	Currency  string
	Type      string
}

func (c *Client) CreateProduct(ctx context.Context, request CreateProductRequest) (*CatalogProduct, error) {
	var out CatalogProduct
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/catalog/products", request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) GetProduct(ctx context.Context, id uuid.UUID) (*CatalogProduct, error) {
	var out CatalogProduct
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/products/"+id.String(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) GetProductByKey(ctx context.Context, key string) (*CatalogProduct, error) {
	var out CatalogProduct
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/products/by-key/"+url.PathEscape(key), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) ListProducts(ctx context.Context, filter ProductFilter) (*CatalogPage[CatalogProduct], error) {
	q := pageQuery(filter.PageOptions)
	q.Set("active_only", strconv.FormatBool(filter.ActiveOnly))
	q.Set("tier_group", filter.TierGroup)
	var out CatalogPage[CatalogProduct]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/products?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) UpdateProduct(ctx context.Context, id uuid.UUID, request UpdateProductRequest) (*CatalogProduct, error) {
	var out CatalogProduct
	if err := c.do(ctx, http.MethodPatch, "/v1/merchant/catalog/products/"+id.String(), request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) CreatePrice(ctx context.Context, request CreatePriceRequest) (*CatalogPrice, error) {
	var out CatalogPrice
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/catalog/prices", request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) GetPrice(ctx context.Context, id uuid.UUID) (*CatalogPrice, error) {
	var out CatalogPrice
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/prices/"+id.String(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) GetPriceByKey(ctx context.Context, key string) (*CatalogPrice, error) {
	var out CatalogPrice
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/prices/by-key/"+url.PathEscape(key), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) ListPrices(ctx context.Context, filter PriceFilter) (*CatalogPage[CatalogPrice], error) {
	q := pageQuery(filter.PageOptions)
	if filter.Archived != nil {
		q.Set("archived", strconv.FormatBool(*filter.Archived))
	}
	q.Set("currency", filter.Currency)
	q.Set("type", filter.Type)
	if filter.ProductID != nil {
		q.Set("product_id", filter.ProductID.String())
	}
	var out CatalogPage[CatalogPrice]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/prices?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) UpdatePrice(ctx context.Context, id uuid.UUID, request UpdatePriceRequest) (*CatalogPrice, error) {
	var out CatalogPrice
	if err := c.do(ctx, http.MethodPatch, "/v1/merchant/catalog/prices/"+id.String(), request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetPriceKey moves a price onto key in place. A live price already holding
// key is archived first.
func (c *Client) SetPriceKey(ctx context.Context, id uuid.UUID, key string) (*CatalogPrice, error) {
	var out CatalogPrice
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/catalog/prices/"+id.String()+"/key", struct {
		Key string `json:"key"`
	}{Key: key}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EnsureUsageProduct preserves an existing catalog definition and handles
// concurrent bootstrap through the same catalog commands in every deployment.
func (c *Client) EnsureUsageProduct(ctx context.Context, key, name string) (uuid.UUID, error) {
	product, err := c.GetProductByKey(ctx, key)
	if err == nil {
		return product.ID, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return uuid.Nil, err
	}
	product, err = c.CreateProduct(ctx, CreateProductRequest{Key: key, DisplayName: name})
	if errors.Is(err, ErrConflict) {
		product, err = c.GetProductByKey(ctx, key)
	}
	if err != nil {
		return uuid.Nil, err
	}
	return product.ID, nil
}
