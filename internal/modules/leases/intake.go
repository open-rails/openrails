package leases

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/modules/money"
)

// intake settles a chunk of the sync's usage in its transaction: priced and
// located, then written to PostgreSQL. Its events join the sync's
// settlement, which the event store takes once the sync committed.
func (r *syncRun) intake(items []money.UsageItem) ([]money.UsageResult, error) {
	in, err := r.money.PrepareUsageTx(r.ctx, r.q, r.settled, items)
	if err != nil {
		return nil, err
	}
	if err := r.money.CommitUsageTx(r.ctx, r.q, in); err != nil {
		return nil, err
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
