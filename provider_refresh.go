package openrails

import (
	"context"
	"net/http"
)

// ProviderRefresh reports a requested provider refresh for the bound
// merchant. Status is "queued" or "already_running" (an in-flight refresh
// absorbs the request and is started now).
type ProviderRefresh struct {
	Status string `json:"status"`
	JobID  int64  `json:"job_id,string"`
}

// RefreshProviders runs the merchant's provider refresh now: the scheduled
// pull that mirrors provider-side renewals, declines, cancellations and vault
// changes. Embedded and remote hosts use it instead of the pull CLI.
func (c *Client) RefreshProviders(ctx context.Context, requestOptions ...RequestOption) (*ProviderRefresh, error) {
	var out ProviderRefresh
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/provider-refresh", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
