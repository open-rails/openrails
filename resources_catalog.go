package openrails

import (
	"context"
	"net/http"
	"strconv"
)

// ProductClient is the product resource on an embedded or remote Client.
type ProductClient struct{ client *Client }

// PriceClient is the immutable price resource on an embedded or remote Client.
type PriceClient struct{ client *Client }

func (c *Client) initResources() {
	c.ProductAccess = &ProductAccessClient{client: c}
	c.Products = &ProductClient{client: c}
	c.Prices = &PriceClient{client: c}
}

func (p *ProductClient) Create(ctx context.Context, params *ProductCreateParams) (*Product, error) {
	var out Product
	if err := p.client.do(ctx, http.MethodPost, p.client.catalogPath()+"/products", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *ProductClient) Retrieve(ctx context.Context, id string) (*Product, error) {
	id, err := resourceProductID(id)
	if err != nil {
		return nil, err
	}
	var out Product
	if err = p.client.do(ctx, http.MethodGet, p.client.catalogPath()+"/products/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *ProductClient) RetrieveByKey(ctx context.Context, key string) (*Product, error) {
	key, err := pathID("key", key)
	if err != nil {
		return nil, err
	}
	var out Product
	if err = p.client.do(ctx, http.MethodGet, p.client.catalogPath()+"/products/by-key/"+key, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *ProductClient) Update(ctx context.Context, id string, params *ProductUpdateParams) (*Product, error) {
	id, err := resourceProductID(id)
	if err != nil {
		return nil, err
	}
	var out Product
	if err = p.client.do(ctx, http.MethodPatch, p.client.catalogPath()+"/products/"+id, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type ProductListParams struct {
	PageOptions
	CatalogID string
	Archived  *bool
	TierGroup string
}

func (p *ProductClient) List(ctx context.Context, params *ProductListParams) (*CatalogPage[Product], error) {
	if params == nil {
		params = &ProductListParams{}
	}
	q := pageQuery(params.PageOptions)
	q.Set("catalog_id", params.CatalogID)
	q.Set("tier_group", params.TierGroup)
	if params.Archived != nil {
		q.Set("archived", strconv.FormatBool(*params.Archived))
	}
	var out CatalogPage[Product]
	if err := p.client.do(ctx, http.MethodGet, p.client.catalogPath()+"/products?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *PriceClient) Create(ctx context.Context, params *PriceCreateParams) (*Price, error) {
	var out Price
	if err := p.client.do(ctx, http.MethodPost, p.client.catalogPath()+"/prices", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *PriceClient) Retrieve(ctx context.Context, id string) (*Price, error) {
	id, err := resourcePriceID(id)
	if err != nil {
		return nil, err
	}
	var out Price
	if err = p.client.do(ctx, http.MethodGet, p.client.catalogPath()+"/prices/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *PriceClient) RetrieveByKey(ctx context.Context, key string) (*Price, error) {
	key, err := pathID("key", key)
	if err != nil {
		return nil, err
	}
	var out Price
	if err = p.client.do(ctx, http.MethodGet, p.client.catalogPath()+"/prices/by-key/"+key, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *PriceClient) Update(ctx context.Context, id string, params *PriceUpdateParams) (*Price, error) {
	id, err := resourcePriceID(id)
	if err != nil {
		return nil, err
	}
	var out Price
	if err = p.client.do(ctx, http.MethodPatch, p.client.catalogPath()+"/prices/"+id, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type PriceListParams struct {
	PageOptions
	CatalogID string
	ProductID string
	Archived  *bool
	Currency  string
	Type      string
}

func (p *PriceClient) List(ctx context.Context, params *PriceListParams) (*CatalogPage[Price], error) {
	if params == nil {
		params = &PriceListParams{}
	}
	q := pageQuery(params.PageOptions)
	q.Set("catalog_id", params.CatalogID)
	q.Set("product_id", params.ProductID)
	q.Set("currency", normalizeCurrency(params.Currency))
	q.Set("type", params.Type)
	if params.Archived != nil {
		q.Set("archived", strconv.FormatBool(*params.Archived))
	}
	var out CatalogPage[Price]
	if err := p.client.do(ctx, http.MethodGet, p.client.catalogPath()+"/prices?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func resourceProductID(id string) (string, error) {
	parsed, err := ParseProductID(id)
	if err != nil {
		return "", invalidErr("invalid product_id")
	}
	return requireTypedID("product_id", parsed)
}
func resourcePriceID(id string) (string, error) {
	parsed, err := ParsePriceID(id)
	if err != nil {
		return "", invalidErr("invalid price_id")
	}
	return requireTypedID("price_id", parsed)
}

// SetKey moves a price onto a merchant-unique lookup key.
func (p *PriceClient) SetKey(ctx context.Context, id, key string) (*Price, error) {
	id, err := resourcePriceID(id)
	if err != nil {
		return nil, err
	}
	key, err = requireID("key", key)
	if err != nil {
		return nil, err
	}
	var out Price
	if err = p.client.do(ctx, http.MethodPost, p.client.catalogPath()+"/prices/"+id+"/key", struct {
		Key string `json:"key"`
	}{key}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Ensure returns the product for key, creating it with name only if absent.
// Existing labels are preserved. The server owns concurrency and catalog scope.
func (p *ProductClient) Ensure(ctx context.Context, key, name string) (*Product, error) {
	key, err := pathID("key", key)
	if err != nil {
		return nil, err
	}
	var out Product
	if err = p.client.do(ctx, http.MethodPut, p.client.catalogPath()+"/products/by-key/"+key, struct {
		DisplayName string `json:"display_name"`
	}{name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
