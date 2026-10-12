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
	if err := c.read(ctx, ReadMerchantConfiguration, "/v1/merchant/configuration", &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateMerchantConfiguration merges params into the merchant's configuration
// and returns it. It needs merchant configuration held in Vault: a
// configuration read from a file is read-only.
func (c *Client) UpdateMerchantConfiguration(ctx context.Context, params billing.UpdateMerchantConfigurationParams, options ...RequestOption) (*billing.MerchantConfigurationState, error) {
	var out billing.MerchantConfigurationState
	if err := c.do(ctx, http.MethodPatch, "/v1/merchant/configuration", params, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
