//go:build integration

package critical_test

import (
	"context"
	"net/http"
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
}
