//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

// dunningPolicy declares alpha's dunning policy.
func dunningPolicy(policy billing.DunningPolicy) func(*world) {
	return func(w *world) {
		m := cfgMerchant(w.cfg, "alpha")
		m.MerchantSettings.DunningPolicy = &policy
		setCfgMerchant(&w.cfg, "alpha", m)
	}
}

func TestDunningSchedule(t *testing.T) {
	t.Run("OR-J16-S50/a-monthly-renewal-is-retried-on-days-3-5-7-and-10-then-canceled", func(t *testing.T) {
		// The case ends nine days after the renewal came due: keep that in the past.
		a, p, sub := purchasedMembershipWith(t, 30*24, func(w *world) { w.clock.Advance(-15 * 24 * time.Hour) })
		enableLifecycleMaintenance(t, a)
		due := *sub.CurrentPeriodEndsAt
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		charged := func() []billing.PaymentAttempt {
			attempts, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: sub.ID, Kind: []string{"rebill", "dunning_retry"}})
			require.NoError(t, err)
			return attempts.Items
		}
		const day = 24 * time.Hour
		retries := []time.Duration{2 * day, 4 * day, 6 * day, 9 * day}
		for i, offset := range retries {
			failures := i + 1
			runRegistered(t, fleet, "openrails.billing_pass", 2)
			// A decline is recorded before the case advances: wait for the
			// retry it scheduled, not for the attempt count alone.
			var declined *billing.Subscription
			require.Eventually(t, func() bool {
				var err error
				declined, err = a.alpha.GetSubscription(t.Context(), sub.ID)
				return err == nil && declined.Dunning != nil && declined.Dunning.Attempts == failures &&
					declined.Dunning.NextRetryAt != nil && declined.Dunning.NextRetryAt.Equal(due.Add(offset))
			}, 20*time.Second, 20*time.Millisecond, "failure %d must be counted and its retry scheduled", failures)
			require.Equal(t, billing.SubscriptionStatus("past_due"), declined.Status)
			require.Equal(t, len(retries)-i, *declined.Dunning.RetriesLeft)
			require.Equal(t, due.Add(offset), *declined.Dunning.NextRetryAt)
			require.Equal(t, due.Add(9*day), *declined.Dunning.FinalRetryAt)
			require.Equal(t, billing.DeclineInsufficientFunds, *declined.Dunning.LastFailureReason)
			a.clock.Advance(due.Add(offset).Sub(a.clock.Now()) - time.Microsecond)
			runRegistered(t, fleet, "openrails.billing_pass", 2)
			require.Len(t, charged(), failures, "retry %d may not run before day %d", failures, int(offset/day)+1)
			a.clock.Advance(time.Microsecond)
		}
		// The fifth failure spends the schedule.
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		ended := subscriptionState(t, a, sub.ID, "canceled")
		require.NotNil(t, ended.CanceledAt)
		require.Nil(t, ended.Dunning)
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["membership:paid"])
		attempts := charged()
		require.Len(t, attempts, 5)
		// Newest first.
		for i, offset := range []time.Duration{9 * day, 6 * day, 4 * day, 2 * day, 0} {
			require.True(t, attempts[i].AttemptedAt.Equal(due.Add(offset)), "attempt %d ran at %s, want %s", len(attempts)-i, attempts[i].AttemptedAt, due.Add(offset))
		}
		require.Equal(t, "rebill", attempts[4].Kind)
		require.Len(t, p.effects(t), 1, "five declines collect nothing")
		p.core.SetDecline("4242", "")
		a.clock.Advance(4 * day)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		require.Len(t, charged(), 5, "a canceled subscription is not charged again")
		require.Len(t, p.effects(t), 1)
	})
	t.Run("OR-J16-S56/a-held-engine-case-ends-once-the-merchant-is-armed", func(t *testing.T) {
		for _, route := range []string{"armed", "admin-cancel", "new-card"} {
			t.Run(route, func(t *testing.T) {
				a, p, sub := purchasedMembershipWith(t, 7*24)
				enableLifecycleMaintenance(t, a)
				armWorldDestructive(t, a.world, false)
				p.core.SetDecline("4242", "insufficient_funds")
				a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
				fleet := startFleet(t, a.world)
				for attempt := 1; attempt <= 3; attempt++ {
					runRegistered(t, fleet, "openrails.billing_pass", 2)
					require.Eventually(t, func() bool {
						current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
						return err == nil && current.Dunning != nil && current.Dunning.Attempts == attempt &&
							(attempt == 3 || current.Dunning.NextRetryAt != nil)
					}, 20*time.Second, 20*time.Millisecond)
					if attempt < 3 {
						a.clock.Advance(24 * time.Hour)
					}
				}
				held := func(status billing.FindingStatus) billing.Finding {
					findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.terminal_outcome.held", Status: status})
					require.NoError(t, err)
					require.Len(t, findings.Items, 1)
					require.Equal(t, sub.ID.UUID().String(), findings.Items[0].SubjectKey)
					require.Equal(t, status, findings.Items[0].Status)
					return findings.Items[0]
				}
				current := subscriptionState(t, a, sub.ID, "past_due")
				require.Nil(t, current.Dunning.NextRetryAt)
				held("requires_review")
				access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}})
				require.NoError(t, err)
				require.True(t, access.Entitlements["membership:paid"])
				creates, effects, seen := paymentIntentCreates(p), p.effects(t), held("requires_review").LastSeenAt
				var version int64
				var grace, advice *time.Time
				read := func() {
					require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT row_version, grace_ends_at, retry_advised_at FROM "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" WHERE id = $1", sub.ID.UUID()).Scan(&version, &grace, &advice))
				}
				read()
				require.NotNil(t, grace)
				require.True(t, grace.Equal(a.clock.Now()))
				require.Nil(t, advice)
				heldVersion := version
				if route != "new-card" {
					for range 3 {
						a.clock.Advance(24 * time.Hour)
						runRegistered(t, fleet, "openrails.billing_pass", 2)
					}
				}
				read()
				require.Equal(t, heldVersion, version, "a disarmed held row is not written by later passes")
				require.Equal(t, seen, held("requires_review").LastSeenAt)
				held("requires_review")
				subscriptionState(t, a, sub.ID, "past_due")
				access, err = a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}})
				require.NoError(t, err)
				require.True(t, access.Entitlements["membership:paid"])
				require.Equal(t, creates, paymentIntentCreates(p))
				require.Equal(t, effects, p.effects(t))
				if route == "new-card" {
					method := saveMethod(t, a, p, a.aliceToken, "5556")
					p.core.SetDecline("5556", "incorrect_zip")
					status, raw := a.request(t, http.MethodPut, "/v1/me/subscriptions/"+sub.ID.String()+"/payment-method", a.aliceToken, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &method.ID})
					require.Equal(t, http.StatusOK, status, string(raw))
					runRegistered(t, fleet, "openrails.billing_pass", 2)
					subscriptionState(t, a, sub.ID, "awaiting_method")
					read()
					require.NotNil(t, grace)
					require.True(t, grace.After(a.clock.Now()), "a new card gets a future card-fix deadline")
					armWorldDestructive(t, a.world, true)
					runRegistered(t, fleet, "openrails.billing_pass", 1)
					subscriptionState(t, a, sub.ID, "awaiting_method")
					return
				}
				if route == "armed" {
					armWorldDestructive(t, a.world, true)
					runRegistered(t, fleet, "openrails.billing_pass", 1)
				} else {
					_, err := a.alpha.CancelSubscription(t.Context(), sub.ID, billing.CancelSubscriptionParams{Reason: "support request", RevokeAccess: true})
					require.NoError(t, err)
				}
				ended := subscriptionState(t, a, sub.ID, "canceled")
				if route == "armed" {
					require.Equal(t, "expired", *ended.CancelType)
				}
				held("fixed")
				access, err = a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}})
				require.NoError(t, err)
				require.False(t, access.Entitlements["membership:paid"])
				require.Equal(t, creates, paymentIntentCreates(p))
				require.Equal(t, effects, p.effects(t))
			})
		}
	})
	t.Run("OR-J16-S57/a-customers-own-declined-payments-spend-no-scheduled-retry", func(t *testing.T) {
		a, p, sub := purchasedMembershipWith(t, 30*24, func(w *world) { w.clock.Advance(-15 * 24 * time.Hour) })
		enableLifecycleMaintenance(t, a)
		due := *sub.CurrentPeriodEndsAt
		const day = 24 * time.Hour
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.Dunning != nil && current.Dunning.Attempts == 1 &&
				current.Dunning.NextRetryAt != nil && current.Dunning.NextRetryAt.Equal(due.Add(2*day))
		}, 20*time.Second, 20*time.Millisecond)
		current := subscriptionState(t, a, sub.ID, "past_due")
		require.Equal(t, 4, *current.Dunning.RetriesLeft)
		require.Equal(t, due.Add(9*day), *current.Dunning.FinalRetryAt)
		invoice := myRenewalInvoice(t, a, sub.ID)
		for i := range 3 {
			status, raw := payMyRenewal(t, a, invoice, *sub.PaymentMethodID, fmt.Sprintf("customer-decline-%d", i))
			require.Equal(t, http.StatusPaymentRequired, status, string(raw))
			require.Contains(t, string(raw), `"code":"card_declined"`)
		}
		retried, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: sub.ID, Kind: []string{"customer_retry"}})
		require.NoError(t, err)
		require.Len(t, retried.Items, 3)
		current = subscriptionState(t, a, sub.ID, "past_due")
		require.Equal(t, 1, current.Dunning.Attempts)
		require.Equal(t, due.Add(2*day), *current.Dunning.NextRetryAt)
		require.Equal(t, 4, *current.Dunning.RetriesLeft)
		require.Equal(t, due.Add(9*day), *current.Dunning.FinalRetryAt)
		status, raw := payMyRenewal(t, a, invoice, *sub.PaymentMethodID, "past-the-cap")
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"invoice_not_payable"`)
		a.clock.Advance(due.Add(2 * day).Sub(a.clock.Now()))
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.Dunning != nil && current.Dunning.Attempts == 2 &&
				current.Dunning.NextRetryAt != nil && current.Dunning.NextRetryAt.Equal(due.Add(4*day))
		}, 20*time.Second, 20*time.Millisecond)
	})
	t.Run("OR-J16-S51/a-merchants-customer-retry-cap-is-honoured", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			setting *int
			limit   int
		}{
			{"none", ptr(0), 0},
			{"default", nil, 3},
			{"raised", ptr(4), 4},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, p, sub := purchasedMembershipWith(t, 7*24, dunningPolicy(billing.DunningPolicy{CustomerRetries: tc.setting}))
				require.NotNil(t, sub.PaymentMethodID)
				p.core.SetDecline("4242", "incorrect_zip")
				a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				subscriptionState(t, a, sub.ID, "awaiting_method")
				// The case keeps the cap in force at its first decline.
				var frozen *int
				require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT (dunning_policy->>'customer_retries')::int FROM "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" WHERE id = $1",
					sub.ID.UUID()).Scan(&frozen))
				require.NotNil(t, frozen)
				require.Equal(t, tc.limit, *frozen)
				invoice := myRenewalInvoice(t, a, sub.ID)
				for i := range tc.limit {
					status, raw := payMyRenewal(t, a, invoice, *sub.PaymentMethodID, fmt.Sprintf("corrected-address-%d", i))
					require.Equal(t, http.StatusPaymentRequired, status, string(raw))
					require.Contains(t, string(raw), `"code":"card_declined"`)
				}
				retried, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: sub.ID, Kind: []string{"customer_retry"}})
				require.NoError(t, err)
				require.Len(t, retried.Items, tc.limit)
				creates := paymentIntentCreates(p)
				status, raw := payMyRenewal(t, a, invoice, *sub.PaymentMethodID, "past-the-cap")
				require.Equal(t, http.StatusConflict, status, string(raw))
				require.Contains(t, string(raw), `"code":"invoice_not_payable"`)
				require.Equal(t, creates, paymentIntentCreates(p), "past the cap the card never reaches the provider")
				subscriptionState(t, a, sub.ID, "awaiting_method")
				require.Len(t, p.effects(t), 1)
				// Another card is admitted whatever the cap.
				replacement := saveMethod(t, a, p, a.aliceToken, "5556")
				status, raw = payMyRenewal(t, a, invoice, replacement.ID, "replacement-card")
				require.Equal(t, http.StatusOK, status, string(raw))
				var paid billing.Invoice
				require.NoError(t, json.Unmarshal(raw, &paid))
				require.Equal(t, billing.InvoiceComplete, paid.Status)
				subscriptionState(t, a, sub.ID, "active")
				require.Len(t, p.effects(t), 2, "enrollment and the renewal on the new card")
			})
		}
	})
	t.Run("OR-J16-S52/a-dunning-policy-past-the-networks-reattempt-limits-is-refused", func(t *testing.T) {
		a, _ := newPSPWorld(t)
		tiers := func(monthly ...int) []billing.DunningTier {
			return []billing.DunningTier{{MaxCycleHours: 96}, {MaxCycleHours: 672, RetryAfterHours: []int{24, 48}}, {RetryAfterHours: monthly}}
		}
		daily := []int{24, 48, 72, 96, 120, 144, 168, 192, 216, 240}
		patch := func(policy *billing.DunningPolicy) (int, []byte) {
			body := map[string]any{"display_name": "Alpha"}
			if policy != nil {
				body["dunning_policy"] = policy
			}
			return a.request(t, http.MethodPatch, "/v1/merchant/configuration", a.alphaKey.Secret, body)
		}
		for _, tc := range []struct {
			name, refusal string
			policy        billing.DunningPolicy
		}{
			// 10 scheduled + 3 transient + 3 customer retries: 16 reattempts.
			{"thirty-days", "in 30 days", billing.DunningPolicy{Tiers: tiers(daily...), TransientRetryMinutes: ptr([]int{5, 30, 60}), CustomerRetries: ptr(3)}},
			// The first failure and 5 hourly retries, then 5 by the customer: 11 attempts in a day.
			{"one-day", "within 24 hours", billing.DunningPolicy{Tiers: tiers(1, 2, 3, 4, 5), TransientRetryMinutes: ptr([]int{}), CustomerRetries: ptr(5)}},
			{"cap", "customer_retries must be between 0 and 9", billing.DunningPolicy{CustomerRetries: ptr(10)}},
			{"negative", "customer_retries must be between 0 and 9", billing.DunningPolicy{CustomerRetries: ptr(-1)}},
		} {
			status, raw := patch(&tc.policy)
			require.Equal(t, http.StatusBadRequest, status, "%s: %s", tc.name, raw)
			require.Contains(t, string(raw), `"code":"invalid_param"`, tc.name)
			require.Contains(t, string(raw), tc.refusal, tc.name)
		}
		var state billing.MerchantConfigurationState
		status, raw := patch(nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &state))
		require.Nil(t, state.DunningPolicy, "a refused policy is not stored")
		// 10 scheduled + 2 transient + 3 customer retries: 15 reattempts, the limit.
		status, raw = patch(&billing.DunningPolicy{Tiers: tiers(daily...), TransientRetryMinutes: ptr([]int{5, 30}), CustomerRetries: ptr(3)})
		require.Equal(t, http.StatusOK, status, string(raw))
		state = billing.MerchantConfigurationState{}
		require.NoError(t, json.Unmarshal(raw, &state))
		require.NotNil(t, state.DunningPolicy)
		require.NotNil(t, state.DunningPolicy.CustomerRetries)
		require.Equal(t, 3, *state.DunningPolicy.CustomerRetries)
		require.Len(t, state.DunningPolicy.Tiers, 3)
		require.Equal(t, daily, state.DunningPolicy.Tiers[2].RetryAfterHours)
	})
	t.Run("OR-J16-S53/an-omitted-transient-ladder-is-the-built-in-one-and-an-empty-list-is-none", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			policy   billing.DunningPolicy
			recorded string
			next     time.Duration
			left     int
		}{
			// A try-again answer is retried five minutes on and spends no retry.
			{"omitted", billing.DunningPolicy{CustomerRetries: ptr(2)}, "[5, 30]", 5 * time.Minute, 3},
			// With no ladder it is an ordinary failure on the weekly schedule.
			{"empty", billing.DunningPolicy{CustomerRetries: ptr(2), TransientRetryMinutes: ptr([]int{})}, "[]", 24 * time.Hour, 2},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, p, sub := purchasedMembershipWith(t, 7*24, dunningPolicy(tc.policy))
				due := *sub.CurrentPeriodEndsAt
				p.core.SetDecline("4242", "try_again_later")
				a.clock.Advance(due.Sub(a.clock.Now()))
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.billing_pass", 2)
				declined := subscriptionState(t, a, sub.ID, "past_due")
				require.NotNil(t, declined.Dunning)
				require.Equal(t, 1, declined.Dunning.Attempts)
				require.Equal(t, due.Add(tc.next), *declined.Dunning.NextRetryAt)
				require.Equal(t, tc.left, *declined.Dunning.RetriesLeft)
				// The case records the ladder it runs under, the default written out.
				var recorded string
				require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT (dunning_policy->'transient_retry_minutes')::text FROM "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" WHERE id = $1",
					sub.ID.UUID()).Scan(&recorded))
				require.Equal(t, tc.recorded, recorded)
				require.Len(t, p.effects(t), 1)
			})
		}
	})
	t.Run("OR-J16-S54/a-declared-failure-inside-the-merchants-longer-schedule-stays-in-dunning", func(t *testing.T) {
		// A monthly renewal is retried up to 18 days after it falls due: a
		// 19-day window, where the built-in schedule's is 10 days.
		const day = 24
		a, p := newNMIWorld(t, dunningPolicy(billing.DunningPolicy{Tiers: []billing.DunningTier{
			{MaxCycleHours: 96}, {MaxCycleHours: 672, RetryAfterHours: []int{day, 2 * day}}, {RetryAfterHours: []int{2 * day, 6 * day, 12 * day, 18 * day}},
		}}))
		p.core.AddPlan(nmimock.Plan{ID: "monthly", Name: "Monthly", Amount: "10.00", Days: 30})
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-membership", DisplayName: "NMI membership", Entitlements: []string{"nmi:member"}})
		require.NoError(t, err)
		price, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-monthly", PSP: "nmi", PlanID: "monthly", Currency: "USD"})
		require.NoError(t, err)
		// NMI is still retrying a renewal that fell due fifteen days ago.
		now := a.clock.Now()
		due := now.Add(-15 * 24 * time.Hour)
		started := due.Add(-30 * 24 * time.Hour)
		vaultID := p.core.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
		vault := p.core.Vault(vaultID)
		scheduleID := p.core.AddSchedule(nmimock.Schedule{Vault: vaultID, Plan: "monthly", Amount: "10.00", Days: 30, Created: started, NextBilling: due, Order: "dunned-nmi-membership"})
		initial := p.core.AddScheduleSale(scheduleID, started)
		require.NoError(t, p.persist(t.Context()))
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		ref := billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vaultID, RailMethodRef: vault.BillingID}
		book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"},
			Customers:      []billing.DeclaredCustomer{{Customer: customer}},
			PaymentMethods: []billing.DeclaredPaymentMethod{{Customer: customer, Rail: "nmi", RailCustomerRef: vaultID, RailMethodRef: vault.BillingID, RecurringTransactionID: initial.TransactionID, CreatedAt: started}},
			Subscriptions: []billing.DeclaredSubscription{{SourceID: "dunned-nmi-membership", Customer: customer, Price: price.ID, Rail: "nmi", RailSubscriptionID: scheduleID,
				StartedAt: started, PaidThrough: &due, PaymentMethod: &ref, Dunning: &billing.DunningEvidence{Retries: 3, ScheduleLive: true},
				AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: scheduleID, Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: due}}},
			Transactions: []billing.DeclaredTransaction{{SourceID: "dunned-nmi-initial-receipt", RailSubscriptionID: scheduleID, TransactionID: initial.TransactionID, Type: "sale", Success: true,
				Amount: 10_000_000, Currency: "USD", OccurredAt: started, NextBillingAt: &due}},
		}
		imported, err := a.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, imported.Blocked, "%+v", imported.Reasons)
		subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, subs.Items, 1)
		dunned := subscriptionState(t, a, subs.Items[0].ID, "past_due")
		require.Equal(t, due, *dunned.CurrentPeriodEndsAt, "no period is granted")
	})
	t.Run("OR-J16-S55/a-declared-failure-past-its-own-cycles-window-is-not-dunned", func(t *testing.T) {
		// A weekly renewal is retried for 2 days: its window is 3 days, not the
		// 10 days of the longest (monthly) schedule.
		a, p := newNMIWorld(t)
		p.core.AddPlan(nmimock.Plan{ID: "weekly", Name: "Weekly", Amount: "10.00", Days: 7})
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-membership", DisplayName: "NMI membership", Entitlements: []string{"nmi:member"}})
		require.NoError(t, err)
		price, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-weekly", PSP: "nmi", PlanID: "weekly", Currency: "USD"})
		require.NoError(t, err)
		// The renewal fell due five days ago.
		now := a.clock.Now()
		due := now.Add(-5 * 24 * time.Hour)
		started := due.Add(-7 * 24 * time.Hour)
		vaultID := p.core.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
		vault := p.core.Vault(vaultID)
		scheduleID := p.core.AddSchedule(nmimock.Schedule{Vault: vaultID, Plan: "weekly", Amount: "10.00", Days: 7, Created: started, NextBilling: due, Order: "lapsed-nmi-membership"})
		initial := p.core.AddScheduleSale(scheduleID, started)
		require.NoError(t, p.persist(t.Context()))
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		ref := billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vaultID, RailMethodRef: vault.BillingID}
		book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"},
			Customers:      []billing.DeclaredCustomer{{Customer: customer}},
			PaymentMethods: []billing.DeclaredPaymentMethod{{Customer: customer, Rail: "nmi", RailCustomerRef: vaultID, RailMethodRef: vault.BillingID, RecurringTransactionID: initial.TransactionID, CreatedAt: started}},
			Subscriptions: []billing.DeclaredSubscription{{SourceID: "lapsed-nmi-membership", Customer: customer, Price: price.ID, Rail: "nmi", RailSubscriptionID: scheduleID,
				StartedAt: started, PaidThrough: &due, PaymentMethod: &ref, Dunning: &billing.DunningEvidence{Retries: 3, ScheduleLive: true},
				AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: scheduleID, Amount: 10_000_000, Currency: "USD", IntervalHours: 168, FirstDueAt: due}}},
			Transactions: []billing.DeclaredTransaction{{SourceID: "lapsed-nmi-initial-receipt", RailSubscriptionID: scheduleID, TransactionID: initial.TransactionID, Type: "sale", Success: true,
				Amount: 10_000_000, Currency: "USD", OccurredAt: started, NextBillingAt: &due}},
		}
		imported, err := a.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, imported.Blocked, "%+v", imported.Reasons)
		subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, subs.Items, 1)
		held := subscriptionState(t, a, subs.Items[0].ID, "unverified")
		require.Equal(t, due, *held.CurrentPeriodEndsAt, "no period is granted")
	})
}
