//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestCancellationNotificationReplayAndRollback(t *testing.T) {
	w := newWorld(t)
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()
	w.refreshProviders()
	w.settleCollectionScans()
	rt := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	subID := l.sub.UUID()
	params := &subscriptions.CancelMembershipParams{SubscriptionID: &subID, CancelType: models.CancelTypeUser}
	consumer, err := w.client[embedded].NotificationEvents("cancellation-regression")
	require.NoError(t, err)
	require.NoError(t, consumer.Register(ctx))
	notices := func() []billing.NotificationEvent {
		t.Helper()
		events, err := consumer.Pending(ctx, 100)
		require.NoError(t, err)
		var ended []billing.NotificationEvent
		for _, event := range events {
			if event.CustomerID == l.c.cid() && (event.EventType == string(models.NotificationPremiumEnded) || event.EventType == string(models.NotificationSubscriptionCanceled)) {
				ended = append(ended, event)
			}
		}
		return ended
	}
	before := w.subscription(embedded, l.sub)
	rollback := errors.New("later domain write failed")
	var rolledBackID uuid.UUID
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		result, err := rt.SubscriptionLifecycleService.CancelMembershipTx(ctx, rt.DB.NewWithPgxTx(tx), params)
		if err != nil {
			return err
		}
		require.Len(t, result.Notifications, 1)
		rolledBackID = result.Notifications[0].ID
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	require.Equal(t, before.Status, w.subscription(embedded, l.sub).Status)
	require.Empty(t, notices(), "the cancellation and its fact roll back together")

	w.clock.Advance(time.Minute)
	require.NoError(t, rt.SubscriptionLifecycleService.CancelMembership(ctx, params))
	first := notices()
	require.Len(t, first, 1)
	require.Equal(t, string(models.NotificationSubscriptionCanceled), first[0].EventType)
	var stopped billing.NotificationData
	require.NoError(t, json.Unmarshal(first[0].Data, &stopped))
	require.False(t, stopped.AccessEnded)
	require.Equal(t, before.CurrentPeriodEndsAt, stopped.EndedAt)
	require.Equal(t, billing.NotificationID(rolledBackID), first[0].ID, "the same transition retains its identity after rollback")
	canceled := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionCanceled, canceled.Status)
	require.Equal(t, before.CurrentPeriodEndsAt, canceled.CurrentPeriodEndsAt, "period-end cancellation keeps paid access")

	w.clock.Advance(time.Minute)
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		result, err := rt.SubscriptionLifecycleService.CancelMembershipTx(ctx, rt.DB.NewWithPgxTx(tx), params)
		if err == nil {
			require.Empty(t, result.Notifications, "a replay with no lifecycle transition emits no fact")
		}
		return err
	})
	require.NoError(t, err)
	require.Equal(t, first, notices())
	require.Equal(t, canceled.CanceledAt, w.subscription(embedded, l.sub).CanceledAt)

	// An immediate merchant revoke of that scheduled cancellation is a real
	// transition, so its distinct fact must still be published.
	params.CancelType, params.RevokeAccess = models.CancelTypeMerchant, true
	require.NoError(t, rt.SubscriptionLifecycleService.CancelMembership(ctx, params))
	after := notices()
	require.Len(t, after, 2)
	require.Equal(t, string(models.NotificationPremiumEnded), after[1].EventType)
	var ended billing.NotificationData
	require.NoError(t, json.Unmarshal(after[1].Data, &ended))
	require.True(t, ended.AccessEnded)
	require.NotEqual(t, after[0].ID, after[1].ID)
	require.True(t, w.subscription(embedded, l.sub).CurrentPeriodEndsAt.Before(*before.CurrentPeriodEndsAt))

	w.clock.Advance(time.Minute)
	require.NoError(t, rt.SubscriptionLifecycleService.CancelMembership(ctx, params))
	require.Equal(t, after, notices(), "replaying the immediate revoke also emits nothing")
}

func TestCanceledMembershipNotifiesWhenPaidAccessActuallyExpires(t *testing.T) {
	w := newWorld(t)
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()
	w.refreshProviders()
	w.settleCollectionScans()
	rt := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	subID := l.sub.UUID()
	require.NoError(t, rt.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{SubscriptionID: &subID, CancelType: models.CancelTypeUser}))
	require.True(t, l.c.hasNotification("subscription_canceled"))
	require.False(t, l.c.hasNotification("premium_ended"))
	w.clock.Advance(l.periodEnd().Sub(w.clock.Now()) + time.Second)
	w.converge()
	data := l.c.notificationData("premium_ended")
	require.Equal(t, true, data["access_ended"])
	require.Equal(t, "access_ended", data["reason"])
	require.NotEmpty(t, data["ended_at"])
}

func TestRevokedSubscriptionDoesNotEndIndependentAccess(t *testing.T) {
	w := newWorld(t)
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()
	w.refreshProviders()
	w.settleCollectionScans()
	sub := w.subscription(embedded, l.sub)
	_, err := w.client[embedded].CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{
		{CustomerID: l.c.cid(), ProductID: sub.ProductID}, // independent, indefinite access
	}})
	require.NoError(t, err)
	rt := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	subID := l.sub.UUID()
	require.NoError(t, rt.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{SubscriptionID: &subID, CancelType: models.CancelTypeMerchant, RevokeAccess: true}))
	require.True(t, l.c.entitled(l.ent))
	require.False(t, l.c.hasNotification("premium_ended"))
	stopped := l.c.notificationData("subscription_canceled")
	require.NotContains(t, stopped, "ended_at", "indefinite access must not acquire the subscription's old expiry")
}

func TestFutureGrantDoesNotHideMembershipExpiry(t *testing.T) {
	w := newWorld(t)
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()
	w.refreshProviders()
	w.settleCollectionScans()
	sub := w.subscription(embedded, l.sub)
	grantEnd := w.clock.Now().Add(2 * time.Hour)
	granted := l.c.grant(sub.ProductID, nil, &grantEnd)
	// Seed a future entitlement interval: the current access really has a gap.
	_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.product_access SET starts_at = $1 WHERE id = $2`), w.clock.Now().Add(time.Hour), granted.ID.UUID())
	require.NoError(t, err)
	rt := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	subID := l.sub.UUID()
	require.NoError(t, rt.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{SubscriptionID: &subID, CancelType: models.CancelTypeMerchant, RevokeAccess: true}))
	require.False(t, l.c.entitled(l.ent))
	ended := l.c.notificationData("premium_ended")
	require.Equal(t, true, ended["access_ended"])
	require.Equal(t, w.clock.Now().UTC().Format(time.RFC3339Nano), ended["ended_at"])
}

func TestCancellationNotificationFailureRollsBack(t *testing.T) {
	w := newWorld(t)
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()
	w.refreshProviders()
	w.settleCollectionScans()
	rt := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	_, err := w.pool.Exec(ctx, w.q(`CREATE FUNCTION billing.reject_cancel_notice() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN IF NEW.event_type = 'premium_ended' THEN RAISE EXCEPTION 'cancel notice unavailable'; END IF; RETURN NEW; END $$;
	CREATE TRIGGER reject_cancel_notice BEFORE INSERT ON billing.notification_events FOR EACH ROW EXECUTE FUNCTION billing.reject_cancel_notice();`))
	require.NoError(t, err)
	subID := l.sub.UUID()
	params := &subscriptions.CancelMembershipParams{SubscriptionID: &subID, CancelType: models.CancelTypeMerchant, RevokeAccess: true}
	var publicationErr error
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, publicationErr = rt.SubscriptionLifecycleService.CancelMembershipTx(ctx, rt.DB.NewWithPgxTx(tx), params)
		return publicationErr
	})
	require.ErrorContains(t, publicationErr, "cancel notice unavailable", "the producer returns the failed fact write")
	require.Error(t, err)
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status)
	require.True(t, l.c.entitled(l.ent), "a failed notification write cannot commit the access revoke")
}
