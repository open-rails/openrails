//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// dunningPolicy declares alpha's dunning policy.
func dunningPolicy(policy billing.DunningPolicy) func(*world) {
	return func(w *world) {
		m := w.cfg.Merchants["alpha"]
		m.MerchantSettings.DunningPolicy = &policy
		w.cfg.Merchants["alpha"] = m
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
			var declined *billing.Subscription
			require.Eventually(t, func() bool {
				var err error
				declined, err = a.alpha.GetSubscription(t.Context(), sub.ID)
				return err == nil && declined.Dunning != nil && declined.Dunning.Attempts == failures
			}, 20*time.Second, 20*time.Millisecond, "failure %d must be counted", failures)
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
				order := myRenewalOrder(t, a, sub.ID)
				for i := range tc.limit {
					status, raw := payMyRenewal(t, a, order, *sub.PaymentMethodID, fmt.Sprintf("corrected-address-%d", i))
					require.Equal(t, http.StatusPaymentRequired, status, string(raw))
					require.Contains(t, string(raw), `"code":"card_declined"`)
				}
				retried, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: sub.ID, Kind: []string{"customer_retry"}})
				require.NoError(t, err)
				require.Len(t, retried.Items, tc.limit)
				creates := paymentIntentCreates(p)
				status, raw := payMyRenewal(t, a, order, *sub.PaymentMethodID, "past-the-cap")
				require.Equal(t, http.StatusConflict, status, string(raw))
				require.Contains(t, string(raw), `"code":"order_not_payable"`)
				require.Equal(t, creates, paymentIntentCreates(p), "past the cap the card never reaches the provider")
				subscriptionState(t, a, sub.ID, "awaiting_method")
				require.Len(t, p.effects(t), 1)
				// Another card is admitted whatever the cap.
				replacement := saveMethod(t, a, p, a.aliceToken, "5556")
				status, raw = payMyRenewal(t, a, order, replacement.ID, "replacement-card")
				require.Equal(t, http.StatusOK, status, string(raw))
				var paid billing.Order
				require.NoError(t, json.Unmarshal(raw, &paid))
				require.Equal(t, billing.OrderComplete, paid.Status)
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
}
