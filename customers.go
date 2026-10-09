package openrails

import (
	"context"
	"net/http"
	"net/url"

	"github.com/open-rails/openrails/billing"
)

// ListCustomers lists the merchant's customers, newest first.
func (c *Client) ListCustomers(ctx context.Context, params billing.CustomerListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Customer], error) {
	q := url.Values{}
	if params.Search != "" {
		q.Set("search", params.Search)
	}
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.Customer]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/admin/customers", pageValues(q, params.PageRequest)), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCustomer reads one customer: its contact, its settings, and per currency
// its balance and the card that pays its invoices.
func (c *Client) GetCustomer(ctx context.Context, id billing.CustomerID, requestOptions ...RequestOption) (*billing.Customer, error) {
	path, err := customerIDPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.Customer
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateCustomer changes one customer's settings, only the fields params
// names, and answers the customer; a customer not billed yet is created. A
// person needs a recent sign-in for it.
func (c *Client) UpdateCustomer(ctx context.Context, id billing.CustomerID, params billing.UpdateCustomerParams, requestOptions ...RequestOption) (*billing.Customer, error) {
	path, err := customerIDPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.Customer
	if err := c.do(ctx, http.MethodPatch, path, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
