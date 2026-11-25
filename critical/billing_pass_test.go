//go:build integration

package critical_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// billingPass is the hourly pass as River queues it: one attempt.
type billingPass struct{}

func (billingPass) Kind() string { return "openrails.billing_pass" }

func (billingPass) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: openrails.QueueBilling, MaxAttempts: 1}
}

// nmiHistoryJob is the hourly NMI history job as River queues it: one
// attempt, on its own queue.
type nmiHistoryJob struct{}

func (nmiHistoryJob) Kind() string { return "openrails.nmi_history" }

func (nmiHistoryJob) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "nmi_history", MaxAttempts: 1}
}

// quietFleet is a started fleet with no periodic pass: each pass is the
// test's, at the hour it says.
func quietFleet(t *testing.T, w *world) *river.Client[pgx.Tx] {
	t.Helper()
	fleet := hostFleet(t, w, 2)
	fleet.PeriodicJobs().Clear()
	require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
	require.NoError(t, fleet.Start(t.Context()))
	return fleet
}

// runPass runs one billing pass and reports whether it failed.
func runPass(t *testing.T, fleet *river.Client[pgx.Tx]) bool {
	t.Helper()
	failed := runOnce(t, fleet, billingPass{})
	// River runs accepted operations on wall time; business time is the test's.
	jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
	require.NoError(t, err)
	for _, op := range jobs.Jobs {
		if jobWaits(op) {
			_, err = fleet.JobRetry(t.Context(), op.ID)
			require.NoError(t, err)
		}
	}
	return failed
}

// runHistory runs one NMI history job and reports whether it failed.
func runHistory(t *testing.T, fleet *river.Client[pgx.Tx]) bool {
	t.Helper()
	return runOnce(t, fleet, nmiHistoryJob{})
}

// runOnce queues one job and reports whether it failed.
func runOnce(t *testing.T, fleet *river.Client[pgx.Tx], args river.JobArgs) bool {
	t.Helper()
	events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer unsubscribe()
	job, err := fleet.Insert(t.Context(), args, nil)
	require.NoError(t, err)
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if event != nil && event.Job.ID == job.Job.ID {
				return event.Kind == river.EventKindJobFailed
			}
		case <-deadline.C:
			t.Fatalf("%s did not finish", args.Kind())
		}
	}
}

// isHistoryRead is NMI's transaction report as the history job reads it: a
// window of every transaction, naming no sale, schedule or vault.
func isHistoryRead(r *http.Request) bool {
	if !strings.HasSuffix(r.URL.Path, "/query.php") {
		return false
	}
	_ = r.ParseForm()
	return r.Form.Get("start_date") != "" && r.Form.Get("order_id") == "" && r.Form.Get("subscription_id") == "" &&
		r.Form.Get("customer_vault_id") == "" && r.Form.Get("transaction_id") == ""
}

type nmiHistoryReadRow struct {
	ReadAt *time.Time
	DueAt  time.Time
}

func nmiHistoryReads(t *testing.T, a *authorityWorld) []nmiHistoryReadRow {
	t.Helper()
	rows, err := a.pool.Query(t.Context(), "SELECT read_at, due_at FROM "+pgx.Identifier{a.schema, "nmi_history_reads"}.Sanitize()+" WHERE merchant_id = $1", a.alpha.MerchantID().UUID())
	require.NoError(t, err)
	reads, err := pgx.CollectRows(rows, pgx.RowToStructByPos[nmiHistoryReadRow])
	require.NoError(t, err)
	return reads
}

// awaitHistoryQueued waits for the merchant's configuration, loaded in the
// background at start, to queue its NMI PSP's history.
func awaitHistoryQueued(t *testing.T, a *authorityWorld) {
	t.Helper()
	require.Eventually(t, func() bool { return len(nmiHistoryReads(t, a)) == 1 }, 20*time.Second, 20*time.Millisecond,
		"the declared NMI PSP's history is queued")
}

// nmiHistoryRead is the merchant's one NMI PSP's history read: when it last
// completed and when the next is due.
func nmiHistoryRead(t *testing.T, a *authorityWorld) (*time.Time, time.Time) {
	t.Helper()
	reads := nmiHistoryReads(t, a)
	require.Len(t, reads, 1)
	return reads[0].ReadAt, reads[0].DueAt
}

// nmiHistoryCounts sums the merchant's NMI history authorizations by outcome.
func nmiHistoryCounts(t *testing.T, client *openrails.Client) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	for _, row := range metricTable(t, client, billing.QueryMetricsParams{Measures: []string{"nmi_history_authorizations"},
		By: []string{"outcome"}, Range: &billing.MetricsRange{Last: "90d"}}) {
		got[fmt.Sprint(row["outcome"])] += whole(t, row["nmi_history_authorizations"])
	}
	return got
}

// openInvoices counts the invoices still open in storage.
func openInvoices(t *testing.T, w *world) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "invoices"}.Sanitize()+" WHERE status = 'open'").Scan(&n))
	return n
}

// blockedBy counts the sessions waiting on a lock the holder session holds.
func blockedBy(t *testing.T, w *world, holder int32) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))", holder).Scan(&n))
	return n
}

// holdInvoice locks the invoice's row, as a writer would, until unlock: a pass
// reaching it waits.
func holdInvoice(t *testing.T, w *world, invoice billing.InvoiceID) (holder int32, unlock func()) {
	t.Helper()
	conn, err := w.pool.Acquire(t.Context())
	require.NoError(t, err)
	tx, err := conn.Begin(t.Context())
	require.NoError(t, err)
	var once sync.Once
	unlock = func() {
		once.Do(func() {
			_ = tx.Rollback(context.Background())
			conn.Release()
		})
	}
	t.Cleanup(unlock)
	_, err = tx.Exec(t.Context(), "SELECT 1 FROM "+pgx.Identifier{w.schema, "invoices"}.Sanitize()+" WHERE id = $1 FOR UPDATE", invoice.UUID())
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&holder))
	return holder, unlock
}

// awaitAnyJob waits for the first of the jobs to finish.
func awaitAnyJob(t *testing.T, events <-chan *river.Event, ids map[int64]bool) *river.Event {
	t.Helper()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if event != nil && ids[event.Job.ID] {
				return event
			}
		case <-deadline.C:
			t.Fatal("no billing pass finished")
		}
	}
}

// reported tells whether the job recorded a pass report.
func reported(t *testing.T, w *world, job int64) bool {
	t.Helper()
	var ok bool
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT metadata ? 'output' FROM "+pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()+" WHERE id = $1", job).Scan(&ok))
	return ok
}

// nextHour is the top of the UTC hour after at: the next pass.
func nextHour(at time.Time) time.Time { return at.UTC().Truncate(time.Hour).Add(time.Hour) }

func advanceTo(w *world, at time.Time) { w.clock.Advance(at.Sub(w.clock.Now())) }

func entitled(t *testing.T, client *openrails.Client, customer billing.CustomerID, entitlement string) bool {
	t.Helper()
	access, err := client.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{entitlement}})
	require.NoError(t, err)
	return access.Entitlements[entitlement]
}

func lotExpired(t *testing.T, client *openrails.Client, id billing.CreditGrantID) int64 {
	t.Helper()
	g, err := client.GetCreditGrant(t.Context(), id)
	require.NoError(t, err)
	return g.ExpiredAmount
}

// suspendHeld declares the built-in schedule with held renewals suspended at
// their allowance.
func suspendHeld(w *world) {
	m := cfgMerchant(w.cfg, "alpha")
	m.MerchantSettings.DunningPolicy = &billing.DunningPolicy{Tiers: []billing.DunningTier{
		{MaxCycleHours: 96}, {MaxCycleHours: 672, RetryAfterHours: []int{24, 48}}, {RetryAfterHours: []int{48, 96, 144, 216}},
	}, AccessWhileRenewalHeld: billing.DunningAccessSuspend}
	setCfgMerchant(&w.cfg, "alpha", m)
}

func TestBillingPass(t *testing.T) {
	t.Run("OR-J04-S42/an-engine-renewal-waits-for-the-hourly-pass-with-access-unbroken", func(t *testing.T) {
		// An hourly member whose held renewal suspends access: the old 6m
		// allowance ended long before the next hourly pass.
		a, p, sub := purchasedMembershipWith(t, 1, suspendHeld)
		due := *sub.CurrentPeriodEndsAt
		customer := sub.CustomerID
		fleet := quietFleet(t, a.world)
		windows, err := a.alpha.ListProductAccess(t.Context(), billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{customer}})
		require.NoError(t, err)
		var grace *billing.ProductAccessGrant
		for i := range windows.Items {
			if windows.Items[i].SourceType == "grace" {
				grace = &windows.Items[i]
			}
		}
		require.NotNil(t, grace, "the paid period carries its renewal grace")
		require.Equal(t, due, grace.StartsAt)
		require.NotNil(t, grace.EndsAt)
		renewing := nextHour(due)
		require.False(t, grace.EndsAt.Before(due.Add(2*time.Hour)), "grace reaches two passes past the due instant")
		require.True(t, grace.EndsAt.After(renewing), "grace %s outlasts the renewing pass %s", grace.EndsAt, renewing)
		t.Logf("due %s, grace ends %s, renewing pass %s", due.Format(time.RFC3339), grace.EndsAt.Format(time.RFC3339), renewing.Format(time.RFC3339))
		for _, at := range []time.Time{due, due.Add(time.Minute), renewing.Add(-time.Second)} {
			advanceTo(a.world, at)
			require.True(t, entitled(t, a.alpha, customer, "membership:paid"), "access at %s, before the pass", at)
		}
		advanceTo(a.world, renewing)
		require.False(t, runPass(t, fleet))
		renewalSettled(t, a, p, sub, due)
		require.True(t, entitled(t, a.alpha, customer, "membership:paid"))
	})
	t.Run("OR-J04-S43/an-nmi-retry-is-queued-by-the-next-pass", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		due := *sub.CurrentPeriodEndsAt
		p.core.EditSchedule(*sub.RailSubscriptionID, func(s *nmimock.Schedule) { s.NextBilling = due.Add(7 * 24 * time.Hour) })
		fleet := quietFleet(t, a.world)
		// NMI skipped the period: the rebill watch finds it a day on and
		// OpenRails collects it.
		missed := due.Add(24*time.Hour + 20*time.Minute)
		advanceTo(a.world, missed)
		require.True(t, entitled(t, a.alpha, sub.CustomerID, "nmi:member"))
		advanceTo(a.world, nextHour(missed))
		require.False(t, runPass(t, fleet), "the rebill watch hands the skipped period to collection")
		require.True(t, entitled(t, a.alpha, sub.CustomerID, "nmi:member"), "access holds while the retry waits")
		advanceTo(a.world, nextHour(a.clock.Now()))
		require.False(t, runPass(t, fleet), "the next pass queues the retry")
		nmiRenewalReadback(t, a, p, sub)
		require.Equal(t, missed.Format(time.DateOnly), a.clock.Now().Format(time.DateOnly), "collected the day it was found")
	})
	t.Run("OR-J04-S44/a-credit-lot-lapses-at-00-00-utc-on-that-pass", func(t *testing.T) {
		w := newWorld(t)
		meterForCapture(t, w)
		customer := billing.CustomerID(uuid.New())
		midnight := nextHour(w.clock.Now()).Truncate(24 * time.Hour).Add(24 * time.Hour)
		lots, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "lapses-at-midnight", ExpiresAt: &midnight}})
		require.NoError(t, err)
		fleet := quietFleet(t, w)
		remaining := func() int64 {
			out := syncLease(t, w.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{computeView(customer)}})
			return lineOf(t, viewOf(t, out, customer), catalog.BalanceLimit).Remaining
		}
		advanceTo(w, midnight.Add(-time.Second))
		require.Zero(t, lotExpired(t, w.alpha, lots[0].ID))
		require.EqualValues(t, 1000, remaining(), "the lot is spendable before it lapses")
		advanceTo(w, midnight)
		require.Zero(t, remaining(), "a view never offers a lapsed lot, pass or no pass")
		require.False(t, runPass(t, fleet))
		require.EqualValues(t, 1000, lotExpired(t, w.alpha, lots[0].ID), "the 00:00 pass books the lapse")
	})
	t.Run("OR-J04-S45/a-delinquency-grace-ends-on-the-next-pass", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.ArrearsGraceDays = ptr(1)
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		oneUsage(t, a.alpha, usageInput(customer, "owed-past-grace", 3))
		a.clock.Advance(time.Second)
		fleet := quietFleet(t, a.world)
		runUsageDue(t, fleet)
		invoice := oneUsageInvoice(t, a, customer)
		require.NotNil(t, invoice.Usage)
		graceEnds := invoice.Usage.DueAt.Add(24 * time.Hour)
		advanceTo(a.world, graceEnds.Add(-time.Minute))
		require.False(t, runPass(t, fleet))
		require.False(t, oneUsageInvoice(t, a, customer).Usage.Delinquent, "inside its grace")
		advanceTo(a.world, nextHour(graceEnds))
		require.False(t, runPass(t, fleet))
		require.True(t, oneUsageInvoice(t, a, customer).Usage.Delinquent, "delinquent at the pass after its grace ends")
		out := syncLease(t, a.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{computeView(customer)}})
		require.NotNil(t, out.LimitSets[0].Refusal, "a gate refuses a delinquent customer's requests")
		require.Equal(t, billing.DenyDelinquentUnpaidInvoice, out.LimitSets[0].Refusal.DenyCode)
		require.Equal(t, graceEnds.Format(time.DateOnly), nextHour(graceEnds).Add(-time.Second).Format(time.DateOnly), "the same day as its grace end")
	})
	t.Run("OR-J04-S46/an-invoice-past-its-expiry-is-tidied-by-the-next-pass", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		fleet := quietFleet(t, a.world)
		_, price := publicSale(t, a, "tidied-invoice", 9_990_000, nil)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		expires := a.clock.Now().Add(time.Hour + 10*time.Minute)
		invoice, err := a.alphaRemote.CreateInvoice(t.Context(), "tidied", billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpiresAt: &expires})
		require.NoError(t, err)
		advanceTo(a.world, expires.Add(time.Minute))
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices/"+invoice.ID.String()+"/pay", a.aliceToken, billing.PayInvoiceParams{ExpectedTotal: 9_990_000, Payment: billing.InvoicePaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}, map[string]string{"Idempotency-Key": "past-expiry"})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"invoice_not_payable"`, "past its expiry an invoice is not payable, pass or no pass")
		advanceTo(a.world, nextHour(expires))
		require.False(t, runPass(t, fleet))
		expired, err := a.alphaRemote.GetInvoice(t.Context(), invoice.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceExpired, expired.Status)
		require.Empty(t, p.effects(t))
	})
	t.Run("OR-J04-S37/an-invoice-expiry-backlog-past-one-batch-drains-in-one-pass", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		fleet := quietFleet(t, a.world)
		_, price := publicSale(t, a, "backlog-invoice", 9_990_000, nil)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		expires := a.clock.Now().Add(time.Hour)
		// More than one batch of the expiry's reads.
		for i := range 201 {
			_, err := a.alpha.CreateInvoice(t.Context(), fmt.Sprintf("backlog-%d", i), billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpiresAt: &expires})
			require.NoError(t, err)
		}
		advanceTo(a.world, nextHour(expires))
		require.False(t, runPass(t, fleet))
		require.Zero(t, openInvoices(t, a.world), "one pass expires the whole backlog")
		require.Empty(t, p.effects(t))
	})
	t.Run("OR-J04-S38/a-pass-that-starts-while-one-runs-exits", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		fleet := quietFleet(t, a.world)
		_, price := publicSale(t, a, "overlapped-invoice", 9_990_000, nil)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		expires := a.clock.Now().Add(time.Hour)
		invoice, err := a.alpha.CreateInvoice(t.Context(), "overlapped", billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpiresAt: &expires})
		require.NoError(t, err)
		advanceTo(a.world, nextHour(expires))
		// The first pass reaches the invoice and waits on it.
		conn, err := a.pool.Acquire(t.Context())
		require.NoError(t, err)
		defer conn.Release()
		tx, err := conn.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.Background()) }()
		_, err = tx.Exec(t.Context(), "SELECT 1 FROM "+pgx.Identifier{a.schema, "invoices"}.Sanitize()+" WHERE id = $1 FOR UPDATE", invoice.ID.UUID())
		require.NoError(t, err)
		var holder int32
		require.NoError(t, tx.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&holder))
		events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
		defer unsubscribe()
		first, err := fleet.Insert(t.Context(), billingPass{}, nil)
		require.NoError(t, err)
		require.Eventually(t, func() bool { return blockedBy(t, a.world, holder) > 0 }, 30*time.Second, 20*time.Millisecond)
		require.False(t, runPass(t, fleet), "the second pass exits while the first runs")
		require.Equal(t, 1, openInvoices(t, a.world), "it ran no scan")
		require.NoError(t, tx.Rollback(t.Context()))
		deadline := time.NewTimer(60 * time.Second)
		defer deadline.Stop()
		for done := false; !done; {
			select {
			case event := <-events:
				if event != nil && event.Job.ID == first.Job.ID {
					require.Equal(t, river.EventKindJobCompleted, event.Kind)
					done = true
				}
			case <-deadline.C:
				t.Fatal("the first pass did not finish")
			}
		}
		require.Zero(t, openInvoices(t, a.world), "the first pass takes what is due")
	})
	t.Run("OR-J04-S80/two-passes-started-together-one-scans-the-other-exits-and-neither-holds-a-session", func(t *testing.T) {
		a, _ := newPaymentWorld(t, withTaggedSessions)
		fleet := quietFleet(t, a.world)
		_, price := publicSale(t, a, "raced-invoice", 9_990_000, nil)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		expires := a.clock.Now().Add(time.Hour)
		invoice, err := a.alpha.CreateInvoice(t.Context(), "raced", billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpiresAt: &expires})
		require.NoError(t, err)
		advanceTo(a.world, nextHour(expires))
		holder, unlock := holdInvoice(t, a.world, invoice.ID)
		defer unlock()
		events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
		defer unsubscribe()
		passes, err := fleet.InsertMany(t.Context(), []river.InsertManyParams{{Args: billingPass{}}, {Args: billingPass{}}})
		require.NoError(t, err)
		ids := map[int64]bool{passes[0].Job.ID: true, passes[1].Job.ID: true}
		exited := awaitAnyJob(t, events, ids)
		require.Equal(t, river.EventKindJobCompleted, exited.Kind)
		require.False(t, reported(t, a.world, exited.Job.ID), "the pass that found the other running exits without a report")
		require.Eventually(t, func() bool { return blockedBy(t, a.world, holder) > 0 }, 30*time.Second, 20*time.Millisecond, "the other scans")
		require.Equal(t, 1, blockedBy(t, a.world, holder), "and only it")
		require.Equal(t, 1, openInvoices(t, a.world))
		require.Equal(t, 1, liveLeases(t, a.world, "periodic_leases", "openrails.billing_pass.run"))
		require.Zero(t, heldSessions(t, a.world), "the running pass holds a lease row, not a session")
		unlock()
		delete(ids, exited.Job.ID)
		ran := awaitAnyJob(t, events, ids)
		require.Equal(t, river.EventKindJobCompleted, ran.Kind)
		require.True(t, reported(t, a.world, ran.Job.ID))
		require.Zero(t, openInvoices(t, a.world), "the running pass takes what is due")
		require.Zero(t, liveLeases(t, a.world, "periodic_leases", "openrails.billing_pass.run"), "and frees its lease")
	})
	t.Run("OR-J04-S81/a-dead-passs-lease-lapses-and-the-next-pass-runs", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		fleet := quietFleet(t, a.world)
		_, price := publicSale(t, a, "after-a-crash", 9_990_000, nil)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		expires := a.clock.Now().Add(time.Hour)
		_, err := a.alpha.CreateInvoice(t.Context(), "after-a-crash", billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpiresAt: &expires})
		require.NoError(t, err)
		advanceTo(a.world, nextHour(expires))
		// A pass whose process dies stops renewing its lease and never frees it.
		alive, die := context.WithCancel(t.Context())
		dead, err := worldDB(t, a.world).TryPassLease(alive, "openrails.billing_pass", 5*time.Second)
		require.NoError(t, err)
		require.NotNil(t, dead)
		die()
		require.False(t, runPass(t, fleet), "a pass under a live lease exits")
		require.Equal(t, 1, openInvoices(t, a.world), "and runs no scan")
		require.Eventually(t, func() bool { return liveLeases(t, a.world, "periodic_leases", "openrails.billing_pass.run") == 0 }, 20*time.Second, 100*time.Millisecond, "the dead pass's lease lapses")
		require.False(t, runPass(t, fleet))
		require.Zero(t, openInvoices(t, a.world), "the next pass takes what is due")
		require.Zero(t, liveLeases(t, a.world, "periodic_leases", "openrails.billing_pass.run"))
	})
	t.Run("OR-J04-S47/the-next-pass-takes-what-a-failed-or-missed-pass-left-due", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		due := *sub.CurrentPeriodEndsAt
		customer := billing.CustomerID(uuid.New())
		lapse := due.Add(-30 * time.Minute)
		lots, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "lapses-before-the-renewal", ExpiresAt: &lapse}})
		require.NoError(t, err)
		_, price := publicSale(t, a, "expires-before-the-renewal", 9_990_000, nil)
		expires := due.Add(-20 * time.Minute)
		invoice, err := a.alphaRemote.CreateInvoice(t.Context(), "tidied-after-a-failed-scan", billing.CreateInvoiceParams{CustomerID: sub.CustomerID, Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpiresAt: &expires})
		require.NoError(t, err)
		fleet := quietFleet(t, a.world)
		// The pass at the hour fails partway: the balance watch cannot post.
		restore := storageFault(t, a.world, a.schema, "ledger_transfers", "NEW.transfer_type = 'credit_expire'")
		first := nextHour(due)
		advanceTo(a.world, first)
		require.True(t, runPass(t, fleet), "a scan that fails fails the pass")
		renewalSettled(t, a, p, sub, due)
		require.Zero(t, lotExpired(t, a.alpha, lots[0].ID))
		tidied, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"invoice.expired"}, Limit: 10})
		require.NoError(t, err)
		require.Len(t, tidied.Events, 1, "the scans after the failed one still run")
		require.Equal(t, invoice.ID.String(), tidied.Events[0].Subject.ID)
		restore()
		// The next hour's pass never runs; the one after takes what is due.
		advanceTo(a.world, first.Add(2*time.Hour))
		require.False(t, runPass(t, fleet))
		require.EqualValues(t, 1000, lotExpired(t, a.alpha, lots[0].ID))
		renewalSettled(t, a, p, sub, due)
	})
	t.Run("OR-J04-S71/a-failed-nmi-history-read-fails-its-own-job-and-the-next-hours-reads-it", func(t *testing.T) {
		a, p := newNMIWorld(t)
		awaitHistoryQueued(t, a)
		fleet := quietFleet(t, a.world)
		p.core.QueryUnavailable(true)
		advanceTo(a.world, nextHour(a.clock.Now()))
		require.False(t, runPass(t, fleet), "the billing pass reads no NMI history")
		require.True(t, runHistory(t, fleet), "a history read NMI refused fails the history job, not only a log line")
		read, due := nmiHistoryRead(t, a)
		require.Nil(t, read, "a failed read records none")
		require.False(t, due.After(a.clock.Now()), "and stays due")
		p.core.QueryUnavailable(false)
		advanceTo(a.world, nextHour(a.clock.Now()))
		require.False(t, runHistory(t, fleet), "the next hour's job reads the account's history")
		read, due = nmiHistoryRead(t, a)
		require.NotNil(t, read)
		require.True(t, read.Equal(a.clock.Now()), "read at %s", read)
		require.True(t, due.Equal(read.Add(23*time.Hour)), "a completed read is due again a day later, less the hour: %s", due)
	})
	t.Run("OR-J04-S83/a-slow-nmi-history-read-holds-up-no-billing-pass-and-catches-up", func(t *testing.T) {
		a, p := newNMIWorld(t)
		vault := p.core.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
		at := a.clock.Now().Add(-48 * time.Hour)
		for _, sale := range []nmimock.Sale{{}, {Declined: "200", IssuerCode: "51"}} {
			sale.Vault, sale.Amount, sale.At = vault, "10.00", at
			p.core.AddSale(sale)
			at = at.Add(time.Minute)
		}
		require.NoError(t, p.persist(t.Context()))
		awaitHistoryQueued(t, a)
		// NMI's transaction report stalls: the history read waits on it.
		stalled := p.core.Hold(isHistoryRead, nmimock.HoldRequest)
		defer stalled.Release()
		advanceTo(a.world, nextHour(a.clock.Now()))
		// One billing worker and the fleet's own schedule: the hourly history
		// job is queued at start, beside the billing pass.
		fleet := hostFleet(t, a.world, 1)
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		select {
		case <-stalled.Arrived():
		case <-time.After(20 * time.Second):
			t.Fatal("the hourly NMI history job did not reach NMI")
		}
		queued, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.nmi_history").States(rivertype.JobStateRunning))
		require.NoError(t, err)
		require.Len(t, queued.Jobs, 1, "NMI history is its own job")
		history := queued.Jobs[0]
		require.Equal(t, "nmi_history", history.Queue, "provider reporting reads run on their own queue")
		require.False(t, runPass(t, fleet), "the billing pass runs and finishes while the history read waits")
		running, err := fleet.JobGet(t.Context(), history.ID)
		require.NoError(t, err)
		require.Equal(t, rivertype.JobStateRunning, running.State, "the history read still waits on NMI")

		stalled.Release()
		require.Eventually(t, func() bool {
			done, err := fleet.JobGet(t.Context(), history.ID)
			return err == nil && done.State == rivertype.JobStateCompleted
		}, 20*time.Second, 20*time.Millisecond, "once NMI answers, the history job finishes")
		require.Equal(t, map[string]int64{"approved": 1, "declined": 1}, nmiHistoryCounts(t, a.alpha), "and the account's history is caught up")

		// A completed read waits a day: the next hour's job reads nothing, the
		// next day's reads the account again.
		var reads atomic.Int32
		p.core.Intercept(isHistoryRead, func(_ *http.Request, serve func() *http.Response) (*http.Response, error) {
			reads.Add(1)
			return serve(), nil
		})
		advanceTo(a.world, nextHour(a.clock.Now()))
		require.False(t, runHistory(t, fleet))
		require.Zero(t, reads.Load(), "nothing is due an hour after a read")
		a.clock.Advance(23 * time.Hour)
		require.False(t, runHistory(t, fleet))
		require.Positive(t, reads.Load(), "a day after it the account is read again")
	})
	t.Run("OR-J04-S48/job-progress-fails-a-day-overdue-and-clears-on-a-clean-pass", func(t *testing.T) {
		w := newWorld(t)
		customer := billing.CustomerID(uuid.New())
		lapse := w.clock.Now().Add(time.Hour)
		lots, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "a-day-overdue", ExpiresAt: &lapse}})
		require.NoError(t, err)
		var progress func(context.Context) error
		for _, probe := range w.engine.Probes() {
			if probe.Name == "openrails_job_progress" {
				progress = probe.Check
			}
		}
		fleet := quietFleet(t, w)
		require.Eventually(t, func() bool { return progress(t.Context()) == nil }, 20*time.Second, 50*time.Millisecond, "a fresh leader")
		restore := storageFault(t, w, w.schema, "ledger_transfers", "NEW.transfer_type = 'credit_expire'")
		advanceTo(w, nextHour(lapse))
		require.True(t, runPass(t, fleet))
		require.NoError(t, progress(t.Context()), "an hour late is not a day overdue")
		advanceTo(w, nextHour(lapse.Truncate(24*time.Hour).Add(48*time.Hour)))
		require.True(t, runPass(t, fleet))
		err = progress(t.Context())
		require.Error(t, err)
		require.Contains(t, err.Error(), "a day overdue")
		require.Contains(t, err.Error(), "openrails.balance_watch")
		restore()
		require.False(t, runPass(t, fleet))
		require.EqualValues(t, 1000, lotExpired(t, w.alpha, lots[0].ID))
		require.NoError(t, progress(t.Context()), "a clean pass clears it")
	})
	t.Run("OR-J04-S60/a-live-operation-whose-wake-was-lost-is-queued-again-an-hour-past-due", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		due := sub.CurrentPeriodEndsAt.Add(pullMargin)
		fleet := quietFleet(t, s.world)
		settleOperations(t, fleet)
		// The period's pull loses its wake: its River job is deleted.
		jobs := operationJobs(t, fleet)
		require.NotEmpty(t, jobs)
		for _, job := range jobs {
			_, err := fleet.JobDelete(t.Context(), job.ID)
			require.NoError(t, err)
		}
		sends := s.wire.sends()
		advanceTo(s.world, due.Add(30*time.Minute))
		require.False(t, runPass(t, fleet))
		require.Empty(t, operationJobs(t, fleet), "a wake less than an hour late is not taken as lost")
		require.Equal(t, sends, s.wire.sends())
		advanceTo(s.world, due.Add(90*time.Minute))
		landedAt := s.clock.Now()
		require.False(t, runPass(t, fleet))
		s.renewed(t, sub, landedAt.Add(168*time.Hour))
		settleOperations(t, fleet)
		require.False(t, runPass(t, fleet))
		settleOperations(t, fleet)
		require.Len(t, operationJobs(t, fleet, rivertype.JobStateScheduled), 1, "the next period's pull, not overdue, keeps its one wake")
	})
}
