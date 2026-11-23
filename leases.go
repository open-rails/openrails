package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// SyncLeases is the protocol under Gate; hosts call NewGate rather than this.
// A lease is one gate's holds of customers' capacity, one per customer and
// limit set, sized from the gate's traffic. Each sync settles the gate's
// captured usage in order (an item is numbered within its lease, and a resend
// is matched by lease and number), grows or refuses holds against settled plus
// held capacity (a request that fits only once other gates' holds shrink is
// capacity_held), and releases a smaller size only after the gate has applied
// the version that carried it. A lease not synced within seconds dies and
// its unused capacity is released; its usage still settles, as late usage.
// Leases answer resends for 61 days after their last sync. Settled usage
// posts to the customer's balance as one deduction per UTC day.
//
// SyncLeases syncs one gate's lease: it settles the lease's usage in order,
// answers each want with a hold or a refusal, and takes back the holds
// returned. The first sync under a new params.LeaseID creates the lease.
func (c *Client) SyncLeases(ctx context.Context, params billing.SyncLeasesParams, requestOptions ...RequestOption) (*billing.SyncLeasesResult, error) {
	switch {
	case params.LeaseID.IsZero():
		return nil, invalidErr("lease_id is required")
	case params.LeaseID.UUID().Version() != 7:
		return nil, invalidErr("lease_id must be a UUIDv7")
	case len(params.Usage) > billing.MaxSyncUsageItems:
		return nil, invalidErr("a sync carries at most 1000 usage items")
	case len(params.Wants) > billing.MaxSyncWants:
		return nil, invalidErr("a sync carries at most 1000 wants")
	case len(params.Returns) > billing.MaxSyncReturns:
		return nil, invalidErr("a sync carries at most 1000 returns")
	case len(params.Committed) > billing.MaxSyncCommitted:
		return nil, invalidErr("a sync carries at most 10000 commitments")
	case len(params.Applied) > billing.MaxSyncApplied:
		return nil, invalidErr("a sync carries at most 10000 applied versions")
	}
	var out billing.SyncLeasesResult
	if err := c.do(ctx, http.MethodPost, "/v1/app/leases/sync", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
