package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// StorageResult is what the event store keeps per usage event. Usage is taken
// in through lease syncs on a clock that spreads it over days, into two
// databases: one whose days end now, and one whose days end past the age at
// which usage history coarsens. Each is measured from its active parts once
// TTLs are applied and the parts merged.
type StorageResult struct {
	Shape  string          `json:"shape"`
	Worlds []*StorageWorld `json:"worlds"`
}

// StorageWorld is one database's usage and what it occupies.
type StorageWorld struct {
	Name          string          `json:"name"`
	From          time.Time       `json:"from"`
	To            time.Time       `json:"to"`
	Sent          int64           `json:"sent"`
	Events        int64           `json:"events"`
	IngestSeconds float64         `json:"ingest_seconds"`
	IngestRate    float64         `json:"ingest_events_per_second"`
	Syncs         *Hist           `json:"syncs"`
	Tables        []TableStorage  `json:"tables"`
	Columns       []ColumnStorage `json:"usage_events_columns"`
	History       *HistoryRows    `json:"history_rows,omitempty"`
	Checks        []Check         `json:"checks"`
	Errors        []string        `json:"errors"`
}

func (w *StorageWorld) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[storage/%s] %s\n", w.Name, msg)
	w.Errors = append(w.Errors, msg)
}

func (w *StorageWorld) check(name string, ok bool, format string, args ...any) {
	w.Checks = append(w.Checks, Check{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
	fmt.Fprintf(os.Stderr, "[storage/%s] check %s ok=%v %s\n", w.Name, name, ok, fmt.Sprintf(format, args...))
}

// ColumnStorage is one column's share of a table.
type ColumnStorage struct {
	Column       string `json:"column"`
	Compressed   uint64 `json:"compressed"`
	Uncompressed uint64 `json:"uncompressed"`
}

// HistoryRows are usage history's rows as stored, and as many as one row per
// UTC day would be, with and without the invoker in the key.
type HistoryRows struct {
	Rows             uint64 `json:"rows"`
	DayRows          uint64 `json:"day_rows"`
	DayRowsNoInvoker uint64 `json:"day_rows_without_invoker"`
}

// storageAge is how far past now the aged database's days end: beyond the
// age at which usage history folds into days.
const storageAge = 36 * 24 * time.Hour

func (e *env) runStorage(ctx context.Context) *StorageResult {
	plan := e.plan
	r := &StorageResult{Shape: fmt.Sprintf("%d customers, %d events a customer a day over %d days, at random instants, each from one of %d invokers (the customer and its API keys); "+
		"%d to %d units at %d micros a unit, a unique source_id, metadata {model, region}, and one daily spend window; %d reporters, each a lease a day, syncing its customers' items in time order",
		plan.StorageCustomers, plan.StoragePerDay, plan.StorageDays, plan.StorageInvokers, plan.Units[0], plan.Units[1], plan.UnitPrice, plan.StorageReporters)}
	end := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	span := time.Duration(plan.StorageDays) * 24 * time.Hour
	for _, s := range []struct {
		name string
		end  time.Time
	}{{"recent", end}, {"aged", end.Add(-storageAge)}} {
		if ctx.Err() != nil {
			break
		}
		r.Worlds = append(r.Worlds, e.storageWorld(ctx, s.name, s.end.Add(-span), s.end))
	}
	return r
}

func (e *env) storageWorld(ctx context.Context, name string, from, to time.Time) *StorageWorld {
	plan := e.plan
	w := &StorageWorld{Name: name, From: from, To: to, Syncs: newHist()}
	schema := "load_store_" + name
	if err := e.resetWorld(ctx, schema); err != nil {
		w.fail("reset: %v", err)
		return w
	}
	clock := clockwork.NewFakeClockAt(from)
	cfg := embeddedConfig(schema, schema, []string{"store"})
	cfg.Merchants[0].BillingPolicies = []billing.BillingPolicy{{Name: "daily", Kind: "window_spend_cap",
		SpendWindows: []billing.BudgetWindow{{Key: "day", WindowSeconds: 86400, Limit: 1_000_000_000_000_000, Currency: "USD"}}}}
	cfg.Merchants[0].BillingPolicyBindings = []billing.BillingPolicyBinding{{PolicyName: "daily"}}
	eng, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: e.pool, ClickHouse: e.ch, Clock: clock, FXTransport: fxWire{}, Logger: quietLogger(os.Stderr)})
	if err != nil {
		w.fail("engine: %v", err)
		return w
	}
	defer eng.Close(context.Background())
	id, err := eng.DeclaredMerchant("store")
	if err != nil {
		w.fail("merchant: %v", err)
		return w
	}
	c, _ := eng.Merchant(id)
	s := &seeded{slug: "store", id: id}
	logf("[storage/%s] seeding %d customers", name, plan.StorageCustomers)
	if err := seed(ctx, c, plan, s, plan.StorageCustomers, 0); err != nil {
		w.fail("seed: %v", err)
		return w
	}
	rng := rand.New(rand.NewPCG(1, 2))
	invokers := make([][]string, len(s.customers))
	for i, cu := range s.customers {
		invokers[i] = []string{cu.String()}
		for range plan.StorageInvokers - 1 {
			invokers[i] = append(invokers[i], fmt.Sprintf("key_%016x", rng.Uint64()))
		}
	}
	models, regions := []string{"small", "medium", "large"}, []string{"us-east-1", "eu-west-1"}
	reporters := max(plan.StorageReporters, 1)
	start := time.Now()
	budget := time.Duration(max(plan.StorageMinutes, 1)) * time.Minute
	for day := from; day.Before(to) && ctx.Err() == nil; day = day.Add(24 * time.Hour) {
		if time.Since(start) > budget {
			logf("[storage/%s] out of time: usage to %s", name, day.Format(time.DateOnly))
			w.To = day
			break
		}
		end := day.Add(24 * time.Hour)
		if end.After(to) {
			end = to
		}
		shares := make([][]billing.LeaseUsage, reporters)
		for ci, cu := range s.customers {
			for range plan.StoragePerDay {
				invoker := invokers[ci][rng.IntN(len(invokers[ci]))]
				units := plan.Units[0] + rng.Int64N(plan.Units[1]-plan.Units[0]+1)
				shares[ci%reporters] = append(shares[ci%reporters], billing.LeaseUsage{CustomerID: cu, Invoker: invoker, InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
					EventType: "compute", Dimensions: map[string]int64{"units": units}, Metadata: map[string]any{"model": models[rng.IntN(len(models))], "region": regions[rng.IntN(len(regions))]},
					Source: "api", SourceID: uuid.NewString(), OccurredAt: day.Add(time.Duration(rng.Int64N(int64(end.Sub(day)))))})
			}
		}
		// The day's usage is reported as it ends.
		clock.Advance(end.Add(time.Minute).Sub(clock.Now()))
		var wg sync.WaitGroup
		var mu sync.Mutex
		var errs []error
		for _, items := range shares {
			slices.SortFunc(items, func(a, b billing.LeaseUsage) int { return a.OccurredAt.Compare(b.OccurredAt) })
			for i := range items {
				items[i].N = int64(i + 1)
			}
			w.Sent += int64(len(items))
			lease := leaseMintedAt(day)
			wg.Go(func() {
				h := newHist()
				err := e.syncAll(ctx, c, lease, items, h)
				mu.Lock()
				defer mu.Unlock()
				w.Syncs.Merge(h)
				if err != nil {
					errs = append(errs, err)
				}
			})
		}
		wg.Wait()
		if len(errs) > 0 {
			w.fail("ingest %s: %v", day.Format(time.DateOnly), errs[0])
			return w
		}
	}
	w.IngestSeconds = time.Since(start).Seconds()
	w.IngestRate = float64(w.Sent) / w.IngestSeconds
	logf("[storage/%s] ingested %d events in %.1fs (%.0f/s)", name, w.Sent, w.IngestSeconds, w.IngestRate)
	if err := e.settleTables(ctx, schema); err != nil {
		w.fail("merge: %v", err)
	}
	var events, requests uint64
	if err := e.ch.QueryRow(ctx, "SELECT count() FROM `"+schema+"`.usage_events FINAL").Scan(&events); err != nil {
		w.fail("events: %v", err)
	}
	if err := e.ch.QueryRow(ctx, "SELECT toUInt64(sum(requests)) FROM `"+schema+"`.usage_history WHERE meter = ''").Scan(&requests); err != nil {
		w.fail("history: %v", err)
	}
	w.Events = int64(events)
	w.check("stored-once", w.Events == w.Sent, "%d sent, %d under FINAL", w.Sent, w.Events)
	w.check("history-counts-each-once", int64(requests) == w.Sent, "%d events in usage history", requests)
	if w.Tables, err = e.storage(ctx, schema); err != nil {
		w.fail("storage: %v", err)
	}
	if w.Columns, err = e.columns(ctx, schema, "usage_events"); err != nil {
		w.fail("columns: %v", err)
	}
	h := &HistoryRows{}
	if err := e.ch.QueryRow(ctx, "SELECT count(), uniqExact(customer_id, toDate(bucket_at), currency, event_type, meter, cell, print, period_starts_at, invoker, outcome), "+
		"uniqExact(customer_id, toDate(bucket_at), currency, event_type, meter, cell, print, period_starts_at, outcome) FROM `"+schema+"`.usage_history").Scan(&h.Rows, &h.DayRows, &h.DayRowsNoInvoker); err != nil {
		w.fail("history rows: %v", err)
	} else {
		w.History = h
	}
	return w
}

// settleTables applies every TTL of the database's tables and merges each
// into its final parts: what the store holds once background merges caught
// up.
func (e *env) settleTables(ctx context.Context, database string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	rows, err := e.ch.Query(ctx, "SELECT name, position(engine_full, ' TTL ') > 0 FROM system.tables WHERE database = ? AND engine LIKE '%MergeTree' ORDER BY name", database)
	if err != nil {
		return err
	}
	type table struct {
		name string
		ttl  bool
	}
	var tables []table
	for rows.Next() {
		var t table
		if err := rows.Scan(&t.name, &t.ttl); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, t)
	}
	rows.Close()
	for _, t := range tables {
		q := "`" + database + "`.`" + t.name + "`"
		if t.ttl {
			if err := e.ch.Exec(ctx, "ALTER TABLE "+q+" MATERIALIZE TTL SETTINGS mutations_sync = 2"); err != nil {
				return fmt.Errorf("%s: %w", t.name, err)
			}
		}
		if err := e.ch.Exec(ctx, "OPTIMIZE TABLE "+q+" FINAL"); err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
	}
	return nil
}

func (e *env) columns(ctx context.Context, database, table string) ([]ColumnStorage, error) {
	rows, err := e.ch.Query(ctx, "SELECT name, data_compressed_bytes, data_uncompressed_bytes FROM system.columns WHERE database = ? AND table = ? ORDER BY data_compressed_bytes DESC, name", database, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColumnStorage
	for rows.Next() {
		var c ColumnStorage
		if err := rows.Scan(&c.Column, &c.Compressed, &c.Uncompressed); err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// leaseMintedAt is a UUIDv7 lease id minted at an instant, as a reporter
// would mint it then.
func leaseMintedAt(at time.Time) billing.LeaseID {
	id := uuid.Must(uuid.NewV7())
	ms := uint64(at.UnixMilli())
	for i := range 6 {
		id[i] = byte(ms >> (40 - 8*i))
	}
	return billing.LeaseID(id)
}
