package openrails

import (
	"context"
	"net/http"
	"net/url"

	"github.com/open-rails/openrails/billing"
)

func limitPath(key string) (string, error) {
	key, err := pathID("key", key)
	if err != nil {
		return "", err
	}
	return "/v1/catalog/limits/" + key, nil
}

// ListLimits returns one page of the merchant's limits, by key.
func (c *Client) ListLimits(ctx context.Context, page billing.PageRequest, requestOptions ...RequestOption) (*billing.ListPage[billing.Limit], error) {
	var out billing.ListPage[billing.Limit]
	if err := c.read(ctx, ReadMeters, withQuery("/v1/catalog/limits", pageValues(nil, page)), &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetLimit reads one limit with the products that give it capacity.
func (c *Client) GetLimit(ctx context.Context, key string, requestOptions ...RequestOption) (*billing.Limit, error) {
	path, err := limitPath(key)
	if err != nil {
		return nil, err
	}
	var out billing.Limit
	if err := c.read(ctx, ReadMeters, path, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetLimit declares the limit under key whole, creating it or changing it.
// A limit with windows keeps its unit (billing.ErrConflict).
func (c *Client) SetLimit(ctx context.Context, key string, params billing.SetLimitParams, requestOptions ...RequestOption) (*billing.Limit, error) {
	path, err := limitPath(key)
	if err != nil {
		return nil, err
	}
	var out billing.Limit
	if err := c.do(ctx, http.MethodPut, path, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCustomerLimits is where the customer stands on every limit in its
// current window, in one page.
func (c *Client) ListCustomerLimits(ctx context.Context, customer billing.CustomerID, requestOptions ...RequestOption) (*billing.ListPage[billing.CustomerLimit], error) {
	if customer.IsZero() {
		return nil, invalidErr("customer_id is required")
	}
	var out billing.ListPage[billing.CustomerLimit]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/app/limits", url.Values{"customer_id": {customer.String()}}), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// AdjustCustomerLimit adds params.Amount to the capacity of the customer's
// current window of params.Limit only, and answers where it stands after.
// A retry adds again.
func (c *Client) AdjustCustomerLimit(ctx context.Context, customer billing.CustomerID, params billing.AdjustLimitParams, requestOptions ...RequestOption) (*billing.CustomerLimit, error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	var out billing.CustomerLimit
	if err := c.do(ctx, http.MethodPost, path+"/limit-adjustments", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
