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
		runRegistered(t, fleet, "openrails.dunning", 1)
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
		runRegistered(t, fleet, "openrails.rebill_watch", 1)
		subscriptionState(t, a, sub.ID, "active")
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.rebill_watch", 1)
		subscriptionState(t, a, sub.ID, "unverified")
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"nmi:member"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["nmi:member"], "unverified keeps access")
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
		runRegistered(t, fleet, "openrails.dunning", 1)
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.CurrentPeriodStartsAt.Equal(*sub.CurrentPeriodEndsAt)
		}, 20*time.Second, 20*time.Millisecond)
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		require.Empty(t, held(), "the finding closes once the renewal is attempted")
	})
}
