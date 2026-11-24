package leases

import (
	"slices"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/modules/money"
)

// intake takes a chunk of the sync's usage into the event store: priced and
// located, inserted and waited for (what an earlier attempt inserted is
// skipped), then the PostgreSQL side in the sync's transaction. While the
// store is out of reach the sync fails whole and settles nothing.
func (r *syncRun) intake(items []money.UsageItem) ([]money.UsageResult, error) {
	in, err := r.money.PrepareUsageTx(r.ctx, r.q, r.DB.EventStore(), items)
	if err != nil {
		return nil, err
	}
	if err := r.money.CommitUsageTx(r.ctx, r.q, in); err != nil {
		return nil, err
	}
	for _, a := range in.Changed() {
		if !slices.Contains(r.changed, a) {
			r.changed = append(r.changed, a)
		}
	}
	return in.Results, nil
}

// identify gives a lease's usage item its intake identity.
func (r *syncRun) identify(item *money.UsageItem, u billing.LeaseUsage) {
	item.EventID = eventstore.LeaseEventID(billing.MerchantID(r.merchant), r.lease.ID, u.N)
	item.LeaseID, item.N = r.lease.ID, u.N
	if item.Source == "" {
		item.Source, item.SourceID = u.Source, u.SourceID
	}
}
