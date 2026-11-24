package merchantarchive

import (
	"context"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
)

// The archive's event store section is the merchant's usage in ClickHouse
// (contract.EventStoreProfiles), after every table of PostgreSQL. Export
// writes it from the deployment's store; a deployment without one exports
// none, and refuses a merchant with usage the ledger has not taken. Restore
// needs a store, refuses a merchant that already has usage there, inserts
// the events and then the rollup rows they do not rebuild, and deletes them
// again when the restore fails.

const eventStorePrefix = "event_store."

func exportUsage(ctx context.Context, database *db.DB, q *gen.Queries, id billing.MerchantID, w *archivewire.Writer) error {
	store := database.EventStore()
	if store == nil {
		unposted, err := q.ListMerchantUsageUnposted(ctx, id.UUID())
		if err != nil {
			return err
		}
		if len(unposted) > 0 {
			return &Error{Code: "unsupported_state", Table: "usage_unposted", Count: int64(len(unposted)), Err: billing.ErrEventStoreRequired}
		}
		return nil
	}
	current := ""
	return store.ExportUsage(ctx, func(table string, values []*string) error {
		name := eventStorePrefix + table
		if name != current {
			if err := w.Table(name); err != nil {
				return err
			}
			current = name
		}
		p := contract.EventStoreProfiles[profileIndex(name)]
		if err := contract.ValidateValues(p, values); err != nil {
			return &Error{Code: "unsupported_state", Table: name, Count: 1, Err: err}
		}
		return w.Row(values)
	})
}

func profileIndex(name string) int {
	for i, p := range contract.EventStoreProfiles {
		if p.Name == name {
			return i
		}
	}
	return -1
}

// usageRestore buffers an archive's usage and inserts it, table by table.
type usageRestore struct {
	store    *eventstore.Store
	table    string
	rows     [][]*string
	inserted bool
}

// restoreBatch is how many archived rows one insert takes.
const restoreBatch = 5000

// restoringUsage prepares a restore's usage into the deployment's store.
func restoringUsage(_ context.Context, database *db.DB) (*usageRestore, error) {
	return &usageRestore{store: database.EventStore()}, nil
}

func (u *usageRestore) takes(p contract.Profile) bool {
	return strings.HasPrefix(p.Name, eventStorePrefix)
}

func (u *usageRestore) add(ctx context.Context, p contract.Profile, values []*string) error {
	if u.store == nil {
		return &Error{Code: "unsupported_state", Table: p.Name, Count: 1, Err: billing.ErrEventStoreRequired}
	}
	if u.table == "" {
		// The merchant's usage must all come from the archive.
		has, err := u.store.HasUsage(ctx)
		if err != nil {
			return err
		}
		if has {
			return &Error{Code: "not_empty", Table: p.Name}
		}
	}
	table := strings.TrimPrefix(p.Name, eventStorePrefix)
	if table != u.table || len(u.rows) >= restoreBatch {
		if err := u.flush(ctx); err != nil {
			return err
		}
		u.table = table
	}
	u.rows = append(u.rows, values)
	return nil
}

func (u *usageRestore) flush(ctx context.Context) error {
	if len(u.rows) == 0 {
		return nil
	}
	u.inserted = true
	err := u.store.ImportUsage(ctx, u.table, u.rows)
	u.rows = nil
	return err
}

// discard deletes what a failed restore inserted.
func (u *usageRestore) discard(ctx context.Context) {
	if u != nil && u.inserted {
		_ = u.store.DeleteMerchant(ctx)
	}
}
