package openrails

import (
	"context"
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// MerchantConfigurationClient applies non-secret merchant metadata. Payment
// provider credentials have their own operation identity and publication contract.
type MerchantConfigurationClient struct{ client *Client }

func (c *MerchantConfigurationClient) Retrieve(ctx context.Context, options ...RequestOption) (*billing.MerchantConfigurationState, error) {
	var out billing.MerchantConfigurationState
	if err := c.client.do(ctx, http.MethodGet, "/v1/merchant/configuration", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *MerchantConfigurationClient) Apply(ctx context.Context, params *billing.MerchantConfigurationApplyParams, options ...RequestOption) (*billing.MerchantConfigurationReceipt, error) {
	if params == nil {
		return nil, fmt.Errorf("merchant configuration application is required")
	}
	var out billing.MerchantConfigurationReceipt
	if err := c.client.do(ctx, http.MethodPost, "/v1/merchant/configuration/applications", params, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
