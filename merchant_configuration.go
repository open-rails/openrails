package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// GetMerchantConfiguration reads the merchant's non-secret configuration
// and its revision.
func (c *Client) GetMerchantConfiguration(ctx context.Context, options ...RequestOption) (*billing.MerchantConfigurationState, error) {
	var out billing.MerchantConfigurationState
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/configuration", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ApplyMerchantConfiguration commits one configuration change against the
// revision it read: omitted fields keep their stored values, and the same
// application replays its original receipt.
func (c *Client) ApplyMerchantConfiguration(ctx context.Context, params billing.ApplyMerchantConfigurationParams, options ...RequestOption) (*billing.MerchantConfigurationReceipt, error) {
	var out billing.MerchantConfigurationReceipt
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/configuration/applications", params, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAPIHost returns the merchant's proven API host and its open claim.
func (c *Client) GetAPIHost(ctx context.Context, options ...RequestOption) (*billing.MerchantAPIHost, error) {
	var out billing.MerchantAPIHost
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/api-host", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetAPIHost claims a host for the merchant's public routes: it routes nothing
// until VerifyAPIHost proves the claim's DNS record. "" releases the host.
func (c *Client) SetAPIHost(ctx context.Context, req billing.SetAPIHostParams, options ...RequestOption) (*billing.MerchantAPIHost, error) {
	var out billing.MerchantAPIHost
	if err := c.do(ctx, http.MethodPut, "/v1/merchant/api-host", req, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// VerifyAPIHost proves the open claim through DNS and binds its host.
func (c *Client) VerifyAPIHost(ctx context.Context, options ...RequestOption) (*billing.MerchantAPIHost, error) {
	var out billing.MerchantAPIHost
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/api-host/verify", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
