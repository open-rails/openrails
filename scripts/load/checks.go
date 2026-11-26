package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// boundWindow is how far back the bound counts others' admissions: a view
// decides for up to two sync intervals and an admission reaches the server
// by its gate's second sync.
const boundWindow = 4 * limits.SyncInterval

// Overshoot is how far draining customers went below zero, against the
// gate's bound.
type Overshoot struct {
	Customers   int                 `json:"customers"`
	Drained     int                 `json:"drained"`
	Settled     int                 `json:"settled_exactly"`
	Violations  int                 `json:"violations"`
	Max         int64               `json:"max_overshoot"`
	Median      int64               `json:"median_overshoot_of_drained"`
	MaxBound    int64               `json:"max_bound"`
	MaxRatio    float64             `json:"max_overshoot_to_bound"`
	ServerShare float64             `json:"server_decided_share"`
	PerCustomer []CustomerOvershoot `json:"per_customer"`
}

type CustomerOvershoot struct {
	Merchant  string `json:"merchant"`
	Customer  string `json:"customer"`
	Funding   int64  `json:"funding"`
	Captured  int64  `json:"captured"`
	Available int64  `json:"available"`
	Overshoot int64  `json:"overshoot"`
	Bound     int64  `json:"bound"`
	Gates     int    `json:"gates"`
	Admitted  int    `json:"admitted"`
	Server    int    `json:"server_decided"`
}

// overshootBound is the bound for one customer's admissions: the most any
// one gate's view could miss (what every other gate admitted in the
// boundWindow before the customer's last admission) plus one request per
// gate deciding on a stale view. It leaves out the margin, which only
// tightens the bound, so a breach of it is a breach of the design's.
func overshootBound(adms []admission, window time.Duration, maxCost int64) (bound int64, gates int) {
	if len(adms) == 0 {
		return 0, 0
	}
	last := adms[0].At
	procs := map[int]bool{}
	for _, a := range adms {
		last = max(last, a.At)
		procs[a.Proc] = true
	}
	own := map[int]int64{}
	var total int64
	for _, a := range adms {
		if a.At >= last-window.Nanoseconds() {
			own[a.Proc] += a.Cost
			total += a.Cost
		}
	}
	var others int64
	for p := range procs {
		others = max(others, total-own[p])
	}
	return others + int64(len(procs))*maxCost, len(procs)
}

// checkWorld compares what the gates captured with what ClickHouse stored,
// the draining customers' balances with the bound, and the ledger.
func (e *env) checkWorld(ctx context.Context, w *WorldResult, schema string, seeds map[string]*seeded, admin map[string]*openrails.Client) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	bySlug := map[string]string{}
	for slug, s := range seeds {
		bySlug[s.id.String()] = slug
	}
	type stored struct{ n, amount int64 }
	final := map[string]map[string]stored{}
	// Raw events keep their priced quantities: at a per-unit price, the
	// amount a gate captured is the quantity times it.
	rows, err := e.ch.Query(ctx, "SELECT toString(merchant_id), toString(customer_id), count(), toInt64(sum(arraySum(charges.quantity))) FROM `"+schema+"`.usage_events FINAL GROUP BY merchant_id, customer_id")
	if err != nil {
		w.fail("final counts: %v", err)
	} else {
		for rows.Next() {
			var m, c string
			var n uint64
			var quantity int64
			if err := rows.Scan(&m, &c, &n, &quantity); err != nil {
				w.fail("final counts: %v", err)
				break
			}
			slug := bySlug[m]
			if final[slug] == nil {
				final[slug] = map[string]stored{}
			}
			final[slug][c] = stored{int64(n), quantity * e.plan.UnitPrice}
		}
		_ = rows.Close()
	}
	raw := map[string]int64{}
	if rows, err := e.ch.Query(ctx, "SELECT toString(merchant_id), count() FROM `"+schema+"`.usage_events GROUP BY merchant_id"); err == nil {
		for rows.Next() {
			var m string
			var n uint64
			if rows.Scan(&m, &n) == nil {
				raw[bySlug[m]] = int64(n)
			}
		}
		_ = rows.Close()
	}
	for _, slug := range sortedKeys(seeds) {
		if slug == crashSlug {
			continue
		}
		var captured, kept, amountGap, missing, extra int64
		for c, n := range w.captured[slug] {
			captured += n
			got := final[slug][c]
			switch {
			case got.n < n:
				missing += n - got.n
			case got.n > n:
				extra += got.n - n
			}
			amountGap += got.amount - w.amounts[slug][c]
		}
		for c, got := range final[slug] {
			kept += got.n
			if _, ok := w.captured[slug][c]; !ok {
				extra += got.n
				amountGap += got.amount
			}
		}
		w.check("stored-once/"+slug, missing == 0 && extra == 0 && raw[slug] == kept && amountGap == 0,
			"captured %d, stored %d under FINAL (%d raw); missing %d, extra %d, amount difference %d micros", captured, kept, raw[slug], missing, extra, amountGap)
	}
	e.checkDrains(ctx, w, seeds, admin)
	e.checkLedger(ctx, w, schema, seeds)
	w.Rows = e.rowStats(ctx, w, schema)
	if w.Storage, err = e.storage(ctx, schema); err != nil {
		w.fail("storage: %v", err)
	}
	w.Queries = e.queryStats(ctx, w, schema)
}

func (e *env) checkDrains(ctx context.Context, w *WorldResult, seeds map[string]*seeded, admin map[string]*openrails.Client) {
	byCustomer := map[string][]admission{}
	for _, a := range w.admissions {
		byCustomer[a.Customer] = append(byCustomer[a.Customer], a)
	}
	o := &Overshoot{}
	var drained []int64
	var admitted, server int
	for _, slug := range sortedKeys(seeds) {
		s := seeds[slug]
		for _, c := range s.drains {
			b, err := admin[slug].GetBalance(ctx, c, billing.GetBalanceParams{Currency: "USD"})
			if err != nil {
				w.fail("balance of %s: %v", c, err)
				continue
			}
			adms := byCustomer[c.String()]
			sort.Slice(adms, func(i, j int) bool { return adms[i].At < adms[j].At })
			bound, gates := overshootBound(adms, boundWindow, e.plan.maxCost())
			co := CustomerOvershoot{Merchant: slug, Customer: c.String(), Funding: e.plan.DrainFunding, Captured: w.amounts[slug][c.String()],
				Available: b.AvailableAmount, Overshoot: max(0, -b.AvailableAmount), Bound: bound, Gates: gates, Admitted: len(adms)}
			for _, a := range adms {
				if a.Path == "server" {
					co.Server++
				}
			}
			admitted += co.Admitted
			server += co.Server
			o.Customers++
			if co.Funding-co.Captured == co.Available {
				o.Settled++
			}
			if co.Available <= 0 || co.Funding-co.Captured < e.plan.maxCost() {
				o.Drained++
				drained = append(drained, co.Overshoot)
			}
			if co.Overshoot > co.Bound {
				o.Violations++
			}
			o.Max = max(o.Max, co.Overshoot)
			o.MaxBound = max(o.MaxBound, co.Bound)
			if co.Bound > 0 {
				o.MaxRatio = max(o.MaxRatio, float64(co.Overshoot)/float64(co.Bound))
			}
			o.PerCustomer = append(o.PerCustomer, co)
		}
	}
	if o.Customers == 0 {
		return
	}
	if len(drained) > 0 {
		slices.Sort(drained)
		o.Median = drained[len(drained)/2]
	}
	if admitted > 0 {
		o.ServerShare = float64(server) / float64(admitted)
	}
	w.Overshoot = o
	w.check("overshoot-within-bound", o.Violations == 0, "%d of %d draining customers drained; max overshoot %d micros, max bound %d, max ratio %.2f, %d over the bound",
		o.Drained, o.Customers, o.Max, o.MaxBound, o.MaxRatio, o.Violations)
	w.check("drain-balances-settled-once", o.Settled == o.Customers, "%d of %d balances equal funding less the captured usage", o.Settled, o.Customers)
}

// checkLedger runs the books' integrity diagnostics per merchant, as
// openrails ledger-audit does.
func (e *env) checkLedger(ctx context.Context, w *WorldResult, schema string, seeds map[string]*seeded) {
	database, err := db.NewWithPGXPool(quietLogger(os.Stderr), e.pool, schema)
	if err != nil {
		w.fail("ledger audit: %v", err)
		return
	}
	for _, slug := range sortedKeys(seeds) {
		id := seeds[slug].id
		var rep ledger.IntegrityReport
		err := database.RunInMerchantConn(merchant.WithID(ctx, id), func(ctx context.Context) error {
			var err error
			rep, err = ledger.CheckIntegrity(ctx, database.Qx(ctx), id.UUID())
			return err
		})
		detail := fmt.Sprintf("conservation %d, counters %d, lots %d, rollups %d, signs %d, applications %d, unscheduled %d",
			len(rep.Conservation), len(rep.Counters), len(rep.Lots), len(rep.Rollups), len(rep.Signs), len(rep.Applications), len(rep.Unscheduled))
		if err != nil {
			detail = err.Error()
		}
		w.check("ledger-balances/"+slug, err == nil && rep.OK(), "%s", detail)
	}
}

// RowStats is what the run left in PostgreSQL: ledger rows grow with
// customers and balance movements, not with requests.
type RowStats struct {
	Customers        int64            `json:"customers_with_transfers"`
	LedgerTransfers  int64            `json:"ledger_transfers"`
	ByType           map[string]int64 `json:"ledger_transfers_by_type"`
	UsagePostings    int64            `json:"usage_postings"`
	Unposted         int64            `json:"usage_pending"`
	UsageEvents      int64            `json:"usage_events"`
	PerCustomerMax   int64            `json:"ledger_transfers_per_customer_max"`
	TableBytes       map[string]int64 `json:"table_bytes"`
	TransfersPerUser float64          `json:"ledger_transfers_per_customer"`
}

func (e *env) rowStats(ctx context.Context, w *WorldResult, schema string) *RowStats {
	r := &RowStats{ByType: map[string]int64{}, TableBytes: map[string]int64{}}
	q := func(sql string, dst ...any) {
		if err := e.pool.QueryRow(ctx, sql).Scan(dst...); err != nil {
			w.fail("rows: %s: %v", sql, err)
		}
	}
	t := pgx.Identifier{schema}.Sanitize()
	q("SELECT count(*), count(DISTINCT customer_id) FROM "+t+".ledger_transfers WHERE customer_id IS NOT NULL", &r.LedgerTransfers, &r.Customers)
	q("SELECT coalesce(max(n), 0) FROM (SELECT count(*) n FROM "+t+".ledger_transfers WHERE customer_id IS NOT NULL GROUP BY customer_id) x", &r.PerCustomerMax)
	q("SELECT count(*) FROM "+t+".usage_postings", &r.UsagePostings)
	q("SELECT count(*) FROM "+t+".usage_pending", &r.Unposted)
	if rows, err := e.pool.Query(ctx, "SELECT transfer_type, count(*) FROM "+t+".ledger_transfers GROUP BY 1"); err == nil {
		for rows.Next() {
			var k string
			var n int64
			if rows.Scan(&k, &n) == nil {
				r.ByType[k] = n
			}
		}
		rows.Close()
	}
	if rows, err := e.pool.Query(ctx, "SELECT c.relname, pg_total_relation_size(c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind IN ('r','p') ORDER BY 2 DESC LIMIT 12", schema); err == nil {
		for rows.Next() {
			var k string
			var n int64
			if rows.Scan(&k, &n) == nil {
				r.TableBytes[k] = n
			}
		}
		rows.Close()
	}
	if n, err := e.countEvents(ctx, schema); err == nil {
		r.UsageEvents = n
	}
	if r.Customers > 0 {
		r.TransfersPerUser = float64(r.LedgerTransfers) / float64(r.Customers)
	}
	return r
}

// TableStorage is a ClickHouse table's active parts.
type TableStorage struct {
	Table        string `json:"table"`
	Rows         uint64 `json:"rows"`
	Parts        uint64 `json:"parts"`
	Bytes        uint64 `json:"bytes_on_disk"`
	Compressed   uint64 `json:"compressed"`
	Uncompressed uint64 `json:"uncompressed"`
}

func (e *env) storage(ctx context.Context, database string) ([]TableStorage, error) {
	rows, err := e.ch.Query(ctx, `SELECT table, sum(rows), count(), sum(bytes_on_disk), sum(data_compressed_bytes), sum(data_uncompressed_bytes)
FROM system.parts WHERE database = ? AND active GROUP BY table ORDER BY table`, database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableStorage
	for rows.Next() {
		var t TableStorage
		if err := rows.Scan(&t.Table, &t.Rows, &t.Parts, &t.Bytes, &t.Compressed, &t.Uncompressed); err != nil {
			return out, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// QueryStats is ClickHouse's own timing of one kind of query, from
// system.query_log.
type QueryStats struct {
	Count      uint64  `json:"count"`
	P50MS      float64 `json:"p50_ms"`
	P99MS      float64 `json:"p99_ms"`
	MaxMS      uint64  `json:"max_ms"`
	AvgRows    float64 `json:"avg_read_rows"`
	MaxRows    uint64  `json:"max_read_rows"`
	MaxMemory  uint64  `json:"max_memory_bytes"`
	TotalMS    uint64  `json:"total_ms"`
	AvgWritten float64 `json:"avg_written_rows"`
}

const queryKinds = `multiIf(
  query_kind = 'Insert' AND query ILIKE '%usage_intake%', 'insert_usage',
  query ILIKE '%usage_events FINAL ARRAY JOIN%', 'posting_final_scan',
  query ILIKE '%usage_events ARRAY JOIN%', 'print_sources',
  query ILIKE '%usage_events%', 'dedup_lookup',
  query ILIKE '%limit_usage%', 'view_limit_usage',
  query ILIKE '%usage_quantities%', 'view_usage_quantities',
  query ILIKE '%usage_history%', 'usage_history',
  'other')`

func (e *env) queryStats(ctx context.Context, w *WorldResult, database string) map[string]QueryStats {
	_ = e.ch.Exec(ctx, "SYSTEM FLUSH LOGS")
	rows, err := e.ch.Query(ctx, `SELECT `+queryKinds+` AS kind, count(), quantile(0.5)(query_duration_ms), quantile(0.99)(query_duration_ms), max(query_duration_ms),
  avg(read_rows), max(read_rows), toUInt64(greatest(max(memory_usage), 0)), sum(query_duration_ms), avg(written_rows)
FROM system.query_log WHERE type = 'QueryFinish' AND has(databases, ?) AND event_time > now() - INTERVAL 6 HOUR GROUP BY kind`, database)
	if err != nil {
		w.fail("query log: %v", err)
		return nil
	}
	defer rows.Close()
	out := map[string]QueryStats{}
	for rows.Next() {
		var kind string
		var s QueryStats
		if err := rows.Scan(&kind, &s.Count, &s.P50MS, &s.P99MS, &s.MaxMS, &s.AvgRows, &s.MaxRows, &s.MaxMemory, &s.TotalMS, &s.AvgWritten); err != nil {
			w.fail("query log: %v", err)
			return out
		}
		out[kind] = s
	}
	return out
}

// CrashResult is one crashed process kind: whether every capture it logged
// before or after the crash was stored, once.
type CrashResult struct {
	Kind      string `json:"kind"`
	Captured  int    `json:"captured"`
	Stored    int    `json:"stored_once"`
	Lost      int    `json:"lost"`
	Twice     int    `json:"stored_twice"`
	Unlogged  int    `json:"stored_unlogged"`
	OK        bool   `json:"ok"`
	Expect    string `json:"expect"`
	RawCopies int64  `json:"raw_copies"`
}

func (e *env) checkCrash(ctx context.Context, w *WorldResult, schema string, crash billing.MerchantID, _ []gateSpec) []CrashResult {
	final := map[string]int{}
	rows, err := e.ch.Query(ctx, "SELECT source_id, count() FROM `"+schema+"`.usage_events FINAL WHERE merchant_id = "+uuidLit(crash.UUID())+" GROUP BY source_id")
	if err != nil {
		w.fail("crash counts: %v", err)
		return nil
	}
	for rows.Next() {
		var id string
		var n uint64
		if rows.Scan(&id, &n) == nil {
			final[id] = int(n)
		}
	}
	_ = rows.Close()
	raw, _ := e.countEvents(ctx, schema, crash)
	logged := map[string]map[string]bool{}
	var out []CrashResult
	for _, kind := range []string{"outbox", "plain"} {
		logged[kind] = map[string]bool{}
		f, err := os.Open(e.out + "/crash-" + kind + ".captures")
		if err != nil {
			w.fail("crash log %s: %v", kind, err)
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if id, _, ok := strings.Cut(sc.Text(), " "); ok {
				logged[kind][id] = true
			}
		}
		_ = f.Close()
		r := CrashResult{Kind: kind, Captured: len(logged[kind]), RawCopies: raw}
		for id := range logged[kind] {
			switch n := final[id]; {
			case n == 0:
				r.Lost++
			case n == 1:
				r.Stored++
			default:
				r.Twice++
			}
		}
		out = append(out, r)
	}
	var stored int
	for id, n := range final {
		stored += n
		if logged["outbox"][id] || logged["plain"][id] {
			continue
		}
		for i := range out {
			if out[i].Kind == kindOfPrefix(id) {
				out[i].Unlogged++
			}
		}
	}
	for i := range out {
		r := &out[i]
		if r.Kind == "outbox" {
			r.Expect = "every capture stored once"
			r.OK = r.Lost == 0 && r.Twice == 0
		} else {
			r.Expect = "captures unsent at the crash lost, none stored twice"
			r.OK = r.Twice == 0
		}
		w.check("crash/"+r.Kind, r.OK, "%d captured, %d stored once, %d lost, %d twice, %d stored but not logged; %d raw rows for %d stored",
			r.Captured, r.Stored, r.Lost, r.Twice, r.Unlogged, r.RawCopies, stored)
	}
	return out
}

// kindOfPrefix is a crash request id's process kind: ids are c<n>-<seq>, and
// processes 0 and 2 have the outbox.
func kindOfPrefix(id string) string {
	p, _, _ := strings.Cut(id, "-")
	n, _ := strconv.Atoi(strings.TrimPrefix(p, "c"))
	if n%2 == 0 {
		return "outbox"
	}
	return "plain"
}

// checkExpiring checks each lapsed lot: it paid for exactly the usage before
// its lapse (it is drawn first, and the customer had no usage before it).
func (e *env) checkExpiring(ctx context.Context, w *WorldResult, schema string, admin map[string]*openrails.Client, seeds map[string]*seeded, grants []billing.CreditGrant) {
	if len(grants) == 0 || len(e.plan.Normal) == 0 {
		return
	}
	slug := e.plan.Normal[0].Slug
	var ok, lapsed, mismatched int
	var worst int64
	for _, g := range grants {
		got, err := admin[slug].GetCreditGrant(ctx, g.ID)
		if err != nil {
			w.fail("lot %s: %v", g.ID, err)
			continue
		}
		var before int64
		if err := e.ch.QueryRow(ctx, "SELECT sum(amount) FROM `"+schema+"`.usage_events FINAL WHERE merchant_id = "+uuidLit(seeds[slug].id.UUID())+
			" AND customer_id = "+uuidLit(g.CustomerID.UUID())+" AND occurred_at < "+timeLit(*g.ExpiresAt)).Scan(&before); err != nil {
			w.fail("usage before lapse: %v", err)
			continue
		}
		if got.ExpiredAmount > 0 || got.RemainingAmount == 0 {
			lapsed++
		}
		want := min(before, got.Amount)
		conserved := got.SpentAmount+got.ExpiredAmount+got.RemainingAmount+got.RevokedAmount+got.RefundedAmount == got.Amount
		if conserved && got.SpentAmount == want && got.RemainingAmount == 0 {
			ok++
		} else {
			mismatched++
			worst = max(worst, abs(got.SpentAmount-want))
		}
	}
	w.check("lot-expiry-charges-usage-before-lapse", ok == len(grants), "%d lots, %d lapsed, %d spent exactly the usage before the lapse, %d did not (worst by %d micros)",
		len(grants), lapsed, ok, mismatched, worst)
}

// uuidLit and timeLit are ClickHouse literals, so no bind depends on the
// driver's formatting of a type.
func uuidLit(id uuid.UUID) string { return "toUUID('" + id.String() + "')" }

func timeLit(t time.Time) string {
	return "toDateTime64('" + t.UTC().Format("2006-01-02 15:04:05.000000") + "', 6, 'UTC')"
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
