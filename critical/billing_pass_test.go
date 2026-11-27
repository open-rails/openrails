//go:build integration

package critical_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
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

// runOnce queues one job until it runs and reports whether it failed.
func runOnce(t *testing.T, fleet *river.Client[pgx.Tx], args river.JobArgs) bool {
	t.Helper()
	finished := finishPass(t, fleet, args.Kind(), func() []int64 {
		job, err := fleet.Insert(t.Context(), args, nil)
		require.NoError(t, err)
		return []int64{job.Job.ID}
	})
	return finished[0].Kind == river.EventKindJobFailed
}

// passExits runs one billing pass and requires it to exit: another run holds
// the pass's lease.
func passExits(t *testing.T, fleet *river.Client[pgx.Tx]) {
	t.Helper()
	finished := finishJobs(t, fleet, func() []int64 {
		job, err := fleet.Insert(t.Context(), billingPass{}, nil)
		require.NoError(t, err)
		return []int64{job.Job.ID}
	})
	require.Equal(t, river.EventKindJobCompleted, finished[0].Kind)
	require.False(t, passRan(t, fleet, finished[0]), "it exits without its report")
}

// leasedPasses exit while another run of their kind holds its lease: one
// that ran records its report, one that exited none.
var leasedPasses = map[string]bool{"openrails.billing_pass": true}

// finishPass queues rounds of jobs with insert until one of a round ran, and
// returns that round's finishes. A fleet's own pass, such as the one it runs
// on start, can hold the lease while a round starts; that round exits and is
// queued again. Any other kind's first round is its answer.
func finishPass(t *testing.T, fleet *river.Client[pgx.Tx], kind string, insert func() []int64) []*river.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		finished := finishJobs(t, fleet, insert)
		if !leasedPasses[kind] || slices.ContainsFunc(finished, func(e *river.Event) bool { return passRan(t, fleet, e) }) {
			return finished
		}
		if time.Now().After(deadline) {
			t.Fatalf("every %s exited: another run kept its lease", kind)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// finishJobs queues one round of jobs with insert and returns their finishes.
func finishJobs(t *testing.T, fleet *river.Client[pgx.Tx], insert func() []int64) []*river.Event {
	t.Helper()
	events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer unsubscribe()
	waiting := map[int64]bool{}
	for _, id := range insert() {
		waiting[id] = true
	}
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	var finished []*river.Event
	for len(waiting) != 0 {
		select {
		case event := <-events:
			if event != nil && waiting[event.Job.ID] {
				finished = append(finished, event)
				delete(waiting, event.Job.ID)
			}
		case <-deadline.C:
			t.Fatalf("jobs did not finish: %v", waiting)
		}
	}
	return finished
}

// passRan reports whether a finished leased pass ran rather than exited.
func passRan(t *testing.T, fleet *river.Client[pgx.Tx], event *river.Event) bool {
	t.Helper()
	if event.Kind == river.EventKindJobFailed {
		return true
	}
	job, err := fleet.JobGet(t.Context(), event.Job.ID)
	require.NoError(t, err)
	return job.Output() != nil
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
		// Until its expiry posts, the lapsed lot is in the balance: the 00:00
		// pass books it.
		advanceTo(w, midnight)
		require.EqualValues(t, 1000, remaining())
		require.False(t, runPass(t, fleet))
		require.EqualValues(t, 1000, lotExpired(t, w.alpha, lots[0].ID), "the 00:00 pass books the lapse")
		require.Zero(t, remaining())
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
		passExits(t, fleet)
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
		passExits(t, fleet)
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
