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

// GetCustomers reads up to billing.MaxCustomerLookup customers. Every
// requested customer is in the answer; one the merchant never declared or
// billed is nil.
func (c *Client) GetCustomers(ctx context.Context, ids []billing.CustomerID, requestOptions ...RequestOption) (map[billing.CustomerID]*billing.Customer, error) {
	if err := batchIDs("customer_ids", ids, billing.MaxCustomerLookup); err != nil {
		return nil, err
	}
	var out billing.CustomerLookup
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/customers/lookup", billing.CustomerLookupParams{CustomerIDs: ids}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Customers, nil
}

// EnsureCustomers creates 1 to billing.MaxBatchItems distinct customers under
// the bound merchant, or replaces their declared fields, all or none; the
// answer is in request order. Commerce writes create customers on demand;
// call this to declare them first, or to set their billing emails.
func (c *Client) EnsureCustomers(ctx context.Context, items []billing.EnsureCustomerParams, requestOptions ...RequestOption) ([]billing.Customer, error) {
	if err := batchSize(len(items), billing.MaxBatchItems); err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.ID.IsZero() {
			return nil, invalidErr("customer id is required")
		}
	}
	var out billing.EnsureCustomerBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/customers/ensure", billing.EnsureCustomerBatchParams{Items: items}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
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
