//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/retention"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// retentionWorld is a fixture with one merchant and the cleanup job over its
// schema, on a clock the test moves.
type retentionWorld struct {
	*fixture
	t        *testing.T
	db       *db.DB
	clock    *clockwork.FakeClock
	merchant uuid.UUID
}

func newRetentionWorld(t *testing.T) (*retentionWorld, *openrails.Client) {
	t.Helper()
	f := newFixture(t)
	slug := "retention-" + uuid.NewString()[:8]
	client := f.runtime(t, slug)
	database, err := db.NewWithPGXPool(f.pool, f.schema)
	require.NoError(t, err)
	w := &retentionWorld{fixture: f, t: t, db: database, clock: clockwork.NewFakeClockAt(time.Now().UTC())}
	require.NoError(t, f.pool.QueryRow(t.Context(), w.q(`SELECT id FROM billing.merchants WHERE slug = $1`), slug).Scan(&w.merchant))
	return w, client
}

// q moves a statement authored in billing to the fixture's schema.
func (w *retentionWorld) q(sql string) string {
	return strings.ReplaceAll(sql, "billing.", pgx.Identifier{w.schema}.Sanitize()+".")
}

func (w *retentionWorld) exec(sql string, args ...any) {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(sql), args...)
	require.NoError(w.t, err)
}

func (w *retentionWorld) count(sql string, args ...any) int {
	w.t.Helper()
	var n int
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(sql), args...).Scan(&n))
	return n
}

// sweep runs one cleanup pass; budget caps one merchant's rows for the pass.
func (w *retentionWorld) sweep(budget int) riverjobs.CleanupResult {
	w.t.Helper()
	result, err := riverjobs.CleanupExpiredDataWorker{
		DB: w.db, Clock: w.clock, Config: riverjobs.DefaultCleanupConfig(), RowBudget: budget,
	}.Sweep(w.t.Context())
	require.NoError(w.t, err)
	return result
}

func (w *retentionWorld) partitions(table string) map[string][2]time.Time {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT partition::text, range_from, range_to FROM billing.month_partitions($1)`), table)
	require.NoError(w.t, err)
	defer rows.Close()
	out := map[string][2]time.Time{}
	for rows.Next() {
		var name string
		var from, to time.Time
		require.NoError(w.t, rows.Scan(&name, &from, &to))
		out[name] = [2]time.Time{from.UTC(), to.UTC()}
	}
	require.NoError(w.t, rows.Err())
	return out
}

func partitionName(table string, month time.Time) string {
	return fmt.Sprintf("%s_y%04dm%02d", table, month.UTC().Year(), int(month.UTC().Month()))
}

func requireRefused(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "23514", pgErr.Code, pgErr.Message)
}

// The calendar alone creates and drops partitions: months are made ahead of
// their first row and dropped whole once past retention, and a row leaves no
// other way.
func TestPartitionsAreCreatedAheadAndDroppedByTheCalendar(t *testing.T) {
	w, client := newRetentionWorld(t)
	ctx := t.Context()
	now := w.clock.Now()
	thisMonth := retention.MonthStart(now)

	// Migration made every month a row can be written into, and two ahead.
	usage, admissions := w.partitions("usage_events"), w.partitions("admission_operations")
	for m := retention.MonthStart(now.Add(-retention.UsageIngestWindow)); !m.After(thisMonth.AddDate(0, retention.PartitionsAheadMonths, 0)); m = m.AddDate(0, 1, 0) {
		require.Contains(t, usage, partitionName("usage_events", m))
	}
	for m := thisMonth; !m.After(thisMonth.AddDate(0, retention.PartitionsAheadMonths, 0)); m = m.AddDate(0, 1, 0) {
		require.Contains(t, admissions, partitionName("admission_operations", m))
		require.Equal(t, [2]time.Time{m, m.AddDate(0, 1, 0)}, admissions[partitionName("admission_operations", m)])
	}

	// A restore may bring back any retained month: it makes them first.
	require.NotContains(t, usage, partitionName("usage_events", retention.UsageDropBefore(now)))
	_, err := retention.EnsureRetainedPartitions(t.Context(), w.db.GenDirectory(), now)
	require.NoError(t, err)
	require.Contains(t, w.partitions("usage_events"), partitionName("usage_events", retention.UsageDropBefore(now)))
	require.Zero(t, w.sweep(0).PartitionsDropped, "a retained month is not dropped")

	// One admitted request and one usage event land in this month's partitions.
	customer := billing.CustomerID(uuid.New())
	_, err = client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}})
	require.NoError(t, err)
	_, err = client.CreateCreditGrant(ctx, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "test", SourceID: "seed"})
	require.NoError(t, err)
	request, deadline := "job-"+uuid.NewString(), now.Add(time.Hour)
	verdicts, err := client.Admit(ctx, []billing.AdmitParams{{
		RequestID: request, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer,
		Currency: "USD", EstimatedAmount: 100_000, ExpiresAt: &deadline,
	}})
	require.NoError(t, err)
	require.True(t, verdicts[0].Allowed(), "%+v", verdicts[0])
	_, err = client.CaptureAdmission(ctx, request, billing.CaptureAdmissionParams{Amount: 90_000, Usage: &billing.CaptureUsage{EventType: "inference"}})
	require.NoError(t, err)
	require.Equal(t, 1, w.count(`SELECT count(*) FROM billing.`+partitionName("admission_operations", thisMonth)))
	require.Equal(t, 1, w.count(`SELECT count(*) FROM billing.`+partitionName("usage_events", thisMonth)))

	// Nothing deletes these rows one by one.
	_, err = w.pool.Exec(ctx, w.q(`DELETE FROM billing.admission_operations WHERE merchant_id = $1`), w.merchant)
	requireRefused(t, err)

	// A pass today changes nothing: every partition is current.
	today := w.sweep(0)
	require.Zero(t, today.PartitionsCreated)
	require.Zero(t, today.PartitionsDropped)

	// Three months on: the months ahead exist before any row needs them, the
	// admission is past its window plus 30 days and went with its month, and
	// the usage it captured is kept.
	w.clock.Advance(92 * 24 * time.Hour)
	later := retention.MonthStart(w.clock.Now())
	result := w.sweep(0)
	require.Positive(t, result.PartitionsCreated)
	require.Positive(t, result.PartitionsDropped)
	usage, admissions = w.partitions("usage_events"), w.partitions("admission_operations")
	require.Contains(t, usage, partitionName("usage_events", later.AddDate(0, retention.PartitionsAheadMonths, 0)))
	require.Contains(t, admissions, partitionName("admission_operations", later.AddDate(0, retention.PartitionsAheadMonths, 0)))
	require.NotContains(t, admissions, partitionName("admission_operations", thisMonth))
	for name, bounds := range admissions {
		require.True(t, bounds[1].After(retention.AdmissionsDropBefore(w.clock.Now())), "%s is past retention and still there", name)
	}
	require.Zero(t, w.count(`SELECT count(*) FROM billing.admission_operations WHERE merchant_id = $1`, w.merchant))
	require.Contains(t, usage, partitionName("usage_events", thisMonth))
	require.Equal(t, 1, w.count(`SELECT count(*) FROM billing.usage_events WHERE merchant_id = $1`, w.merchant))

	// The usage month is invoiced by the end of the next month and kept 24
	// months past that: on the last day of the 25th month it is still there,
	// a day later it is gone.
	dropAt := thisMonth.AddDate(0, 1+retention.UsageInvoicingLagMonths+retention.UsageInvoicedMonths, 0)
	w.clock.Advance(dropAt.Add(-time.Hour).Sub(w.clock.Now()))
	w.sweep(0)
	require.Contains(t, w.partitions("usage_events"), partitionName("usage_events", thisMonth))
	w.clock.Advance(2 * time.Hour)
	w.sweep(0)
	require.NotContains(t, w.partitions("usage_events"), partitionName("usage_events", thisMonth))
	require.Zero(t, w.count(`SELECT count(*) FROM billing.usage_events WHERE merchant_id = $1`, w.merchant))

	// The money those rows described never moved.
	balance, err := client.GetBalance(ctx, customer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 910_000, balance.BalanceAmount)
}

// planRelations runs EXPLAIN and returns the relations the plan scans.
func (w *retentionWorld) planRelations(sql string, args ...any) []string {
	w.t.Helper()
	var raw []byte
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), "EXPLAIN (FORMAT JSON) "+w.q(sql), args...).Scan(&raw))
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	require.NoError(w.t, json.Unmarshal(raw, &plans))
	var out []string
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if name, ok := node["Relation Name"].(string); ok {
			out = append(out, name)
		}
		children, _ := node["Plans"].([]any)
		for _, c := range children {
			walk(c.(map[string]any))
		}
	}
	walk(plans[0].Plan)
	return out
}

// A read that names a time range touches only the partitions of that range.
func TestPartitionedReadsPruneToTheirMonths(t *testing.T) {
	w, _ := newRetentionWorld(t)
	now := w.clock.Now()
	thisMonth := retention.MonthStart(now)
	customer := uuid.New()

	// A usage read over this month: the shape of every usage report.
	require.Equal(t, []string{partitionName("usage_events", thisMonth)}, w.planRelations(
		`SELECT event_type, sum(amount) FROM billing.usage_events
		 WHERE merchant_id = $1 AND customer_id = $2 AND currency = 'USD' AND occurred_at >= $3 AND occurred_at < $4
		 GROUP BY event_type`, w.merchant, customer, thisMonth, thisMonth.AddDate(0, 1, 0)))

	// The idempotency lookup reads the ingest window, never the months ahead
	// or the retained history behind it.
	from, to := now.Add(-retention.UsageIngestWindow), now.Add(retention.UsageClockSkew)
	scanned := w.planRelations(
		`SELECT id FROM billing.usage_events
		 WHERE merchant_id = $1 AND customer_id = $2 AND currency = 'USD' AND event_type = 'e' AND source = 's' AND source_id = 'i'
		   AND occurred_at >= $3 AND occurred_at <= $4`, w.merchant, customer, from, to)
	require.NotEmpty(t, scanned)
	for _, name := range scanned {
		bounds := w.partitions("usage_events")[name]
		require.True(t, bounds[1].After(from) && !bounds[0].After(to), "%s is outside the ingest window", name)
	}
	require.NotContains(t, scanned, partitionName("usage_events", thisMonth.AddDate(0, 1, 0)))

	// A spend window reads the months it spans; an admission addressed by its
	// own admitted_at reads one.
	require.Equal(t, []string{partitionName("admission_operations", thisMonth)}, w.planRelations(
		`SELECT sum(estimated_amount) FROM billing.admission_operations
		 WHERE merchant_id = $1 AND customer_id = $2 AND currency = 'USD' AND state <> 'released'
		   AND admitted_at >= $3 AND admitted_at < $4`, w.merchant, customer, now.Add(-time.Hour), now.Add(time.Hour)))
	require.Equal(t, []string{partitionName("admission_operations", thisMonth)}, w.planRelations(
		`SELECT state FROM billing.admission_operations WHERE merchant_id = $1 AND request_id = 'r' AND admitted_at = $2`, w.merchant, now))
}

// The partition key is part of every unique key, so the application keeps the
// two identities the keys used to: a request id is admitted once, and a usage
// coordinate is recorded once within the ingest window.
func TestPartitionedIdentitiesAndWriteBounds(t *testing.T) {
	w, client := newRetentionWorld(t)
	ctx := t.Context()
	customer, other := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
	for _, c := range []billing.CustomerID{customer, other} {
		_, err := client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: c}})
		require.NoError(t, err)
		_, err = client.CreateCreditGrant(ctx, c, billing.CreateCreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "test", SourceID: "seed"})
		require.NoError(t, err)
	}

	admit := func(c billing.CustomerID, request string, deadline time.Time) billing.AdmissionVerdict {
		t.Helper()
		verdicts, err := client.Admit(ctx, []billing.AdmitParams{{
			RequestID: request, CustomerID: c, Invoker: c.String(), InvokerType: billing.InvokerTypeCustomer,
			Currency: "USD", EstimatedAmount: 1_000, ExpiresAt: &deadline,
		}})
		require.NoError(t, err)
		return verdicts[0]
	}
	request, deadline := "job-"+uuid.NewString(), time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	require.True(t, admit(customer, request, deadline).Allowed())
	replay := admit(customer, request, deadline)
	require.True(t, replay.Allowed())
	require.True(t, replay.Admission.Replayed)
	// The same id for another customer is a reuse, not a second admission.
	reused := admit(other, request, deadline)
	require.NotNil(t, reused.Error, "%+v", reused)
	require.Equal(t, billing.CodeIdempotencyKeyReused, reused.Error.Code)
	require.Equal(t, 1, w.count(`SELECT count(*) FROM billing.admission_operations WHERE merchant_id = $1 AND request_id = $2`, w.merchant, request))

	// A hold lives at most 30 days past its admission, at admit and on extend.
	tooFar := admit(customer, "job-"+uuid.NewString(), time.Now().Add(retention.AdmissionMaxHold+time.Hour))
	require.NotNil(t, tooFar.Error, "%+v", tooFar)
	require.Equal(t, billing.CodeInvalidParam, tooFar.Error.Code)
	require.Equal(t, "expires_at", *tooFar.Error.Param)
	_, err := client.ExtendAdmission(ctx, request, billing.ExtendAdmissionParams{ExpiresAt: time.Now().Add(retention.AdmissionMaxHold + time.Hour)})
	require.ErrorIs(t, err, billing.ErrInvalid)
	_, err = client.ExtendAdmission(ctx, request, billing.ExtendAdmissionParams{ExpiresAt: time.Now().Add(retention.AdmissionMaxHold - time.Hour)})
	require.NoError(t, err)

	// A usage coordinate is recorded once, whatever time each attempt reports.
	usage := billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "inference", Amount: 5_000, Source: "worker", SourceID: "event-1"}
	first, err := recordUsage(ctx, client, usage)
	require.NoError(t, err)
	require.False(t, first.Replayed)
	earlier := time.Now().Add(-48 * time.Hour)
	usage.OccurredAt = &earlier
	again, err := recordUsage(ctx, client, usage)
	require.NoError(t, err)
	require.True(t, again.Replayed)
	require.Equal(t, first.ID, again.ID)
	usage.Amount = 6_000
	_, err = recordUsage(ctx, client, usage)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	require.Equal(t, 1, w.count(`SELECT count(*) FROM billing.usage_events WHERE merchant_id = $1 AND source_id = 'event-1'`, w.merchant))

	// An event is accepted within the ingest window and no further back or ahead.
	backdated := time.Now().Add(-retention.UsageIngestWindow + time.Hour)
	_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "inference", Amount: 1, Source: "worker", SourceID: "backdated", OccurredAt: &backdated})
	require.NoError(t, err)
	for name, at := range map[string]time.Time{"stale": time.Now().Add(-retention.UsageIngestWindow - time.Hour), "future": time.Now().Add(time.Hour)} {
		_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "inference", Amount: 1, Source: "worker", SourceID: name, OccurredAt: &at})
		require.ErrorIs(t, err, billing.ErrInvalid, name)
	}
}

// Row retention deletes what is past its period and nothing else, in bounded
// passes; the tables that refuse ad-hoc deletes still refuse them; permanent
// tables are never touched.
func TestRetentionDeletesOnlyRowsPastTheirPeriod(t *testing.T) {
	w, client := newRetentionWorld(t)
	ctx := t.Context()
	day := 24 * time.Hour

	customer := billing.CustomerID(uuid.New())
	_, err := client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}})
	require.NoError(t, err)
	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "plan-" + uuid.NewString()[:8], DisplayName: "Plan"})
	require.NoError(t, err)
	var productID, psp, subscription uuid.UUID
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT id FROM billing.products WHERE merchant_id = $1 AND key = $2`), w.merchant, product.Key).Scan(&productID))
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.psps (merchant_id, rail, account_id, key, environment) VALUES ($1, 'stripe', 'acct_retention', 'stripe', 'live') RETURNING id`), w.merchant).Scan(&psp))
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.subscriptions (merchant_id, customer_id, product_id, rail, psp_id, status, collection_policy, started_at) VALUES ($1, $2, $3, 'stripe', $4, 'pending', 'provider', now()) RETURNING id`),
		w.merchant, customer.UUID(), productID, psp).Scan(&subscription))

	// Subscription history: 400 changes past 25 months, 3 just inside it, and
	// the one the insert above recorded.
	old, young := retention.SubscriptionTransitions+day, retention.SubscriptionTransitions-day
	w.exec(`INSERT INTO billing.subscription_status_transitions (merchant_id, subscription_id, from_status, to_status, occurred_at)
	        SELECT $1, $2, 'pending', 'active', now() - $3::interval - (n || ' minutes')::interval FROM generate_series(1, 400) n`, w.merchant, subscription, old)
	w.exec(`INSERT INTO billing.subscription_status_transitions (merchant_id, subscription_id, from_status, to_status, occurred_at)
	        SELECT $1, $2, 'active', 'past_due', now() - $3::interval + (n || ' minutes')::interval FROM generate_series(1, 3) n`, w.merchant, subscription, young)
	transitions := func() int {
		return w.count(`SELECT count(*) FROM billing.subscription_status_transitions WHERE merchant_id = $1`, w.merchant)
	}
	require.Equal(t, 404, transitions())

	// An ad-hoc delete is refused, old row or not; so is one that claims to be
	// the sweep but reaches a row still inside its period.
	_, err = w.pool.Exec(ctx, w.q(`DELETE FROM billing.subscription_status_transitions WHERE merchant_id = $1`), w.merchant)
	requireRefused(t, err)
	tx, err := w.pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT set_config('openrails.retention_table', 'subscription_status_transitions', true)`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, w.q(`DELETE FROM billing.subscription_status_transitions WHERE merchant_id = $1 AND to_status = 'past_due'`), w.merchant)
	requireRefused(t, err)
	require.NoError(t, tx.Rollback(ctx))
	// Naming another table opens nothing here.
	tx, err = w.pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT set_config('openrails.retention_table', 'maintenance_runs', true)`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, w.q(`DELETE FROM billing.subscription_status_transitions WHERE merchant_id = $1 AND to_status = 'active'`), w.merchant)
	requireRefused(t, err)
	require.NoError(t, tx.Rollback(ctx))
	require.Equal(t, 404, transitions())

	// Checkout attempts: one expired 91 days ago, one expired 89 days ago, and
	// one that failed long ago and is kept.
	attempt := func(status string, expiredAgo time.Duration) uuid.UUID {
		var id uuid.UUID
		require.NoError(t, w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.checkout_attempts (merchant_id, customer_id, psp_id, mode, rail, status, expires_at)
			VALUES ($1, $2, $3, 'payment_method', 'stripe', $4, now() - $5::interval) RETURNING id`), w.merchant, customer.UUID(), psp, status, expiredAgo).Scan(&id))
		return id
	}
	staleAttempt := attempt("expired", retention.ExpiredCheckoutAttempts+day)
	freshAttempt := attempt("expired", retention.ExpiredCheckoutAttempts-day)
	failedAttempt := attempt("failed", 400*day)

	// Reconciliation: a run and its resolved finding past 12 months, a run a
	// still-open finding names, a resolved finding that is still being seen,
	// and a destructive run, which is permanent.
	run := func(kind string, startedAgo time.Duration) uuid.UUID {
		var id uuid.UUID
		var mode, actor any = "advisory", nil
		if kind != "reconciliation" {
			mode, actor = nil, "operator"
		}
		require.NoError(t, w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.maintenance_runs (merchant_id, kind, mode, actor, status, started_at, finished_at)
			VALUES ($1, $2, $3, $4, 'completed', now() - $5::interval, now() - $5::interval) RETURNING id`), w.merchant, kind, mode, actor, startedAgo).Scan(&id))
		return id
	}
	finding := func(subject string, seenRun uuid.UUID, resolvedAgo, seenAgo time.Duration) {
		status, resolution := "requires_review", any(nil)
		var resolvedAt any
		if resolvedAgo > 0 {
			status, resolution = "fixed", "admin_fixed"
			resolvedAt = time.Now().Add(-resolvedAgo)
		}
		w.exec(`INSERT INTO billing.reconciliation_findings (merchant_id, finding_type, subject_key, severity, status, resolution, resolved_at, last_seen_at, first_seen_run, last_seen_run)
			VALUES ($1, 'life.retention_test', $2, 'low', $3, $4, $5, now() - $6::interval, $7, $7)`, w.merchant, subject, status, resolution, resolvedAt, seenAgo, seenRun)
	}
	period := retention.ReconciliationRuns
	staleRun, pinnedRun, seenRun, freshRun := run("reconciliation", period+2*day), run("reconciliation", period+2*day), run("reconciliation", period+2*day), run("reconciliation", period-day)
	destructiveRun := run("prune", 3*period)
	finding("resolved-long-ago", staleRun, period+day, period+day)
	finding("still-open", pinnedRun, 0, period+day)
	finding("resolved-but-still-seen", seenRun, period+day, day)
	_, err = w.pool.Exec(ctx, w.q(`DELETE FROM billing.maintenance_runs WHERE merchant_id = $1 AND id = $2`), w.merchant, freshRun)
	requireRefused(t, err)

	// Money that must survive everything below.
	_, err = client.CreateCreditGrant(ctx, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "test", SourceID: "seed"})
	require.NoError(t, err)
	_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "inference", Amount: 5_000, Source: "worker", SourceID: "event-1"})
	require.NoError(t, err)
	permanent := func() map[string]int {
		out := map[string]int{}
		for _, table := range []string{"ledger_accounts", "ledger_transfers", "grants", "payments", "invoices", "invoice_items", "invoice_payments"} {
			out[table] = w.count(`SELECT count(*) FROM billing.`+table+` WHERE merchant_id = $1`, w.merchant)
		}
		return out
	}
	before := permanent()
	require.Positive(t, before["ledger_transfers"])
	require.Positive(t, before["grants"])

	// A pass is bounded: with a budget of 150 rows it deletes 150, oldest
	// first, and says the merchant has more.
	first := w.sweep(150)
	require.EqualValues(t, 150, first.SubscriptionTransitions)
	require.Equal(t, 1, first.MerchantsBudgetCapped)
	require.Equal(t, 254, transitions())
	require.Equal(t, 250, w.count(`SELECT count(*) FROM billing.subscription_status_transitions WHERE merchant_id = $1 AND occurred_at < now() - $2::interval`, w.merchant, retention.SubscriptionTransitions))

	// The following passes drain the backlog and then find nothing left.
	var total riverjobs.CleanupResult
	for range 4 {
		r := w.sweep(150)
		total.SubscriptionTransitions += r.SubscriptionTransitions
		total.CheckoutAttempts += r.CheckoutAttempts
		total.ReconciliationFindings += r.ReconciliationFindings
		total.ReconciliationRuns += r.ReconciliationRuns
	}
	require.EqualValues(t, 250, total.SubscriptionTransitions)
	require.EqualValues(t, 1, total.CheckoutAttempts)
	require.EqualValues(t, 1, total.ReconciliationFindings)
	require.EqualValues(t, 1, total.ReconciliationRuns)
	idle := w.sweep(150)
	require.Zero(t, idle.SubscriptionTransitions+idle.CheckoutAttempts+idle.ReconciliationFindings+idle.ReconciliationRuns)
	require.Zero(t, idle.MerchantsBudgetCapped)

	// What is left is exactly what is still inside its period, or permanent.
	require.Equal(t, 4, transitions())
	require.Zero(t, w.count(`SELECT count(*) FROM billing.subscription_status_transitions WHERE merchant_id = $1 AND occurred_at < now() - $2::interval`, w.merchant, retention.SubscriptionTransitions))
	exists := func(table string, id uuid.UUID) bool {
		return w.count(`SELECT count(*) FROM billing.`+table+` WHERE merchant_id = $1 AND id = $2`, w.merchant, id) == 1
	}
	require.False(t, exists("checkout_attempts", staleAttempt))
	require.True(t, exists("checkout_attempts", freshAttempt))
	require.True(t, exists("checkout_attempts", failedAttempt))
	require.False(t, exists("maintenance_runs", staleRun))
	require.True(t, exists("maintenance_runs", pinnedRun), "a run an open finding names is kept")
	require.True(t, exists("maintenance_runs", seenRun), "a run a kept finding names is kept")
	require.True(t, exists("maintenance_runs", freshRun))
	require.True(t, exists("maintenance_runs", destructiveRun), "destructive runs are permanent")
	require.Equal(t, 2, w.count(`SELECT count(*) FROM billing.reconciliation_findings WHERE merchant_id = $1 AND subject_key IN ('still-open', 'resolved-but-still-seen')`, w.merchant))
	require.Zero(t, w.count(`SELECT count(*) FROM billing.reconciliation_findings WHERE merchant_id = $1 AND subject_key = 'resolved-long-ago'`, w.merchant))

	// Permanent tables: unchanged by every pass, and a clock thirty years on
	// changes nothing either. Their own guards refuse a delete outright.
	require.Equal(t, before, permanent())
	w.clock.Advance(30 * 365 * day)
	w.sweep(0)
	require.Equal(t, before, permanent())
	for _, table := range []string{"ledger_transfers", "grants"} {
		_, err = w.pool.Exec(context.WithoutCancel(ctx), w.q(`DELETE FROM billing.`+table+` WHERE merchant_id = $1`), w.merchant)
		requireRefused(t, err)
	}
	balance, err := client.GetBalance(ctx, customer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 995_000, balance.BalanceAmount)
}

// Provider writes and cost evidence. A finished intent that only instructed a
// provider ages out; one that is the record of money, a membership or an
// erasure never does. A cost observation outlives its operation by 90 days and
// is never deleted while the operation is open.
func TestProviderWriteAndCostObservationRetention(t *testing.T) {
	w, client := newRetentionWorld(t)
	ctx := t.Context()
	day := 24 * time.Hour

	customer := billing.CustomerID(uuid.New())
	_, err := client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}})
	require.NoError(t, err)
	_, err = client.CreateCreditGrant(ctx, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "test", SourceID: "seed"})
	require.NoError(t, err)
	var psp, account uuid.UUID
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.psps (merchant_id, rail, account_id, key, environment) VALUES ($1, 'stripe', 'acct_retention', 'stripe', 'live') RETURNING id`), w.merchant).Scan(&psp))
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT id FROM billing.ledger_accounts WHERE merchant_id = $1 AND customer_id = $2 AND account_type = 'customer_balance' AND currency = 'USD'`),
		w.merchant, customer.UUID()).Scan(&account))

	// Intents, each last changed `ago` ago.
	intent := func(kind, status string, ago time.Duration, payload any) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		require.NoError(t, w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.provider_intents
			(merchant_id, rail, psp_id, intent_type, idempotency_key, origin, status, payload, executed_at, next_attempt_at, created_at, updated_at)
			VALUES ($1, 'stripe', $2, $3, $4, 'system', $5, $6::jsonb,
			        CASE WHEN $5 = 'succeeded' THEN now() - $7::interval END, now() - $7::interval, now() - $7::interval, now() - $7::interval)
			RETURNING id`), w.merchant, psp, kind, kind+":"+uuid.NewString(), status, string(body), ago).Scan(&id))
		return id
	}
	old, young := retention.ProviderWrites+day, retention.ProviderWrites-day
	attemptID := uuid.New()
	oldCancel := intent("stripe_cancel_subscription", "succeeded", old, map[string]any{})
	oldArchive := intent("stripe_archive_price", "failed_terminal", old, map[string]any{})
	youngCancel := intent("stripe_cancel_subscription", "succeeded", young, map[string]any{})
	pendingCancel := intent("stripe_cancel_subscription", "pending", old, map[string]any{})
	// Records of money, a membership and an erasure, just as old.
	collection := intent("subscription_collection", "succeeded", old, map[string]any{"previous_period_end": "2020-01-01T00:00:00Z", "attempt": 1})
	refusedEnrollment := intent("initial_membership", "failed_terminal", old, map[string]any{"checkout_attempt_id": attemptID})
	refund := intent("stripe_refund", "succeeded", old, map[string]any{})
	erasure := intent("nmi_vault_delete", "succeeded", old, map[string]any{"rail_customer_ref": "vault-1"})

	// An attempt that expired long ago, but that an intent names: it reached a
	// provider, and its enrollment's outcome is read against it.
	w.exec(`INSERT INTO billing.checkout_attempts (id, merchant_id, customer_id, psp_id, mode, rail, status, expires_at)
		VALUES ($1, $2, $3, $4, 'payment_method', 'stripe', 'expired', now() - $5::interval)`, attemptID, w.merchant, customer.UUID(), psp, retention.ExpiredCheckoutAttempts+30*day)

	// The mutation log: two old entries (one names an intent that is deleted,
	// one an intent that is kept) and a recent one naming the deleted intent.
	logEntry := func(of uuid.UUID, ago time.Duration) uuid.UUID {
		var id uuid.UUID
		require.NoError(t, w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.provider_mutation_logs (merchant_id, rail, psp_id, provider_intent_id, phase, attempt, created_at)
			VALUES ($1, 'stripe', $2, $3, 'succeeded', 0, now() - $4::interval) RETURNING id`), w.merchant, psp, of, ago).Scan(&id))
		return id
	}
	oldLog, oldLogOfKept, youngLog := logEntry(oldCancel, old), logEntry(collection, old), logEntry(oldCancel, young)

	// Cost observations of three operations: released 100 days ago, released
	// 10 days ago, and still open after a year.
	operation := func(id string, releasedAgo time.Duration) {
		state, reference := "open", any(nil)
		var releasedAt any
		if releasedAgo > 0 {
			state, reference, releasedAt = "released", "host-release", time.Now().Add(-releasedAgo)
		}
		w.exec(`INSERT INTO billing.operation_authorizations
			(operation_id, merchant_id, customer_id, record_owner, ledger_account_id, currency, amount, claim_reference,
			 authorization_body_bytes, authorization_body_digest, state, terminal_reference, released_at, created_at)
			VALUES ($1, $2, $3, 'host', $4, 'USD', 1000, 'claim-' || $1, 'body'::bytea, sha256('body'::bytea), $5, $6, $7, now() - interval '400 days')`,
			id, w.merchant, customer.UUID(), account, state, reference, releasedAt)
		w.exec(`INSERT INTO billing.cost_qualifications
			(merchant_id, operation_id, provider, provider_resource_id, provider_lifetime_starts_at, provider_lifetime_ends_at, provider_absent_at,
			 provider_absence_reference, billing_stop_reference, windows_closed_at, windows_closed_reference,
			 lifecycle_evidence_bytes, lifecycle_evidence_digest, quiescence_seconds)
			VALUES ($1, $2, 'cloud', 'resource-' || $2, now() - interval '400 days', now() - interval '399 days', now() - interval '399 days',
			        'absent', 'stopped', now() - interval '399 days', 'closed', 'evidence'::bytea, sha256('evidence'::bytea), 60)`, w.merchant, id)
		for n := range 3 {
			w.exec(`INSERT INTO billing.cost_observations
				(merchant_id, operation_id, observation_id, normalized_query, query_starts_at, query_ends_at, raw_body_available, raw_body_bytes, raw_body_digest,
				 covers_lifetime, has_negative_record, refusal_kind, qualification_reason, observed_at)
				VALUES ($1, $2, $3, 'q', now() - interval '400 days', now() - interval '399 days', false, ''::bytea, sha256(''::bytea),
				        false, false, 'response_too_large', 'provider_evidence_refused', now() - interval '398 days')`, w.merchant, id, fmt.Sprintf("obs-%d", n))
		}
	}
	operation("closed-long-ago", retention.CostObservations+10*day)
	operation("closed-recently", 10*day)
	operation("still-open", 0)
	observations := func(op string) int {
		return w.count(`SELECT count(*) FROM billing.cost_observations WHERE merchant_id = $1 AND operation_id = $2`, w.merchant, op)
	}

	// A log entry's content is immutable, and so is which intent it names: the
	// one change allowed is the link going NULL when that intent is deleted.
	_, err = w.pool.Exec(ctx, w.q(`UPDATE billing.provider_mutation_logs SET reason = 'edited' WHERE merchant_id = $1 AND id = $2`), w.merchant, youngLog)
	requireRefused(t, err)
	_, err = w.pool.Exec(ctx, w.q(`UPDATE billing.provider_mutation_logs SET provider_intent_id = $3 WHERE merchant_id = $1 AND id = $2`), w.merchant, youngLog, collection)
	requireRefused(t, err)

	// No delete reaches a cost observation outside the sweep, however old.
	_, err = w.pool.Exec(ctx, w.q(`DELETE FROM billing.cost_observations WHERE merchant_id = $1`), w.merchant)
	requireRefused(t, err)

	result := w.sweep(0)
	require.EqualValues(t, 2, result.ProviderIntents)
	require.EqualValues(t, 2, result.ProviderMutationLogs)
	require.EqualValues(t, 3, result.CostObservations)
	require.Zero(t, result.CheckoutAttempts)

	exists := func(table string, id uuid.UUID) bool {
		return w.count(`SELECT count(*) FROM billing.`+table+` WHERE merchant_id = $1 AND id = $2`, w.merchant, id) == 1
	}
	for name, id := range map[string]uuid.UUID{"old cancel": oldCancel, "old archive": oldArchive} {
		require.False(t, exists("provider_intents", id), "%s is a finished instruction past 25 months", name)
	}
	for name, id := range map[string]uuid.UUID{
		"recent cancel": youngCancel, "pending cancel": pendingCancel, "collection": collection,
		"refused enrollment": refusedEnrollment, "refund": refund, "erasure": erasure,
	} {
		require.True(t, exists("provider_intents", id), "%s must be kept", name)
	}
	require.True(t, exists("checkout_attempts", attemptID), "an attempt an intent names is kept")
	require.False(t, exists("provider_mutation_logs", oldLog))
	require.False(t, exists("provider_mutation_logs", oldLogOfKept))
	// The recent entry outlives the intent it described; only its link is gone.
	require.Equal(t, 1, w.count(`SELECT count(*) FROM billing.provider_mutation_logs WHERE merchant_id = $1 AND id = $2 AND provider_intent_id IS NULL`, w.merchant, youngLog))

	require.Zero(t, observations("closed-long-ago"))
	require.Equal(t, 3, observations("closed-recently"))
	require.Equal(t, 3, observations("still-open"), "an open operation keeps its evidence however old")
	// The operations themselves and their qualifications are permanent.
	require.Equal(t, 3, w.count(`SELECT count(*) FROM billing.operation_authorizations WHERE merchant_id = $1`, w.merchant))
	require.Equal(t, 3, w.count(`SELECT count(*) FROM billing.cost_qualifications WHERE merchant_id = $1`, w.merchant))

	// A second pass finds nothing: the open operation's old observations do
	// not keep the merchant on the due list.
	again := w.sweep(0)
	require.Zero(t, again.ProviderIntents+again.ProviderMutationLogs+again.CostObservations)
}

// Writers crossing into a new month create its partitions at the same moment,
// each through its own pool, as db.EnsurePartitions does on a first write.
// Every one of them finds the month ready.
func TestWritersRaceIntoANewMonth(t *testing.T) {
	f := newFixture(t)
	writers := make([]*db.DB, 8)
	for i := range writers {
		pool, err := pgxpool.New(t.Context(), f.dsn(t))
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		writers[i], err = db.NewWithPGXPool(pool, f.schema)
		require.NoError(t, err)
	}
	for ahead := 6; ahead < 12; ahead++ {
		month := retention.MonthStart(time.Now()).AddDate(0, ahead, 0)
		start := make(chan struct{})
		errs := make([]error, len(writers))
		var wg sync.WaitGroup
		for i, writer := range writers {
			wg.Go(func() {
				<-start
				_, errs[i] = retention.EnsurePartitions(context.Background(), writer.GenDirectory(), month)
			})
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			require.NoError(t, err, "writer %d entering %s", i, month.Format("2006-01"))
		}
		for _, table := range []string{"usage_events", "admission_operations"} {
			var n int
			require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{f.schema}.Sanitize()+".month_partitions($1) WHERE partition = $2",
				table, partitionName(table, month)).Scan(&n))
			require.Equal(t, 1, n, "%s has %s", table, month.Format("2006-01"))
		}
	}
}
