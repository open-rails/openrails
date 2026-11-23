package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// SyncLeases syncs one gate's lease: it settles the lease's usage in order,
// answers each want with a hold or a refusal, and takes back the holds
// returned. The first sync under a new params.LeaseID creates the lease.
func (c *Client) SyncLeases(ctx context.Context, params billing.SyncLeasesParams, requestOptions ...RequestOption) (*billing.SyncLeasesResult, error) {
	switch {
	case params.LeaseID.IsZero():
		return nil, invalidErr("lease_id is required")
	case len(params.Usage) > billing.MaxSyncUsageItems:
		return nil, invalidErr("a sync carries at most 1000 usage items")
	case len(params.Wants) > billing.MaxSyncWants:
		return nil, invalidErr("a sync carries at most 1000 wants")
	case len(params.Returns) > billing.MaxSyncReturns:
		return nil, invalidErr("a sync carries at most 1000 returns")
	}
	var out billing.SyncLeasesResult
	if err := c.do(ctx, http.MethodPost, "/v1/app/leases/sync", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
