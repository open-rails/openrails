package money

import (
	"context"

	"github.com/open-rails/openrails/internal/eventstore"
)

// UsageSettlement is one transaction's usage intake: the events its batches
// settled in PostgreSQL (PrepareUsageTx, CommitUsageTx) and the balances
// they moved. The event store takes the events only once the transaction
// committed (Insert): a transaction that rolls back inserted nothing, and a
// resend of what committed settles and inserts nothing again, so no rollup
// counts an item twice. Events the store never takes (a crash between the
// commit and the insert, or a store that fails for good) are charged and
// missing from its history and limit usage.
type UsageSettlement struct {
	store   *eventstore.Store
	events  []eventstore.UsageEvent
	changed []UsageAccount
}

// NewUsageSettlement starts a transaction's usage intake into store.
func NewUsageSettlement(store *eventstore.Store) *UsageSettlement {
	return &UsageSettlement{store: store}
}

// Events is how many events the transaction settled.
func (u *UsageSettlement) Events() int { return len(u.events) }

// Changed are the balances the settled usage took from, for the caller to
// watch once committed (WatchUsage): one that crosses a line of its balance
// watch queues a look, and one that takes arrears to the threshold asks for a
// usage invoice.
func (u *UsageSettlement) Changed() []UsageAccount { return u.changed }

// Ping, before the transaction commits, refuses events the store cannot
// take now: committed while it is out of reach, they would be lost.
func (u *UsageSettlement) Ping(ctx context.Context) error {
	if len(u.events) == 0 {
		return nil
	}
	return u.store.Ping(ctx)
}

// Insert takes the settled events into the event store once the transaction
// committed, in one insert that returns when they read back: a view answered
// after it counts them.
func (u *UsageSettlement) Insert(ctx context.Context) error {
	if len(u.events) == 0 {
		return nil
	}
	return u.store.InsertUsage(ctx, u.events)
}
