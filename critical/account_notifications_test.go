//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/helpers/notify"
	notifypg "github.com/open-rails/helpers/notify/postgres"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func accountInbox(t *testing.T, destination **notifypg.Store) func(*world) {
	t.Helper()
	return func(w *world) {
		schema := w.schema + "_inbox"
		_, err := w.pool.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := w.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			require.NoError(t, err)
		})
		require.NoError(t, notifypg.Migrate(t.Context(), w.pool, schema))
		*destination, err = notifypg.New(w.pool, notifypg.Config{Schema: schema, Scope: "accounts"})
		require.NoError(t, err)
		w.deps.Notifications = &openrails.NotificationChannel{Sink: *destination, Recipient: func(_ context.Context, source openrails.NotificationRecipient) (string, bool, error) {
			return source.CustomerID.UUID().String(), source.Issuer == "" || source.Issuer == authtest.Issuer, nil
		}}
	}
}

func accountNotices(t *testing.T, store *notifypg.Store, user string) []notify.Item {
	t.Helper()
	page, err := store.List(t.Context(), notify.Recipient{Audience: notify.AudienceUser, ID: user}, notify.ListOptions{})
	require.NoError(t, err)
	return page.Items
}

func TestAccountNotificationChannel(t *testing.T) {
	t.Run("declined-renewal-publishes-once-and-paid-recovery-withdraws-unread", func(t *testing.T) {
		var inbox *notifypg.Store
		a, p, sub := purchasedMembershipWith(t, 7*24, accountInbox(t, &inbox))
		require.Empty(t, accountNotices(t, inbox, a.alice.ID), "routine enrollment is not an inbox message")
		due := *sub.CurrentPeriodEndsAt
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 2)
		subscriptionState(t, a, sub.ID, "past_due")
		var item notify.Item
		require.Eventually(t, func() bool {
			items := accountNotices(t, inbox, a.alice.ID)
			if len(items) == 1 {
				item = items[0]
				return true
			}
			return false
		}, 20*time.Second, 20*time.Millisecond)
		require.Equal(t, "subscription:"+sub.ID.UUID().String()+":payment_method_failed", item.Origin.ID)
		require.Equal(t, a.alpha.MerchantID().String(), item.Origin.Scope)
		require.Equal(t, "payment_method_failed", item.Kind)
		require.Nil(t, item.ReadAt)
		require.Empty(t, accountNotices(t, inbox, a.bob.ID))
		runRegistered(t, fleet, "openrails.dunning", 2)
		items := accountNotices(t, inbox, a.alice.ID)
		require.Len(t, items, 1)
		require.Equal(t, item.ID, items[0].ID, "retry before the due time must not duplicate the receipt")
		adoption, err := a.local.ExportNotificationAdoption(t.Context())
		require.NoError(t, err)
		require.Len(t, adoption.Adoptions, 1)
		require.Equal(t, item.Origin, adoption.Adoptions[0].Current.Origin)
		require.False(t, adoption.Adoptions[0].Current.Withdrawn)
		require.Len(t, adoption.Adoptions[0].Previous, 1)
		repeated, err := a.local.ExportNotificationAdoption(t.Context())
		require.NoError(t, err)
		require.Equal(t, adoption, repeated, "stopped source export must not manufacture new revisions")
		p.core.SetDecline("4242", "")
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.dunning", 2)
		renewalSettled(t, a, p, sub, due)
		require.Eventually(t, func() bool { return len(accountNotices(t, inbox, a.alice.ID)) == 0 }, 20*time.Second, 20*time.Millisecond)
		_, err = inbox.Get(t.Context(), notify.Recipient{Audience: notify.AudienceUser, ID: a.alice.ID}, item.ID)
		require.ErrorIs(t, err, notify.ErrNotFound)
		facts, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment_method_failed"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, facts.Events, 1, "withdrawing the inbox item must preserve the general billing fact")
		require.Equal(t, facts.Events[0].ID, adoption.Adoptions[0].Previous[0].ID)
		resolved, err := a.local.ExportNotificationAdoption(t.Context())
		require.NoError(t, err)
		require.Len(t, resolved.Adoptions, 1)
		require.True(t, resolved.Adoptions[0].Current.Withdrawn)
		require.Greater(t, resolved.Adoptions[0].Current.Revision, adoption.Adoptions[0].Current.Revision)
		require.NoError(t, a.local.RestoreNotificationOrdering(t.Context(), resolved))
		require.NoError(t, a.local.RestoreNotificationOrdering(t.Context(), resolved), "identical source snapshot replay is safe")
		require.Error(t, a.local.RestoreNotificationOrdering(t.Context(), adoption), "old ordering cannot replace a newer resolution")
		afterRestore, err := a.local.ExportNotificationAdoption(t.Context())
		require.NoError(t, err)
		require.Equal(t, resolved, afterRestore)
	})

	t.Run("replacement-method-withdraws-the-method-request-before-payment", func(t *testing.T) {
		var inbox *notifypg.Store
		a, p, sub := purchasedMembershipWith(t, 7*24, accountInbox(t, &inbox))
		p.core.SetDecline("4242", "lost_card")
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 2)
		subscriptionState(t, a, sub.ID, "awaiting_method")
		require.Eventually(t, func() bool { return len(accountNotices(t, inbox, a.alice.ID)) == 1 }, 20*time.Second, 20*time.Millisecond)
		require.Equal(t, "payment_method_update_required", accountNotices(t, inbox, a.alice.ID)[0].Kind)
		method := saveMethod(t, a, p, a.aliceToken, "5556")
		status, raw := a.request(t, http.MethodPut, "/v1/me/subscriptions/"+sub.ID.String()+"/payment-method", a.aliceToken, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &method.ID})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Empty(t, accountNotices(t, inbox, a.alice.ID))
	})

	for _, tc := range []struct {
		name, decline, state, kind string
		expire                     bool
	}{
		{"failed-renewal", "insufficient_funds", "past_due", "payment_method_failed", false},
		{"invalid-method", "lost_card", "awaiting_method", "payment_method_update_required", false},
		{"expired-method-deadline", "lost_card", "awaiting_method", "payment_method_update_required", true},
	} {
		t.Run("cancellation-withdraws-obsolete-payment-request/"+tc.name, func(t *testing.T) {
			var inbox *notifypg.Store
			a, p, sub := purchasedMembershipWith(t, 7*24, accountInbox(t, &inbox))
			p.core.SetDecline("4242", tc.decline)
			a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
			fleet := startFleet(t, a.world)
			runRegistered(t, fleet, "openrails.dunning", 2)
			waiting := subscriptionState(t, a, sub.ID, tc.state)
			items := accountNotices(t, inbox, a.alice.ID)
			require.Len(t, items, 1)
			require.Equal(t, tc.kind, items[0].Kind)
			require.Nil(t, items[0].ReadAt)
			if tc.expire {
				enableLifecycleMaintenance(t, a)
				require.NotNil(t, waiting.Dunning)
				require.NotNil(t, waiting.Dunning.FinalRetryAt)
				a.clock.Advance(waiting.Dunning.FinalRetryAt.Sub(a.clock.Now()) + time.Microsecond)
				runRegistered(t, fleet, "openrails.dunning", 2)
			} else {
				status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Stop renewal after the failed charge"})
				require.Equal(t, http.StatusOK, status, string(raw))
			}
			subscriptionState(t, a, sub.ID, "canceled")
			_, err := inbox.Get(t.Context(), notify.Recipient{Audience: notify.AudienceUser, ID: a.alice.ID}, items[0].ID)
			require.ErrorIs(t, err, notify.ErrNotFound, "an ended renewal must not keep asking the customer to repair its payment")
			facts, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{tc.kind}, Limit: 100})
			require.NoError(t, err)
			require.Len(t, facts.Events, 1)
			adoption, err := a.local.ExportNotificationAdoption(t.Context())
			require.NoError(t, err)
			found := false
			for _, adopted := range adoption.Adoptions {
				if adopted.Current.Kind == tc.kind {
					found = true
					require.True(t, adopted.Current.Withdrawn, "migration must not revive a canceled request for an unrepaired method")
				}
			}
			require.True(t, found, "source export must account for the retained payment fact")
		})
	}

	t.Run("held-collection-does-not-resolve-an-existing-payment-request", func(t *testing.T) {
		var inbox *notifypg.Store
		a, p, sub := purchasedMembershipWith(t, 7*24, accountInbox(t, &inbox))
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 2)
		subscriptionState(t, a, sub.ID, "past_due")
		// The operator has not armed terminal lifecycle actions. The next
		// nonrecoverable decline is held for verification, not paid or canceled.
		p.core.SetDecline("4242", "stolen_card")
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.dunning", 2)
		require.Eventually(t, func() bool {
			findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.terminal_outcome.held"})
			return err == nil && len(findings.Items) == 1
		}, 20*time.Second, 20*time.Millisecond, "the new provider outcome must commit before adoption")
		subscriptionState(t, a, sub.ID, "past_due")
		adoption, err := a.local.ExportNotificationAdoption(t.Context())
		require.NoError(t, err)
		require.Len(t, adoption.Adoptions, 1)
		require.False(t, adoption.Adoptions[0].Current.Withdrawn, "uncertain provider state is not recovery evidence")
		require.Len(t, accountNotices(t, inbox, a.alice.ID), 1)
	})
}

func TestOrderNotificationRecovery(t *testing.T) {
	for _, disposition := range []string{"partial-then-paid", "waived", "uncollectible", "funded-credit"} {
		t.Run(disposition, func(t *testing.T) {
			var inbox *notifypg.Store
			a, p, customer := usageBillingWorld(t, true, false, accountInbox(t, &inbox))
			p.core.SetDecline("4242", "202")
			oneUsage(t, a.alpha, usageInput(customer, "declined-usage", 3))
			a.clock.Advance(time.Hour) // The threshold order needs a nonzero billing period.
			fleet := startFleet(t, a.world)
			runUsageDue(t, fleet)
			require.Eventually(t, func() bool { return len(accountNotices(t, inbox, a.alice.ID)) == 1 }, 20*time.Second, 20*time.Millisecond)
			item := accountNotices(t, inbox, a.alice.ID)[0]
			order := oneUsageOrder(t, a, customer)
			require.Equal(t, "order:"+order.ID.UUID().String()+":"+item.Kind, item.Origin.ID)
			require.Nil(t, item.ReadAt)
			var err error
			switch disposition {
			case "partial-then-paid":
				_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 10_000_000, TransactionID: "bank-part-one"})
				require.NoError(t, err)
				require.Len(t, accountNotices(t, inbox, a.alice.ID), 1, "partial payment leaves an unresolved amount")
				_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 20_000_000, TransactionID: "bank-part-two"})
			case "waived":
				_, err = a.alpha.WaiveOrderBalance(t.Context(), order.ID)
			case "uncollectible":
				_, err = a.alpha.MarkOrderUncollectible(t.Context(), order.ID)
			case "funded-credit":
				_, err = a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 30_000_000, Source: "notification-test", SourceID: "repay-usage"}})
			}
			require.NoError(t, err)
			after := oneUsageOrder(t, a, customer)
			if disposition == "uncollectible" {
				require.Equal(t, billing.OrderUncollectible, after.Status)
				require.EqualValues(t, 30_000_000, after.Usage.AmountDue)
				require.Len(t, accountNotices(t, inbox, a.alice.ID), 1, "stopped collection does not erase debt")
			} else {
				expected := billing.OrderComplete
				if disposition == "waived" {
					expected = billing.OrderVoid
				}
				require.Equal(t, expected, after.Status)
				require.Zero(t, after.Usage.AmountDue)
				_, err = inbox.Get(t.Context(), notify.Recipient{Audience: notify.AudienceUser, ID: a.alice.ID}, item.ID)
				require.ErrorIs(t, err, notify.ErrNotFound, "settled or waived debt must retire the request")
			}
			facts, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{item.Kind}, Limit: 100})
			require.NoError(t, err)
			require.Len(t, facts.Events, 1, "resolution preserves the declined collection fact")
			p.money(t, 0, 0)
		})
	}
	t.Run("renewal-authentication-request-is-withdrawn-after-confirmation", func(t *testing.T) {
		var inbox *notifypg.Store
		a, p, sub := purchasedMembershipWith(t, 7*24, accountInbox(t, &inbox))
		p.core.SetDecline("4242", "auth")
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 2)
		require.Eventually(t, func() bool { return len(accountNotices(t, inbox, a.alice.ID)) == 1 }, 20*time.Second, 20*time.Millisecond)
		item := accountNotices(t, inbox, a.alice.ID)[0]
		require.Equal(t, "payment_authentication_required", item.Kind)
		var data billing.NotificationData
		require.NoError(t, json.Unmarshal(item.Data, &data))
		status, raw := a.request(t, http.MethodGet, "/v1/me/orders/"+data.OrderID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var waiting billing.Order
		require.NoError(t, json.Unmarshal(raw, &waiting))
		require.NotNil(t, waiting.Payment.NextAction)
		intent, _, ok := strings.Cut(waiting.Payment.NextAction.Payload["client_secret"], "_secret_")
		require.True(t, ok)
		require.True(t, p.core.Authenticate(intent))
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders/"+waiting.ID.String()+"/confirm", a.aliceToken, map[string]any{})
		require.Equal(t, http.StatusOK, status, string(raw))
		var paid billing.Order
		require.NoError(t, json.Unmarshal(raw, &paid))
		require.Equal(t, billing.OrderComplete, paid.Status)
		require.Empty(t, accountNotices(t, inbox, a.alice.ID))
		require.Len(t, p.effects(t), 2, "only enrollment and confirmed renewal charge money")
	})
}
