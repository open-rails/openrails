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
	if params.Query != "" {
		q.Set("q", params.Query)
	}
	var out billing.ListPage[billing.Customer]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/merchant/customers", pageValues(q, params.PageRequest)), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCustomer reads one customer; one the merchant never declared or billed
// is billing.ErrNotFound.
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

// EnsureCustomer creates the customer under the bound merchant, or replaces
// its declared fields. Commerce writes create customers on demand; call this
// to declare one first, or to set its billing email.
func (c *Client) EnsureCustomer(ctx context.Context, id billing.CustomerID, params billing.EnsureCustomerParams, requestOptions ...RequestOption) (*billing.Customer, error) {
	path, err := customerIDPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.Customer
	if err := c.do(ctx, http.MethodPut, path, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCustomerBillingProfile reads one customer's billing at a glance.
func (c *Client) GetCustomerBillingProfile(ctx context.Context, id billing.CustomerID, requestOptions ...RequestOption) (*billing.CustomerBillingProfile, error) {
	path, err := customerIDPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.CustomerBillingProfile
	if err := c.do(ctx, http.MethodGet, path+"/billing-profile", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCustomerBillingPolicy reads the policy assigned to a customer; a null
// PolicyName means the customer inherits its tier's or the default policy.
func (c *Client) GetCustomerBillingPolicy(ctx context.Context, id billing.CustomerID, requestOptions ...RequestOption) (*billing.CustomerBillingPolicy, error) {
	path, err := customerIDPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.CustomerBillingPolicy
	if err := c.do(ctx, http.MethodGet, path+"/billing-policy", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetCustomerBillingPolicy assigns a declared policy to a customer; a nil
// PolicyName restores inheritance. It never creates a customer.
func (c *Client) SetCustomerBillingPolicy(ctx context.Context, id billing.CustomerID, params billing.SetCustomerBillingPolicyParams, requestOptions ...RequestOption) (*billing.CustomerBillingPolicy, error) {
	path, err := customerIDPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.CustomerBillingPolicy
	if err := c.do(ctx, http.MethodPut, path+"/billing-policy", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCustomerDelinquency lists a customer's delinquency in every currency it
// has owed in; an empty page means it was never overdue.
func (c *Client) ListCustomerDelinquency(ctx context.Context, id billing.CustomerID, requestOptions ...RequestOption) (*billing.ListPage[billing.Delinquency], error) {
	path, err := customerIDPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.Delinquency]
	if err := c.do(ctx, http.MethodGet, path+"/delinquency", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListDelinquency lists the merchant's overdue customers, oldest debt first.
func (c *Client) ListDelinquency(ctx context.Context, params billing.DelinquencyListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Delinquency], error) {
	q := url.Values{}
	if params.State != "" {
		q.Set("state", string(params.State))
	}
	var out billing.ListPage[billing.Delinquency]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/merchant/delinquency", pageValues(q, params.PageRequest)), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
