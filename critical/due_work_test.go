//go:build integration

package critical_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// customerConvergeJobs counts the customer's queued convergence jobs.
func customerConvergeJobs(t *testing.T, w *world, customer billing.CustomerID) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "river_job"}.Sanitize()+
		" WHERE kind = 'openrails.customer_converge' AND args->>'customer_id' = $1", customer.UUID().String()).Scan(&n))
	return n
}

// accessEndedNotices counts the customer's access-ended notices.
func accessEndedNotices(t *testing.T, w *world, customer billing.CustomerID) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "notification_events"}.Sanitize()+
		" WHERE customer_id = $1 AND event_type = 'premium_ended'", customer.UUID()).Scan(&n))
	return n
}

// stripePastDueMembership imports a Stripe subscription whose provider is
// still dunning it: past_due, Stripe's own retries unknown to OpenRails.
func stripePastDueMembership(t *testing.T) (*authorityWorld, *stripeAccounts, *billing.Subscription) {
	t.Helper()
	var accounts *stripeAccounts
	a := newAuthorityWorld(t, func(w *world) {
		accounts = newStripeAccounts(t, w, "sk_test_old")
		w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
		m := w.cfg.Merchants["alpha"]
		m.PSPs = openrails.PSPs{"old": stripePSP("acct_old", "sk_test_old", false)}
		w.cfg.Merchants["alpha"] = m
	})
	old := accounts.accounts["sk_test_old"].core
	old.AddProduct("prod_legacy", "Legacy membership")
	old.AddPrice(stripemock.Price{ID: "price_legacy_monthly", Product: "prod_legacy", Amount: 1000, Interval: "month"})
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "stripe-membership", DisplayName: "Stripe membership", Entitlements: []string{"stripe:member"}})
	require.NoError(t, err)
	price, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "stripe-monthly", PSP: "old", PriceID: "price_legacy_monthly"})
	require.NoError(t, err)
	end := a.clock.Now().Truncate(time.Second)
	start := end.Add(-30 * 24 * time.Hour)
	customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
	remote := old.AddSubscription(old.AddCustomer(a.alice.Email, "Dunned member", nil), "pm_dunned", "price_legacy_monthly", 1000, start, end)
	book := billing.DeclaredBilling{AsOf: end, DefaultPSP: billing.PSPRef{Key: "old"}, Customers: []billing.DeclaredCustomer{{Customer: customer}},
		Subscriptions: []billing.DeclaredSubscription{{SourceID: "dunned-stripe-membership", Customer: customer, Price: price.ID, Rail: "stripe", RailSubscriptionID: remote,
			StartedAt: start, PaidThrough: &end, Dunning: &billing.DunningEvidence{Retries: 1, ScheduleLive: true}}}}
	imported, err := a.alpha.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Empty(t, imported.Blocked, "%+v", imported.Reasons)
	subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	sub := subscriptionState(t, a, subs.Items[0].ID, "past_due")
	return a, accounts, sub
}

func TestDueWork(t *testing.T) {
	t.Run("OR-J14-S70/an-engine-renewal-nothing-attempted-reads-as-missed", func(t *testing.T) {
		a, _, sub := purchasedMembership(t)
		due := *sub.CurrentPeriodEndsAt
		renewals := func() []billing.Renewal {
			list, err := a.alpha.ListRenewals(t.Context(), billing.RenewalListParams{SubscriptionID: sub.ID})
			require.NoError(t, err)
			return list.Items
		}
		missed := func() int64 {
			result, err := a.alpha.QueryMetrics(t.Context(), billing.MetricsQuery{Measures: []string{"rebills_missed"},
				Range: &billing.MetricsRange{From: due.Add(-time.Hour).Format(time.RFC3339), To: a.clock.Now().Add(time.Second).Format(time.RFC3339)}})
			require.NoError(t, err)
			var total int64
			for _, row := range result.Rows {
				var n int64
				_, err := fmt.Sscan(fmt.Sprint(row[len(row)-1]), &n)
				require.NoError(t, err)
				total += n
			}
			return total
		}
		a.clock.Advance(due.Add(59 * time.Minute).Sub(a.clock.Now()))
		require.Empty(t, renewals(), "inside its deadline a renewal is neither attempted nor missed")
		require.Zero(t, missed())
		a.clock.Advance(2 * time.Minute)
		// No job runs: the miss is read from the subscription.
		list := renewals()
		require.Len(t, list, 1)
		require.Equal(t, due, list[0].DueAt)
		require.Equal(t, "missed", list[0].FirstOutcome)
		require.Equal(t, "not_attempted", list[0].MissReason)
		require.Equal(t, due.Add(time.Hour), *list[0].MissedAt)
		require.Equal(t, "open", list[0].Outcome)
		require.EqualValues(t, 10_000_000, list[0].Amount)
		read, err := a.alpha.GetRenewal(t.Context(), list[0].ID)
		require.NoError(t, err)
		require.Equal(t, list[0].ID, read.ID)
		require.Empty(t, read.Attempts)
		require.EqualValues(t, 1, missed())
	})
	t.Run("OR-J16-S70/a-provider-billed-grace-that-ends-unanswered-is-read-by-the-due-pass", func(t *testing.T) {
		a, accounts, sub := stripePastDueMembership(t)
		var grace, retry *time.Time
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT grace_ends_at, next_retry_at FROM "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" WHERE id = $1",
			sub.ID.UUID()).Scan(&grace, &retry))
		require.NotNil(t, grace, "a dunned provider subscription waits out its grace")
		require.Nil(t, retry, "the provider runs its own retries")
		a.clock.Advance(grace.Add(time.Minute).Sub(a.clock.Now()))
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		remote := *sub.RailSubscriptionID
		require.Eventually(t, func() bool { return accounts.read("sk_test_old", "/v1/subscriptions/"+remote) }, 20*time.Second, 20*time.Millisecond,
			"a grace that ended with no word from the provider is a question to it, by id")
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.Status != billing.SubscriptionStatus("past_due")
		}, 20*time.Second, 20*time.Millisecond, "the due pass moved it out of dunning")
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"stripe:member"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["stripe:member"], "an unanswered grace keeps access")
	})
	t.Run("OR-J15-S70/an-nmi-renewal-the-probe-cannot-settle-is-read-by-id-after-its-grace", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		p.core.FailRequests(func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/query.php") }, http.StatusServiceUnavailable, 1_000_000)
		due := *sub.CurrentPeriodEndsAt
		a.clock.Advance(due.Add(25 * time.Hour).Sub(a.clock.Now()))
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		subscriptionState(t, a, sub.ID, "active")
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		subscriptionState(t, a, sub.ID, "unverified")
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"nmi:member"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["nmi:member"], "unverified keeps access")
	})
	t.Run("OR-J16-S71/an-overdue-renewal-is-asked-before-the-rebill-watch-reads-nmi", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		// NMI's Query API stops answering a probe of this subscription's sales.
		hold := p.core.Hold(func(r *http.Request) bool {
			if !strings.HasSuffix(r.URL.Path, "/query.php") {
				return false
			}
			_ = r.ParseForm()
			return r.Form.Get("order_id") == sub.ID.UUID().String()
		}, nmimock.HoldRequest)
		defer hold.Release()
		a.clock.Advance(sub.CurrentPeriodEndsAt.Add(49 * time.Hour).Sub(a.clock.Now()))
		fleet := quietFleet(t, a.world)
		_, err := fleet.Insert(t.Context(), billingPass{}, nil)
		require.NoError(t, err)
		select {
		case <-hold.Arrived():
		case <-time.After(20 * time.Second):
			t.Fatal("the rebill watch never probed NMI")
		}
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionStatus("unverified"), current.Status, "the due pass asked about the overdue renewal before NMI was read")
		hold.Release()
		require.True(t, entitled(t, a.alpha, sub.CustomerID, "nmi:member"), "unverified keeps access")
	})
	t.Run("OR-J15-S72/a-complete-provider-refresh-queues-no-due-work", func(t *testing.T) {
		a, _ := newNMIWorld(t)
		fleet := quietFleet(t, a.world)
		refreshMerchant(t, fleet, a.alpha.MerchantID().UUID())
		var queued int
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "river_job"}.Sanitize()+
			" WHERE kind IN ('openrails.dunning', 'openrails.usage_orders', 'openrails.billing_pass')").Scan(&queued))
		require.Zero(t, queued, "what falls due waits for the hourly billing pass; a refresh queues none of it")
	})
	t.Run("OR-J29-S70/a-grant-commits-with-its-customers-convergence", func(t *testing.T) {
		w := newWorld(t)
		customer := billing.CustomerID(uuid.New())
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "comp", DisplayName: "Comp", Entitlements: []string{"comp:reader"}})
		require.NoError(t, err)
		grant := func(key string) error {
			_, err := w.alpha.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: key,
				Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID, Hours: ptr(24), Reason: billing.GrantReasonComp}}})
			return err
		}
		restore := storageFault(t, w, w.schema, "river_job", "NEW.kind = 'openrails.customer_converge'")
		require.Error(t, grant("refused"), "a grant whose convergence cannot be queued is not made")
		access, err := w.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"comp:reader"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["comp:reader"])
		restore()
		require.NoError(t, grant("granted"))
		require.Equal(t, 1, customerConvergeJobs(t, w, customer), "the grant queued its customer's convergence with its commit")
	})
	t.Run("OR-J29-S71/a-revoked-last-window-tells-the-customer-without-a-sweep", func(t *testing.T) {
		w := newWorld(t, func(w *world) { w.cfg.ProviderWriteMode = openrails.ProviderWritesFull })
		customer := billing.CustomerID(uuid.New())
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "comp", DisplayName: "Comp", Entitlements: []string{"comp:reader"}})
		require.NoError(t, err)
		granted, err := w.alpha.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: "comp",
			Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID, Hours: ptr(24 * 30), Reason: billing.GrantReasonComp}}})
		require.NoError(t, err)
		fleet := hostFleet(t, w)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		require.NoError(t, w.alpha.RevokeProductAccess(t.Context(), granted[0].ID, billing.RevokeProductAccessParams{Reason: "comp withdrawn"}))
		require.Eventually(t, func() bool { return accessEndedNotices(t, w, customer) == 1 }, 20*time.Second, 20*time.Millisecond,
			"the revoke's own convergence tells the customer their access ended")
	})
	t.Run("OR-J29-S72/a-timed-window-that-lapses-is-told-by-the-health-pass", func(t *testing.T) {
		w := newWorld(t, func(w *world) { w.cfg.ProviderWriteMode = openrails.ProviderWritesFull })
		customer := billing.CustomerID(uuid.New())
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "pass", DisplayName: "Day pass", Entitlements: []string{"pass:day"}})
		require.NoError(t, err)
		_, err = w.alpha.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: "pass",
			Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID, Hours: ptr(1), Reason: billing.GrantReasonComp}}})
		require.NoError(t, err)
		fleet := hostFleet(t, w)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		require.Zero(t, accessEndedNotices(t, w, customer), "a live window has not lapsed")
		w.clock.Advance(2 * time.Hour)
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		require.Equal(t, 1, accessEndedNotices(t, w, customer), "a window whose time ran out is told by the hourly pass")
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		require.Equal(t, 1, accessEndedNotices(t, w, customer), "once")
	})
	t.Run("OR-J29-S73/only-an-explicit-decline-excuses-an-abandoned-operation", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		psp := psps.Items[0].ID.UUID()
		intents := pgx.Identifier{a.schema, "provider_intents"}.Sanitize()
		// What a terminal outcome leaves: Terminal(...) records no decline.
		terminal := map[string]*string{"no-evidence": nil, "other-evidence": ptr(`{"reason":"gone"}`), "not-declined": ptr(`{"declined":false}`), "declined": ptr(`{"declined":true}`)}
		seed := func(key, status string, expires *time.Time, evidence *string) string {
			var id uuid.UUID
			require.NoError(t, a.pool.QueryRow(t.Context(), "INSERT INTO "+intents+" (merchant_id, rail, intent_type, idempotency_key, status, next_attempt_at, origin, psp_id, expires_at, result_evidence) VALUES ($1, 'stripe', 'critical_operation', $2, $3, $4, 'system', $5, $6, $7::jsonb) RETURNING id",
				a.alpha.MerchantID().UUID(), key, status, a.clock.Now().Add(24*time.Hour), psp, expires, evidence).Scan(&id))
			return "provider_intent:" + id.String()
		}
		findings := func(status billing.FindingStatus) map[string]billing.Finding {
			page, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.provider_intent.abandoned", Status: status})
			require.NoError(t, err)
			out := map[string]billing.Finding{}
			for _, finding := range page.Items {
				out[finding.SubjectKey] = finding
			}
			return out
		}
		requireOpen := func(subjects map[string]string, names ...string) {
			t.Helper()
			open := findings("")
			for _, name := range names {
				require.Contains(t, open, subjects[name], "%s is no decline: its operation stays abandoned", name)
			}
		}
		undeclined := []string{"no-evidence", "other-evidence", "not-declined"}

		// An operation that ended this way is reported, unless it was declined.
		ended := map[string]string{}
		for name, evidence := range terminal {
			ended[name] = seed("ended-"+name, "failed_terminal", nil, evidence)
		}
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		requireOpen(ended, undeclined...)
		require.NotContains(t, findings(""), ended["declined"], "a decline is no abandoned operation")

		// An open finding outlives its operation ending, unless it was declined.
		past := a.clock.Now().Add(-time.Hour)
		overdue := map[string]string{}
		for name := range terminal {
			overdue[name] = seed("overdue-"+name, "pending", &past, nil)
		}
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		requireOpen(overdue, "no-evidence", "other-evidence", "not-declined", "declined")
		for name, evidence := range terminal {
			_, err := a.pool.Exec(t.Context(), "UPDATE "+intents+" SET status = 'failed_terminal', result_evidence = $1::jsonb WHERE idempotency_key = $2", evidence, "overdue-"+name)
			require.NoError(t, err)
		}
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		requireOpen(overdue, undeclined...)
		closed := findings("fixed")
		require.Contains(t, closed, overdue["declined"], "a declined operation is no longer abandoned")
		require.Equal(t, "auto_vanished", *closed[overdue["declined"]].Resolution)
		require.Len(t, closed, 1)

		// Past the lookback the list no longer re-raises them: closing is the
		// resolve query's call alone.
		a.clock.Advance(26 * time.Hour)
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		requireOpen(ended, undeclined...)
		requireOpen(overdue, undeclined...)
		require.Len(t, findings("fixed"), 1)
	})
	t.Run("OR-J29-S37/an-operation-scheduled-ahead-is-not-stuck", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		psp := psps.Items[0].ID.UUID()
		intents := pgx.Identifier{a.schema, "provider_intents"}.Sanitize()
		now := a.clock.Now()
		// Each was queued two days ago, and nothing runs it.
		seed := func(key, status string, attempts int, next time.Time, failure *string) string {
			var id uuid.UUID
			require.NoError(t, a.pool.QueryRow(t.Context(), "INSERT INTO "+intents+" (merchant_id, rail, intent_type, idempotency_key, status, attempts, next_attempt_at, last_failure_reason, origin, psp_id, created_at) VALUES ($1, 'stripe', 'critical_operation', $2, $3, $4, $5, $6, 'system', $7, $8) RETURNING id",
				a.alpha.MerchantID().UUID(), key, status, attempts, next, failure, psp, now.Add(-48*time.Hour)).Scan(&id))
			return id.String()
		}
		stuck := func() map[string]bool {
			page, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.provider_intent.stuck"})
			require.NoError(t, err)
			out := map[string]bool{}
			for _, finding := range page.Items {
				out[finding.SubjectKey] = true
			}
			return out
		}
		scheduled := seed("scheduled-a-week-ahead", "pending", 0, now.Add(7*24*time.Hour), nil)
		recent := seed("due-an-hour-ago", "pending", 0, now.Add(-time.Hour), nil)
		lost := seed("due-a-day-ago", "pending", 0, now.Add(-25*time.Hour), nil)
		failing := seed("failing-for-two-days", "failed_retryable", 12, now.Add(time.Hour), ptr("provider unavailable"))
		// Each re-park moves its next attempt five minutes on.
		parked := seed("parked-for-two-days", "pending", 0, now.Add(5*time.Minute), ptr("no handler registered for intent type critical_operation"))
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		open := stuck()
		require.False(t, open[scheduled], "an operation scheduled ahead waits for its instant")
		require.False(t, open[recent], "an hour past its attempt is not stuck")
		require.True(t, open[lost], "a day past its attempt with nothing running it is stuck")
		require.True(t, open[failing], "an operation failing for a day is stuck")
		require.True(t, open[parked], "an operation parked for a day is stuck")

		// A day past its instant, the scheduled one is stuck too.
		a.clock.Advance(8 * 24 * time.Hour)
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		require.True(t, stuck()[scheduled])
	})
	t.Run("OR-J04-S70/the-hourly-health-pass-raises-held-renewals", func(t *testing.T) {
		a, _, sub := purchasedMembership(t)
		a.clock.Advance(sub.CurrentPeriodEndsAt.Add(3 * time.Hour).Sub(a.clock.Now()))
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		held := func() []billing.Finding {
			findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.renewal.held"})
			require.NoError(t, err)
			return findings.Items
		}
		require.Empty(t, held())
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		require.Len(t, held(), 1, "an engine renewal past its allowance with no outcome is held")
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.CurrentPeriodStartsAt.Equal(*sub.CurrentPeriodEndsAt)
		}, 20*time.Second, 20*time.Millisecond)
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		require.Empty(t, held(), "the finding closes once the renewal is attempted")
	})
	t.Run("OR-J06-S65/a-psp-whose-webhooks-went-silent-while-pulls-find-its-charges-raises-a-finding", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
			m := w.cfg.Merchants["alpha"]
			m.PSPs = openrails.PSPs{}
			for key, subacc := range map[string]string{"silent": "0001", "quiet": "0002", "pulled": "0003"} {
				m.PSPs[key] = openrails.CCBillPSP{ClientAccnum: "945280", ClientSubacc: subacc, Environment: openrails.EnvironmentSandbox, Salt: "critical-ccbill-salt"}.PSPConfig()
			}
			w.cfg.Merchants["alpha"] = m
		})
		listed, err := w.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		psps := map[string]billing.PSP{}
		for _, psp := range listed.Items {
			psps[psp.Key] = psp
		}
		require.Len(t, psps, 3)
		merchantID, customer := w.alpha.MerchantID().UUID(), uuid.New()
		_, err = w.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{w.schema, "customers"}.Sanitize()+" (id, merchant_id) VALUES ($1, $2)", customer, merchantID)
		require.NoError(t, err)
		// A provider-scheduled charge of key's PSP that OpenRails learned of via.
		charge := func(key, via string, at time.Time) {
			t.Helper()
			_, err := w.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{w.schema, "payment_attempts"}.Sanitize()+
				` (merchant_id, customer_id, psp_id, rail, kind, owner, card_entry, source, observed_via, category, amount, currency, attempted_at, step)
				VALUES ($1, $2, $3, $4, 'initial', 'provider', 'saved', 'provider_schedule', $5, 'approved', 10000000, 'USD', $6, '')`,
				merchantID, customer, psps[key].ID.UUID(), string(psps[key].Rail), via, at)
			require.NoError(t, err)
		}
		now := w.clock.Now()
		for day := range 10 {
			at := now.Add(-time.Duration(day+2) * 24 * time.Hour)
			charge("silent", "webhook", at)
			charge("quiet", "webhook", at)
			charge("pulled", "pull", at)
		}
		// In the last day neither webhook-fed PSP sent one; pulls found the
		// silent PSP's charges and the pull-only PSP's, the quiet PSP had none.
		for hour := range 3 {
			at := now.Add(-time.Duration(hour+1) * time.Hour)
			charge("silent", "pull", at)
			charge("pulled", "pull", at)
		}
		fleet := hostFleet(t, w)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		silence := func(status billing.FindingStatus) map[string]billing.Finding {
			page, err := w.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.webhooks.silent", Status: status})
			require.NoError(t, err)
			out := map[string]billing.Finding{}
			for _, finding := range page.Items {
				out[finding.SubjectKey] = finding
			}
			return out
		}
		subject := "psp:" + psps["silent"].ID.String()

		runRegistered(t, fleet, "openrails.merchant_health", 1)
		open := silence("")
		require.Len(t, open, 1, "a PSP with no charges, or one that never sent webhooks, is not silent")
		require.Contains(t, open, subject, "the PSP whose webhooks stopped while pulls find its charges is silent")
		require.Equal(t, billing.FindingStatus("requires_review"), open[subject].Status)
		require.Equal(t, "high", open[subject].Severity)

		charge("silent", "webhook", now.Add(-30*time.Minute))
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		require.Empty(t, silence(""), "the finding clears once its webhooks resume")
		closed := silence("fixed")
		require.Contains(t, closed, subject)
		require.Equal(t, "auto_vanished", *closed[subject].Resolution)
	})
}
