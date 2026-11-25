package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// PostingResult is the month-end close and the postings before balance
// movements, on a clock the run moves.
type PostingResult struct {
	Customers      int                   `json:"customers"`
	Events         int                   `json:"events_per_customer"`
	HeavyRows      int64                 `json:"heavy_rows"`
	IngestSeconds  float64               `json:"ingest_seconds"`
	IngestRate     float64               `json:"ingest_events_per_second"`
	GroupedSeconds float64               `json:"grouped_ingest_seconds"`
	GroupedRate    float64               `json:"grouped_ingest_events_per_second"`
	Syncs          *Hist                 `json:"syncs"`
	CloneSeconds   float64               `json:"heavy_clone_seconds"`
	TopUpLight     *Hist                 `json:"top_up_light"`
	TopUpHeavyMS   float64               `json:"top_up_heavy_ms"`
	LotExpiry      *PassResult           `json:"lot_expiry_pass"`
	Close          *PassResult           `json:"month_end_close"`
	CloseCustomers int64                 `json:"close_postings"`
	Rows           *RowStats             `json:"rows"`
	Storage        []TableStorage        `json:"clickhouse_storage"`
	Queries        map[string]QueryStats `json:"clickhouse_queries"`
	Checks         []Check               `json:"checks"`
	Errors         []string              `json:"errors"`
}

func (p *PostingResult) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[posting] %s\n", msg)
	p.Errors = append(p.Errors, msg)
}

func (p *PostingResult) check(name string, ok bool, format string, args ...any) {
	p.Checks = append(p.Checks, Check{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
	fmt.Fprintf(os.Stderr, "[posting] check %s ok=%v %s\n", name, ok, fmt.Sprintf(format, args...))
}

// runPosting reports usage as a batch reporter would (lease syncs), makes
// one customer heavy by copying its event in ClickHouse, times postings
// before top-ups, lets a lot lapse, and closes the month.
func (e *env) runPosting(ctx context.Context) *PostingResult {
	plan := e.plan
	const schema = "load_post"
	r := &PostingResult{Customers: plan.PostingCustomers, Events: plan.PostingEvents, HeavyRows: plan.HeavyRows, Syncs: newHist(), TopUpLight: newHist()}
	if err := e.resetWorld(ctx, schema); err != nil {
		r.fail("reset: %v", err)
		return r
	}
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second))
	eng, err := openrails.New(ctx, embeddedConfig(schema, schema, []string{"post"}), openrails.Deps{Postgres: e.pool, ClickHouse: e.ch, Clock: clock, FXTransport: fxWire{}, Logger: quietLogger(os.Stderr)})
	if err != nil {
		r.fail("engine: %v", err)
		return r
	}
	defer eng.Close(context.Background())
	id, err := eng.DeclaredMerchant("post")
	if err != nil {
		r.fail("merchant: %v", err)
		return r
	}
	c, _ := eng.Merchant(id)
	s := &seeded{slug: "post", id: id}
	logf("[posting] seeding %d customers", plan.PostingCustomers)
	if err := seed(ctx, c, plan, s, plan.PostingCustomers+2, 0); err != nil {
		r.fail("seed: %v", err)
		return r
	}
	light, heavy, lapsing := s.customers[:plan.PostingCustomers], s.customers[plan.PostingCustomers], s.customers[plan.PostingCustomers+1]
	if err := grantAll(ctx, c, []billing.CreateCreditGrantParams{{CustomerID: heavy, Currency: "USD", Amount: 100_000_000_000, Source: "load", SourceID: "heavy"}}); err != nil {
		r.fail("heavy funding: %v", err)
		return r
	}
	lapseAt := clock.Now().Add(time.Hour)
	lots, err := c.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{{CustomerID: lapsing, Currency: "USD", Amount: 5_000_000, Source: "load", SourceID: "lapsing", ExpiresAt: &lapseAt}})
	if err != nil {
		r.fail("lapsing lot: %v", err)
		return r
	}

	lease := billing.LeaseID(uuid.Must(uuid.NewV7()))
	var items []billing.LeaseUsage
	item := func(customer billing.CustomerID, units int64) {
		items = append(items, billing.LeaseUsage{N: int64(len(items) + 1), CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
			EventType: "compute", Dimensions: map[string]int64{"units": units}, Source: "load", SourceID: fmt.Sprintf("post-%d", len(items)+1)})
	}
	units := func() int64 { return plan.Units[0] + rand.Int64N(plan.Units[1]-plan.Units[0]+1) }
	// Half the events in time order across customers, half grouped by
	// customer: a sync settles at most so many customers.
	half := plan.PostingEvents / 2
	for range half {
		for _, cu := range light {
			item(cu, units())
		}
	}
	item(heavy, 1)
	for range 3 {
		item(lapsing, 1000)
	}
	start := time.Now()
	if err := e.syncAll(ctx, c, lease, items, r.Syncs); err != nil {
		r.fail("ingest: %v", err)
		return r
	}
	r.IngestSeconds = time.Since(start).Seconds()
	r.IngestRate = float64(len(items)) / r.IngestSeconds
	logf("[posting] ingested %d interleaved events in %.1fs", len(items), r.IngestSeconds)
	first := len(items)
	for _, cu := range light {
		for range plan.PostingEvents - half {
			item(cu, units())
		}
	}
	start = time.Now()
	if err := e.syncAll(ctx, c, lease, items[first:], r.Syncs); err != nil {
		r.fail("grouped ingest: %v", err)
		return r
	}
	r.GroupedSeconds = time.Since(start).Seconds()
	r.GroupedRate = float64(len(items)-first) / r.GroupedSeconds
	logf("[posting] ingested %d grouped events in %.1fs", len(items)-first, r.GroupedSeconds)

	if plan.HeavyRows > 1 {
		start = time.Now()
		if err := e.cloneHeavy(ctx, schema, id, heavy, plan.HeavyRows-1, clock.Now()); err != nil {
			r.fail("heavy clone: %v", err)
		}
		r.CloneSeconds = time.Since(start).Seconds()
		logf("[posting] heavy customer has %d events (%.1fs)", plan.HeavyRows, r.CloneSeconds)
	}

	// Postings before balance movements: each top-up posts the month so far.
	clock.Advance(time.Minute)
	for i, cu := range light[:min(5, len(light))] {
		start := time.Now()
		if _, err := c.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{{CustomerID: cu, Currency: "USD", Amount: 1_000_000, Source: "load", SourceID: fmt.Sprintf("top-up-%d", i)}}); err != nil {
			r.fail("top-up: %v", err)
			continue
		}
		r.TopUpLight.Add(time.Since(start))
	}
	start = time.Now()
	if _, err := c.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{{CustomerID: heavy, Currency: "USD", Amount: 1_000_000, Source: "load", SourceID: "top-up-heavy"}}); err != nil {
		r.fail("heavy top-up: %v", err)
	}
	r.TopUpHeavyMS = float64(time.Since(start).Microseconds()) / 1000
	logf("[posting] heavy top-up posted in %.0f ms", r.TopUpHeavyMS)

	fleet, err := riverhelpers.New(ctx, e.pool, &river.Config{Schema: schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}}}, eng.RiverJobs())
	if err != nil {
		r.fail("river: %v", err)
		return r
	}
	// Only the run's own passes: a periodic one would make them exit.
	fleet.PeriodicJobs().Clear()
	if err := eng.Start(ctx, openrails.WithRiverClient(fleet)); err != nil {
		r.fail("start: %v", err)
		return r
	}
	if err := fleet.Start(ctx); err != nil {
		r.fail("river start: %v", err)
		return r
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = fleet.StopAndCancel(sctx)
	}()

	// The lot lapses; usage after it is the lasting lot's.
	clock.Advance(lapseAt.Sub(clock.Now()) + time.Second)
	after := billing.LeaseID(uuid.Must(uuid.NewV7()))
	if err := e.syncAll(ctx, c, after, []billing.LeaseUsage{{N: 1, CustomerID: lapsing, Invoker: lapsing.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
		EventType: "compute", Dimensions: map[string]int64{"units": 500}, Source: "load", SourceID: "after-lapse"}}, r.Syncs); err != nil {
		r.fail("usage after lapse: %v", err)
	}
	r.LotExpiry = runPass(ctx, fleet)
	if lot, err := c.GetCreditGrant(ctx, lots[0].ID); err != nil {
		r.fail("lapsed lot: %v", err)
	} else {
		want := min(3*1000*plan.UnitPrice, lot.Amount)
		r.check("lot-expiry-charges-usage-before-lapse", lot.SpentAmount == want && lot.ExpiredAmount == lot.Amount-want,
			"lot of %d: spent %d (usage before the lapse %d), expired %d, remaining %d", lot.Amount, lot.SpentAmount, want, lot.ExpiredAmount, lot.RemainingAmount)
	}

	// The month ends: every customer's usage posts in one close.
	now := clock.Now().UTC()
	clock.Advance(time.Date(now.Year(), now.Month()+1, 1, 0, 1, 0, 0, time.UTC).Sub(now))
	var before int64
	_ = e.pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema}.Sanitize()+".usage_postings").Scan(&before)
	logf("[posting] closing the month for %d customers", plan.PostingCustomers+2)
	r.Close = runPass(ctx, fleet)
	logf("[posting] close: %s in %.1fs %s", r.Close.State, r.Close.Seconds, r.Close.Error)
	var after2, unposted int64
	_ = e.pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema}.Sanitize()+".usage_postings").Scan(&after2)
	_ = e.pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema}.Sanitize()+".usage_unposted WHERE due_at <= $1", clock.Now()).Scan(&unposted)
	r.CloseCustomers = after2 - before
	r.check("month-end-close-posts-everyone", r.Close.State == "completed" && unposted == 0, "%d postings by the close, %d customers still due", r.CloseCustomers, unposted)
	e.checkPostedOnce(ctx, r, schema, id)

	w := &WorldResult{Name: "posting"}
	e.checkLedger(ctx, w, schema, map[string]*seeded{"post": s})
	r.Checks = append(r.Checks, w.Checks...)
	r.Rows = e.rowStats(ctx, w, schema)
	if r.Storage, err = e.storage(ctx, schema); err != nil {
		w.fail("storage: %v", err)
	}
	r.Queries = e.queryStats(ctx, w, schema)
	r.Errors = append(r.Errors, w.Errors...)
	return r
}

// syncAll sends items on lease in syncs of the most a sync takes, resending
// what a sync left for the next.
func (e *env) syncAll(ctx context.Context, c *openrails.Client, lease billing.LeaseID, items []billing.LeaseUsage, h *Hist) error {
	for len(items) > 0 {
		batch := items[:min(len(items), billing.MaxSyncUsageItems)]
		start := time.Now()
		res, err := c.SyncLeases(ctx, billing.SyncLeasesParams{LeaseID: lease, Usage: batch})
		if err != nil {
			return err
		}
		h.Add(time.Since(start))
		if len(res.Usage.Refused) > 0 {
			return fmt.Errorf("%d items refused, first: %+v", len(res.Usage.Refused), res.Usage.Refused[0])
		}
		done := 0
		for done < len(batch) && batch[done].N <= res.Usage.ThroughN {
			done++
		}
		if done == 0 {
			return fmt.Errorf("a sync settled nothing (through %d)", res.Usage.ThroughN)
		}
		items = items[done:]
	}
	return nil
}

// cloneHeavy copies the customer's one event n times in ClickHouse, spread
// back over the month so far: a month of a heavy customer's usage, which
// every posting of the customer scans under FINAL. The copies go in as
// intake inserts them, so every rollup counts them, priced per unit. Each
// copy keeps the event's pricing period, so it is spread back no further
// than that period's start: intake never dates an event before its period
// (an earlier copy would be usage of the previous period filed under this
// one, which no posting of either period reads).
func (e *env) cloneHeavy(ctx context.Context, database string, merchant billing.MerchantID, customer billing.CustomerID, n int64, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var period time.Time
	if err := e.ch.QueryRow(ctx, fmt.Sprintf("SELECT period_starts_at FROM `%s`.usage_events WHERE merchant_id = toUUID('%s') AND customer_id = toUUID('%s') LIMIT 1",
		database, merchant, customer)).Scan(&period); err != nil {
		return fmt.Errorf("the heavy customer's period: %w", err)
	}
	if period.After(from) {
		from = period
	}
	spread := max(int64(now.Sub(from).Seconds())-60, 1)
	back := fmt.Sprintf("toIntervalSecond(number %% %d)", spread)
	q := fmt.Sprintf("INSERT INTO `%[1]s`.usage_intake (merchant_id, event_id, customer_id, currency, event_type, invoker, invoker_type, outcome, source, source_id, "+
		"lease_id, n, occurred_at, admitted_at, ingested_at, period_starts_at, day_at, dimensions, metadata, amount, stated, charged, forgiven, "+
		"`charges.meter`, `charges.cell`, `charges.print`, `charges.quantity`, `charges.stated`) "+
		"SELECT merchant_id, generateUUIDv4(number), customer_id, currency, event_type, invoker, invoker_type, outcome, source, concat(source_id, '-', toString(number)), "+
		"lease_id, n + 1000000 + number, occurred_at - %[2]s, admitted_at - %[2]s, ingested_at, period_starts_at, toStartOfDay(toDateTime(occurred_at - %[2]s, 'UTC')), "+
		"dimensions, metadata, toInt64(arraySum(charges.quantity)) * %[3]d, stated, charged, forgiven, "+
		"charges.meter, charges.cell, charges.print, charges.quantity, charges.stated "+
		"FROM `%[1]s`.usage_events AS src CROSS JOIN numbers(%[4]d) AS num WHERE src.merchant_id = toUUID('%[5]s') AND src.customer_id = toUUID('%[6]s')",
		database, back, e.plan.UnitPrice, n, merchant, customer)
	return e.ch.Exec(ctx, q)
}

// runPass runs one billing pass on fleet and waits for it.
func runPass(ctx context.Context, fleet *river.Client[pgx.Tx]) *PassResult {
	out := &PassResult{EnqueuedAt: time.Now().UTC()}
	events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer unsubscribe()
	job, err := fleet.Insert(ctx, registeredJob{KindName: "openrails.billing_pass"}, &river.InsertOpts{Queue: openrails.QueueBilling, MaxAttempts: 1})
	if err != nil {
		out.Error = err.Error()
		return out
	}
	timeout := time.After(15 * time.Minute)
	for {
		select {
		case ev := <-events:
			if ev.Job.ID != job.Job.ID {
				continue
			}
			out.Seconds = time.Since(out.EnqueuedAt).Seconds()
			out.State = string(ev.Job.State)
			if len(ev.Job.Errors) > 0 {
				out.Error = ev.Job.Errors[len(ev.Job.Errors)-1].Error
			}
			return out
		case <-timeout:
			out.State = "unfinished"
			return out
		case <-ctx.Done():
			out.State = "cancelled"
			return out
		}
	}
}

// checkPostedOnce compares, per customer, what the ledger took for usage
// with ClickHouse's deduplicated total: every event posted once.
func (e *env) checkPostedOnce(ctx context.Context, r *PostingResult, schema string, merchant billing.MerchantID) {
	posted := map[string]int64{}
	rows, err := e.pool.Query(ctx, "SELECT customer_id::text, sum(amount)::bigint FROM "+pgx.Identifier{schema}.Sanitize()+".usage_postings WHERE merchant_id = $1 GROUP BY 1", merchant.UUID())
	if err != nil {
		r.fail("postings: %v", err)
		return
	}
	for rows.Next() {
		var c string
		var n int64
		if rows.Scan(&c, &n) == nil {
			posted[c] = n
		}
	}
	rows.Close()
	crows, err := e.ch.Query(ctx, "SELECT toString(customer_id), toInt64(sum(arraySum(charges.quantity))) * "+strconv.FormatInt(e.plan.UnitPrice, 10)+
		" FROM `"+schema+"`.usage_events FINAL WHERE merchant_id = "+uuidLit(merchant.UUID())+" GROUP BY customer_id")
	if err != nil {
		r.fail("final totals: %v", err)
		return
	}
	defer crows.Close()
	var customers, mismatched int
	var examples []string
	for crows.Next() {
		var c string
		var n int64
		if crows.Scan(&c, &n) != nil {
			continue
		}
		customers++
		if posted[c] != n {
			mismatched++
			if len(examples) < 3 {
				examples = append(examples, fmt.Sprintf("%s posted %d of %d", c, posted[c], n))
			}
		}
	}
	r.check("posted-once", customers > 0 && mismatched == 0, "%d customers: posted usage equals the FINAL total for %d, %d differ %v", customers, customers-mismatched, mismatched, examples)
}
