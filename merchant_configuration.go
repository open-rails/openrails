package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// GetMerchantConfiguration reads the merchant's configuration, never its
// credentials, and its revision.
func (c *Client) GetMerchantConfiguration(ctx context.Context, options ...RequestOption) (*billing.MerchantConfigurationState, error) {
	var out billing.MerchantConfigurationState
	if err := c.do(ctx, http.MethodGet, "/v1/admin/configuration", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateMerchantConfiguration merges params into the merchant's configuration
// and returns it. It needs merchant configuration held in Vault: a
// configuration read from a file is read-only.
func (c *Client) UpdateMerchantConfiguration(ctx context.Context, params billing.UpdateMerchantConfigurationParams, options ...RequestOption) (*billing.MerchantConfigurationState, error) {
	var out billing.MerchantConfigurationState
	if err := c.do(ctx, http.MethodPatch, "/v1/admin/configuration", params, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAPIHost returns the merchant's proven API host and its open claim.
func (c *Client) GetAPIHost(ctx context.Context, options ...RequestOption) (*billing.MerchantAPIHost, error) {
	var out billing.MerchantAPIHost
	if err := c.do(ctx, http.MethodGet, "/v1/admin/api-host", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
