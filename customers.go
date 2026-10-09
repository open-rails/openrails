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
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.Customer]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/admin/customers", pageValues(q, params.PageRequest)), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
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
	if err := c.do(ctx, http.MethodPost, "/v1/admin/customers/ensure", billing.EnsureCustomerBatchParams{Items: items}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ListCustomerSettings lists customers' settings, newest customer first.
// IDs instead reads 1 to billing.MaxBatchItems named customers in one page;
// one the merchant never declared or billed is absent.
func (c *Client) ListCustomerSettings(ctx context.Context, params billing.CustomerSettingsListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.CustomerSettings], error) {
	q := url.Values{}
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.CustomerSettings]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/admin/customers/settings", pageValues(q, params.PageRequest)), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateCustomerSettings changes 1 to billing.MaxBatchItems distinct
// customers' settings, all or none; the answer is in request order. Each
// item changes only the fields it names. A person needs a recent sign-in
// for it.
func (c *Client) UpdateCustomerSettings(ctx context.Context, items []billing.UpdateCustomerSettingsParams, requestOptions ...RequestOption) ([]billing.CustomerSettings, error) {
	if err := batchSize(len(items), billing.MaxBatchItems); err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.CustomerID.IsZero() {
			return nil, invalidErr("customer_id is required")
		}
	}
	var out billing.CustomerSettingsBatch
	if err := c.do(ctx, http.MethodPatch, "/v1/admin/customers/settings", billing.UpdateCustomerSettingsBatchParams{Items: items}, &out, requestOptions...); err != nil {
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
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/admin/delinquency", pageValues(q, params.PageRequest)), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
