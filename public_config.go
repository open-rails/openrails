package openrails

import (
	"context"

	"github.com/open-rails/openrails/billing"
)

// GetPublicConfig reads the deployment's capabilities and public configuration.
// It sends no credential and does not invoke the handle's token provider.
// Over HTTP it is what a browser reads from the same server, ready for a
// host's page response (billing-ui's BillingProvider config); in process,
// Engine.PublicConfig names the mount the browser reads.
func (c *Client) GetPublicConfig(ctx context.Context, options ...RequestOption) (*billing.PublicConfig, error) {
	var out billing.PublicConfig
	if err := c.read(ctx, ReadPublicConfig, "/v1/config", &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
