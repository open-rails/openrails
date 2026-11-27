package db

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventstore"
)

// SetEventStore binds the event store (Deps.ClickHouse) every handle derived
// from d reads usage through; nil leaves usage off.
func (d *DB) SetEventStore(store *eventstore.Store) {
	if d == nil {
		return
	}
	if d.river == nil {
		d.river = &runtimeBinding{}
	}
	d.river.mu.Lock()
	defer d.river.mu.Unlock()
	d.river.events = store
}

// EventStore is the bound event store; nil without one.
func (d *DB) EventStore() *eventstore.Store {
	if d == nil || d.river == nil {
		return nil
	}
	d.river.mu.RLock()
	defer d.river.mu.RUnlock()
	return d.river.events
}

// EventStoreOf is the event store bound to the DB whose transaction q runs
// in; nil outside one, or without a store.
func EventStoreOf(q *gen.Queries) *eventstore.Store {
	if q == nil {
		return nil
	}
	tx, ok := q.DBTX().(schemaTx)
	if !ok || tx.river == nil {
		return nil
	}
	tx.river.mu.RLock()
	defer tx.river.mu.RUnlock()
	return tx.river.events
}

// UsageLedger posts the usage intake settled. The runtime binds the money
// service, so code holding only a transaction (credit lots, a merchant
// purge) reaches it.
type UsageLedger interface {
	// PostUsage posts, at now, what a customer's usage in a currency costs
	// and the ledger has not taken: everything with a zero cut, else what
	// occurred before cut. reason is usage_receipts.reason.
	PostUsage(ctx context.Context, q *gen.Queries, merchant, customer uuid.UUID, currency string, cut, now time.Time, reason string) (bool, error)
}

// SetUsageLedger binds the usage ledger every handle derived from d reaches.
func (d *DB) SetUsageLedger(l UsageLedger) {
	if d == nil {
		return
	}
	if d.river == nil {
		d.river = &runtimeBinding{}
	}
	d.river.mu.Lock()
	defer d.river.mu.Unlock()
	d.river.usage = l
}

// UsageLedgerOf is the usage ledger bound to the DB whose transaction q runs
// in; nil outside one.
func UsageLedgerOf(q *gen.Queries) UsageLedger {
	if q == nil {
		return nil
	}
	tx, ok := q.DBTX().(schemaTx)
	if !ok || tx.river == nil {
		return nil
	}
	tx.river.mu.RLock()
	defer tx.river.mu.RUnlock()
	return tx.river.usage
}

// InTx is d bound to the transaction q runs in, so work queued through it
// commits with that transaction; d itself outside one.
func (d *DB) InTx(q *gen.Queries) *DB {
	if d == nil {
		return d
	}
	t := TxOf(q)
	if t == nil {
		return d
	}
	t.Log = d.Log
	return t
}

// TxOf is the handle of the transaction q runs in, with its bindings; nil
// outside one.
func TxOf(q *gen.Queries) *DB {
	if q == nil {
		return nil
	}
	tx, ok := q.DBTX().(schemaTx)
	if !ok {
		return nil
	}
	return &DB{pgtx: tx, rw: tx.rw, river: tx.river}
}
