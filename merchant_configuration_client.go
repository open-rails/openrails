package openrails

import (
	"context"
	"fmt"
	"net/http"
)

// MerchantConfigurationClient applies non-secret merchant metadata. Payment
// provider credentials have their own operation identity and publication contract.
type MerchantConfigurationClient struct{ client *Client }

func (c *MerchantConfigurationClient) Retrieve(ctx context.Context, options ...RequestOption) (*MerchantConfigurationState, error) {
	var out MerchantConfigurationState
	if err := c.client.do(ctx, http.MethodGet, "/v1/merchant/configuration", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *MerchantConfigurationClient) Apply(ctx context.Context, params *MerchantConfigurationApplyParams, options ...RequestOption) (*MerchantConfigurationReceipt, error) {
	if params == nil {
		return nil, fmt.Errorf("merchant configuration application is required")
	}
	var out MerchantConfigurationReceipt
	if err := c.client.do(ctx, http.MethodPost, "/v1/merchant/configuration/applications", params, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
