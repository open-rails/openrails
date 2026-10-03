package openrails

import (
	"context"
	"net/http"
	"strconv"

	"github.com/open-rails/openrails/billing"
)

// ProductClient is the product resource on an embedded or remote Client.
type ProductClient struct{ client *Client }

// PriceClient is the immutable price resource on an embedded or remote Client.
type PriceClient struct{ client *Client }

func (c *Client) initResources() {
	c.MerchantConfiguration = &MerchantConfigurationClient{client: c}
	c.PaymentProviders = &PaymentProviderClient{client: c}
	c.Catalog = &CatalogClient{client: c}
	c.ProductAccess = &ProductAccessClient{client: c}
	c.Products = &ProductClient{client: c}
	c.Prices = &PriceClient{client: c}
}

func (p *ProductClient) Create(ctx context.Context, params *billing.ProductCreateParams, requestOptions ...RequestOption) (*billing.Product, error) {
	var out billing.Product
	if err := p.client.do(ctx, http.MethodPost, p.client.catalogPath()+"/products", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *ProductClient) Retrieve(ctx context.Context, id string, requestOptions ...RequestOption) (*billing.Product, error) {
	id, err := resourceProductID(id)
	if err != nil {
		return nil, err
	}
	var out billing.Product
	if err = p.client.do(ctx, http.MethodGet, p.client.catalogPath()+"/products/"+id, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *ProductClient) RetrieveByKey(ctx context.Context, key string, requestOptions ...RequestOption) (*billing.Product, error) {
	key, err := pathID("key", key)
	if err != nil {
		return nil, err
	}
	var out billing.Product
	if err = p.client.do(ctx, http.MethodGet, p.client.catalogPath()+"/products/by-key/"+key, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *ProductClient) Update(ctx context.Context, id string, params *billing.ProductUpdateParams, requestOptions ...RequestOption) (*billing.Product, error) {
	id, err := resourceProductID(id)
	if err != nil {
		return nil, err
	}
	var out billing.Product
	if err = p.client.do(ctx, http.MethodPatch, p.client.catalogPath()+"/products/"+id, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *ProductClient) List(ctx context.Context, params *billing.ProductListParams, requestOptions ...RequestOption) (*billing.CatalogPage[billing.Product], error) {
	if params == nil {
		params = &billing.ProductListParams{}
	}
	q := pageQuery(params.PageOptions)
	q.Set("catalog_id", params.CatalogID)
	q.Set("tier_group", params.TierGroup)
	if params.Archived != nil {
		q.Set("archived", strconv.FormatBool(*params.Archived))
	}
	var out billing.CatalogPage[billing.Product]
	if err := p.client.do(ctx, http.MethodGet, p.client.catalogPath()+"/products?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *PriceClient) Create(ctx context.Context, params *billing.PriceCreateParams, requestOptions ...RequestOption) (*billing.Price, error) {
	if params == nil {
		return nil, invalidErr("params are required")
	}
	selectors := 0
	if params.ProductID != "" {
		selectors++
		if _, err := resourceProductID(params.ProductID); err != nil {
			return nil, err
		}
	}
	if params.ProductKey != "" {
		selectors++
		if !validProductKey(params.ProductKey) {
			return nil, invalidErr("product_key is invalid")
		}
	}
	if params.ProductData != nil {
		selectors++
	}
	if selectors != 1 {
		return nil, invalidErr("exactly one of product_id, product_key and product_data is required")
	}
	var out billing.Price
	if err := p.client.do(ctx, http.MethodPost, p.client.catalogPath()+"/prices", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *PriceClient) Retrieve(ctx context.Context, id string, requestOptions ...RequestOption) (*billing.Price, error) {
	id, err := resourcePriceID(id)
	if err != nil {
		return nil, err
	}
	var out billing.Price
	if err = p.client.do(ctx, http.MethodGet, p.client.catalogPath()+"/prices/"+id, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *PriceClient) RetrieveByKey(ctx context.Context, key string, requestOptions ...RequestOption) (*billing.Price, error) {
	key, err := pathID("key", key)
	if err != nil {
		return nil, err
	}
	var out billing.Price
	if err = p.client.do(ctx, http.MethodGet, p.client.catalogPath()+"/prices/by-key/"+key, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
func (p *PriceClient) Update(ctx context.Context, id string, params *billing.PriceUpdateParams, requestOptions ...RequestOption) (*billing.Price, error) {
	id, err := resourcePriceID(id)
	if err != nil {
		return nil, err
	}
	var out billing.Price
	if err = p.client.do(ctx, http.MethodPatch, p.client.catalogPath()+"/prices/"+id, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *PriceClient) List(ctx context.Context, params *billing.PriceListParams, requestOptions ...RequestOption) (*billing.CatalogPage[billing.Price], error) {
	if params == nil {
		params = &billing.PriceListParams{}
	}
	q := pageQuery(params.PageOptions)
	q.Set("catalog_id", params.CatalogID)
	q.Set("product_id", params.ProductID)
	q.Set("currency", normalizeCurrency(params.Currency))
	q.Set("type", params.Type)
	if params.Archived != nil {
		q.Set("archived", strconv.FormatBool(*params.Archived))
	}
	var out billing.CatalogPage[billing.Price]
	if err := p.client.do(ctx, http.MethodGet, p.client.catalogPath()+"/prices?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
func resourceProductID(id string) (string, error) {
	parsed, err := billing.ParseProductID(id)
	if err != nil {
		return "", invalidErr("invalid product_id")
	}
	return requireTypedID("product_id", parsed)
}
func resourcePriceID(id string) (string, error) {
	parsed, err := billing.ParsePriceID(id)
	if err != nil {
		return "", invalidErr("invalid price_id")
	}
	return requireTypedID("price_id", parsed)
}

// SetKey moves a price onto a merchant-unique lookup key.
func (p *PriceClient) SetKey(ctx context.Context, id, key string, requestOptions ...RequestOption) (*billing.Price, error) {
	id, err := resourcePriceID(id)
	if err != nil {
		return nil, err
	}
	key, err = requireID("key", key)
	if err != nil {
		return nil, err
	}
	var out billing.Price
	if err = p.client.do(ctx, http.MethodPost, p.client.catalogPath()+"/prices/"+id+"/key", struct {
		Key string `json:"key"`
	}{key}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// Ensure creates params only when its key is absent. Existing definitions
// are preserved. The server owns concurrency and catalog scope.
func (p *ProductClient) Ensure(ctx context.Context, params *billing.ProductCreateParams, requestOptions ...RequestOption) (*billing.Product, error) {
	if params == nil {
		return nil, invalidErr("product parameters are required")
	}
	key, err := pathID("key", params.Key)
	if err != nil {
		return nil, err
	}
	var out billing.Product
	if err = p.client.do(ctx, http.MethodPut, p.client.catalogPath()+"/products/by-key/"+key, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
