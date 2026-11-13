//go:build integration

package critical_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
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
	events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer unsubscribe()
	job, err := fleet.Insert(t.Context(), billingPass{}, nil)
	require.NoError(t, err)
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if event == nil || event.Job.ID != job.Job.ID {
				continue
			}
			// River runs accepted operations on wall time; business time is the test's.
			jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
			require.NoError(t, err)
			for _, op := range jobs.Jobs {
				if op.State == rivertype.JobStateScheduled || op.State == rivertype.JobStateRetryable {
					_, err = fleet.JobRetry(t.Context(), op.ID)
					require.NoError(t, err)
				}
			}
			return event.Kind == river.EventKindJobFailed
		case <-deadline.C:
			t.Fatal("the billing pass did not finish")
		}
	}
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
	m := w.cfg.Merchants["alpha"]
	m.MerchantSettings.DunningPolicy = &billing.DunningPolicy{Tiers: []billing.DunningTier{
		{MaxCycleHours: 96}, {MaxCycleHours: 672, RetryAfterHours: []int{24, 48}}, {RetryAfterHours: []int{48, 120, 216, 312}},
	}, AccessWhileRenewalHeld: billing.DunningAccessSuspend}
	w.cfg.Merchants["alpha"] = m
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
		advanceTo(w, midnight.Add(-time.Second))
		require.Zero(t, lotExpired(t, w.alpha, lots[0].ID))
		ok, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "before-midnight", 100)})
		require.NoError(t, err)
		require.True(t, ok[0].Allowed(), "the lot is spent before it lapses")
		advanceTo(w, midnight)
		denied, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "at-midnight", 100)})
		require.NoError(t, err)
		require.False(t, denied[0].Allowed(), "admission never spends a lapsed lot, pass or no pass")
		require.False(t, runPass(t, fleet))
		require.EqualValues(t, 1000, lotExpired(t, w.alpha, lots[0].ID), "the 00:00 pass books the lapse")
	})
	t.Run("OR-J04-S45/a-delinquency-grace-ends-on-the-next-pass", func(t *testing.T) {
		a, _, customer := usageBillingWorld(t, true, true, func(w *world) {
			m := w.cfg.Merchants["alpha"]
			m.ArrearsGraceDays = ptr(1)
			w.cfg.Merchants["alpha"] = m
		})
		oneUsage(t, a.alpha, usageInput(customer, "owed-past-grace", 3))
		fleet := quietFleet(t, a.world)
		runUsageDue(t, fleet)
		order := oneUsageOrder(t, a, customer)
		require.NotNil(t, order.Usage)
		graceEnds := order.Usage.DueAt.Add(24 * time.Hour)
		advanceTo(a.world, graceEnds.Add(-time.Minute))
		require.False(t, runPass(t, fleet))
		require.False(t, oneUsageOrder(t, a, customer).Usage.Delinquent, "inside its grace")
		advanceTo(a.world, nextHour(graceEnds))
		require.False(t, runPass(t, fleet))
		require.True(t, oneUsageOrder(t, a, customer).Usage.Delinquent, "delinquent at the pass after its grace ends")
		results, err := a.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(a.world, customer, "after-grace", 500)})
		require.NoError(t, err)
		require.False(t, results[0].Allowed())
		require.Equal(t, graceEnds.Format(time.DateOnly), nextHour(graceEnds).Add(-time.Second).Format(time.DateOnly), "the same day as its grace end")
	})
	t.Run("OR-J04-S46/an-order-past-its-expiry-is-tidied-by-the-next-pass", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		fleet := quietFleet(t, a.world)
		_, price := publicSale(t, a, "tidied-order", 9_990_000, nil)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		expires := a.clock.Now().Add(time.Hour + 10*time.Minute)
		order, err := a.alphaRemote.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpiresAt: &expires, IdempotencyKey: "tidied"})
		require.NoError(t, err)
		advanceTo(a.world, expires.Add(time.Minute))
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", a.aliceToken, billing.PayOrderParams{ExpectedTotal: 9_990_000, Payment: billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}, map[string]string{"Idempotency-Key": "past-expiry"})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"order_not_payable"`, "past its expiry an order is not payable, pass or no pass")
		advanceTo(a.world, nextHour(expires))
		require.False(t, runPass(t, fleet))
		expired, err := a.alphaRemote.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderExpired, expired.Status)
		require.Empty(t, p.effects(t))
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
		order, err := a.alphaRemote.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: sub.CustomerID, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpiresAt: &expires, IdempotencyKey: "tidied-after-a-failed-scan"})
		require.NoError(t, err)
		fleet := quietFleet(t, a.world)
		// The pass at the hour fails partway: the balance watch cannot post.
		restore := storageFault(t, a.world, a.schema, "ledger_transfers", "NEW.transfer_type = 'credit_expire'")
		first := nextHour(due)
		advanceTo(a.world, first)
		require.True(t, runPass(t, fleet), "a scan that fails fails the pass")
		renewalSettled(t, a, p, sub, due)
		require.Zero(t, lotExpired(t, a.alpha, lots[0].ID))
		tidied, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"order.expired"}, Limit: 10})
		require.NoError(t, err)
		require.Len(t, tidied.Events, 1, "the scans after the failed one still run")
		require.Equal(t, order.ID.String(), tidied.Events[0].Subject.ID)
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
}
