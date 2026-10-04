//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

	// One admitted request and one usage event land in this month's partitions.
	customer := billing.CustomerID(uuid.New())
	_, err := client.EnsureCustomer(ctx, customer, billing.CustomerParams{})
	require.NoError(t, err)
	_, err = client.CreateCreditGrant(ctx, customer, billing.CreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "test", SourceID: "seed"})
	require.NoError(t, err)
	request, deadline := "job-"+uuid.NewString(), now.Add(time.Hour)
	verdicts, err := client.Admit(ctx, []billing.AdmitParams{{
		RequestID: request, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypePayer,
		Currency: "USD", EstimatedAmount: 100_000, ExpiresAt: &deadline,
	}})
	require.NoError(t, err)
	require.True(t, verdicts[0].Allowed(), "%+v", verdicts[0])
	_, err = client.CaptureAdmission(ctx, request, billing.CaptureParams{Amount: 90_000, Usage: &billing.CaptureUsage{EventType: "inference"}})
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
		_, err := client.EnsureCustomer(ctx, c, billing.CustomerParams{})
		require.NoError(t, err)
		_, err = client.CreateCreditGrant(ctx, c, billing.CreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "test", SourceID: "seed"})
		require.NoError(t, err)
	}

	admit := func(c billing.CustomerID, request string, deadline time.Time) billing.AdmissionVerdict {
		t.Helper()
		verdicts, err := client.Admit(ctx, []billing.AdmitParams{{
			RequestID: request, CustomerID: c, Invoker: c.String(), InvokerType: billing.InvokerTypePayer,
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
	usage := billing.UsageEventParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "inference", Amount: 5_000, Source: "worker", SourceID: "event-1"}
	first, err := client.RecordUsage(ctx, usage)
	require.NoError(t, err)
	require.False(t, first.Replayed)
	earlier := time.Now().Add(-48 * time.Hour)
	usage.OccurredAt = &earlier
	again, err := client.RecordUsage(ctx, usage)
	require.NoError(t, err)
	require.True(t, again.Replayed)
	require.Equal(t, first.ID, again.ID)
	usage.Amount = 6_000
	_, err = client.RecordUsage(ctx, usage)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	require.Equal(t, 1, w.count(`SELECT count(*) FROM billing.usage_events WHERE merchant_id = $1 AND source_id = 'event-1'`, w.merchant))

	// An event is accepted within the ingest window and no further back or ahead.
	backdated := time.Now().Add(-retention.UsageIngestWindow + time.Hour)
	_, err = client.RecordUsage(ctx, billing.UsageEventParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "inference", Amount: 1, Source: "worker", SourceID: "backdated", OccurredAt: &backdated})
	require.NoError(t, err)
	for name, at := range map[string]time.Time{"stale": time.Now().Add(-retention.UsageIngestWindow - time.Hour), "future": time.Now().Add(time.Hour)} {
		_, err = client.RecordUsage(ctx, billing.UsageEventParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "inference", Amount: 1, Source: "worker", SourceID: name, OccurredAt: &at})
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
	_, err := client.EnsureCustomer(ctx, customer, billing.CustomerParams{})
	require.NoError(t, err)
	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "plan-" + uuid.NewString()[:8], DisplayName: "Plan"})
	require.NoError(t, err)
	var productID, psp, subscription uuid.UUID
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT id FROM billing.products WHERE merchant_id = $1 AND key = $2`), w.merchant, product.Key).Scan(&productID))
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.psps (merchant_id, rail, account_id, key) VALUES ($1, 'stripe', 'acct_retention', 'stripe') RETURNING id`), w.merchant).Scan(&psp))
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.subscriptions (merchant_id, customer_id, product_id, rail, psp_id) VALUES ($1, $2, $3, 'stripe', $4) RETURNING id`),
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
	_, err = tx.Exec(ctx, `SELECT set_config('openrails.retention', 'subscription_status_transitions', true)`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, w.q(`DELETE FROM billing.subscription_status_transitions WHERE merchant_id = $1 AND to_status = 'past_due'`), w.merchant)
	requireRefused(t, err)
	require.NoError(t, tx.Rollback(ctx))
	// Naming another table opens nothing here.
	tx, err = w.pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT set_config('openrails.retention', 'maintenance_runs', true)`)
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
		mode, actor := "advisory", ""
		if kind != "reconciliation" {
			mode, actor = "", "operator"
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
	_, err = client.CreateCreditGrant(ctx, customer, billing.CreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "test", SourceID: "seed"})
	require.NoError(t, err)
	_, err = client.RecordUsage(ctx, billing.UsageEventParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "inference", Amount: 5_000, Source: "worker", SourceID: "event-1"})
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
