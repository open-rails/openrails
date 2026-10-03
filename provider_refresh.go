package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// RefreshProviders runs the merchant's provider refresh now: the scheduled
// pull that mirrors provider-side renewals, declines, cancellations and vault
// changes. Embedded and remote hosts use it instead of the pull CLI.
func (c *Client) RefreshProviders(ctx context.Context, requestOptions ...RequestOption) (*billing.ProviderRefresh, error) {
	var out billing.ProviderRefresh
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/provider-refresh", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
