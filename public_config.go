package openrails

import (
	"context"

	"github.com/open-rails/openrails/billing"
)

// GetPublicConfig reads the deployment's capabilities and public configuration.
// It sends no credential and does not invoke the handle's token provider.
func (c *Client) GetPublicConfig(ctx context.Context, options ...RequestOption) (*billing.PublicConfig, error) {
	var out billing.PublicConfig
	if err := c.read(ctx, ReadPublicConfig, "/v1/config", &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
