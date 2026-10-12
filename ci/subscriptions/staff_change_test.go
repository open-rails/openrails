//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payment"
)

// requireStaffSchedule: the staff change pending on sub keeps its reason and
// staff member, and the customer was told.
func (w *world) requireStaffSchedule(c *customer, sub billing.SubscriptionID, reason string) {
	w.t.Helper()
	var invoker, why string
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT coalesce(invoker, ''), coalesce(reason, '') FROM billing.scheduled_changes
		WHERE subscription_id = $1 AND status = 'scheduled'`), sub.UUID()).Scan(&invoker, &why))
	require.NotEmpty(w.t, invoker)
	require.Equal(w.t, reason, why)
	w.requireStaffNotice(c, "")
}

// requireStaffNotice: the customer's latest notice is a staff change that
// charged amount ("" for nothing).
func (w *world) requireStaffNotice(c *customer, amount string) {
	w.t.Helper()
	var message, charged string
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT data->>'message', coalesce(data->>'amount', '') FROM billing.notification_events
		WHERE customer_id = $1::uuid AND event_type = 'subscription_changed' ORDER BY created_at DESC, id DESC LIMIT 1`), c.id).Scan(&message, &charged))
	require.Equal(w.t, "Changed by support at your request.", message)
	require.Equal(w.t, amount, charged)
}

// Staff changes schedule the next renewal without charging now, even for an
// upgrade. Repeats keep one schedule; clearing it and scheduling again are safe.
func TestStaffSubscriptionChange(t *testing.T) {
	t.Parallel()
	forEachRail(t, func(t *testing.T, rail string) {
		w := newWorld(t)
		group := "g" + uuid.NewString()[:8]
		basic := w.tierPrice(group, 1, 1000, monthHours, false)
		pro := w.tierPrice(group, 2, 2000, monthHours, false)
		top := w.tierPrice(group, 3, 3000, monthHours, false)
		c, sub := w.engineMember(rail, embedded, basic)
		end := *w.subscription(embedded, sub).CurrentPeriodEndsAt
		w.advance(end.Sub(w.clock.Now()) - 360*time.Hour)
		charges := len(w.railLedger(rail))
		params := billing.ChangeSubscriptionParams{PriceID: priceRef(pro.ID), IdempotencyKey: "staff-" + uuid.NewString()}
		_, err := w.client[remote].ChangeSubscription(t.Context(), sub, params)
		requireCode(t, err, http.StatusBadRequest, billing.CodeInvalidParam)
		preview, err := w.client[remote].PreviewSubscriptionChange(t.Context(), sub, params)
		require.NoError(t, err)
		require.Equal(t, "period_end", preview.Effective)
		require.Zero(t, preview.AmountDueNow)
		require.EqualValues(t, 2000*10_000, preview.NextChargeAmount)
		require.True(t, end.Equal(*preview.NextChargeDate))
		params.Reason = "upgrade by phone"
		up, err := w.client[remote].ChangeSubscription(t.Context(), sub, params)
		require.NoError(t, err)
		require.Equal(t, "succeeded", up.Status)
		require.Equal(t, "period_end", up.Effective)
		require.Zero(t, up.AmountDueNow)
		require.Equal(t, sub, *up.SubscriptionID)
		require.Equal(t, basic.ID, w.subscription(remote, sub).PriceID)
		require.Equal(t, pro.ID, w.subscription(remote, sub).ScheduledChange.PriceID)
		w.requireStaffSchedule(c, sub, params.Reason)
		require.Len(t, w.railLedger(rail), charges, "a staff upgrade does not charge")
		require.True(t, c.entitled(basic.ent))
		require.False(t, c.entitled(pro.ent), "access changes only when renewal settles")
		again, err := w.client[embedded].ChangeSubscription(t.Context(), sub, params)
		require.NoError(t, err)
		require.Equal(t, up, again)
		var count int
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.scheduled_changes WHERE subscription_id=$1 AND status='scheduled'`), sub.UUID()).Scan(&count))
		require.Equal(t, 1, count, "a replay does not duplicate the schedule")
		_, err = w.client[remote].ChangeSubscription(t.Context(), sub, billing.ChangeSubscriptionParams{PriceID: priceRef(top.ID), Reason: "another change", IdempotencyKey: uuid.NewString()})
		requireCode(t, err, http.StatusConflict, billing.CodeSubscriptionChangeAlreadyScheduled)
		clear := billing.ChangeSubscriptionParams{PriceID: priceRef(basic.ID), Reason: "keep current plan", IdempotencyKey: uuid.NewString()}
		clearPreview, err := w.client[remote].PreviewSubscriptionChange(t.Context(), sub, clear)
		require.NoError(t, err)
		require.Zero(t, clearPreview.AmountDueNow)
		cleared, err := w.client[remote].ChangeSubscription(t.Context(), sub, clear)
		require.NoError(t, err)
		require.Equal(t, "now", cleared.Effective)
		require.Zero(t, cleared.AmountDueNow)
		require.Nil(t, w.subscription(remote, sub).ScheduledChange)
		require.Len(t, w.railLedger(rail), charges, "clearing and replay perform no charge")
		params.IdempotencyKey = uuid.NewString()
		_, err = w.client[remote].ChangeSubscription(t.Context(), sub, params)
		require.NoError(t, err)
		require.Len(t, w.railLedger(rail), charges, "rescheduling performs no charge")
		w.advanceHealthyTo(end.Add(time.Second))
		w.runRenewals()
		ledger := w.railLedger(rail)
		require.Len(t, ledger, charges+1, "only the automatic renewal charges")
		require.EqualValues(t, 2000, ledger[len(ledger)-1].Amount)
		current := w.subscription(remote, sub)
		require.Equal(t, pro.ID, current.PriceID)
		require.Nil(t, current.ScheduledChange)
		require.True(t, c.entitled(pro.ent))
		require.Empty(t, w.stripe.Unexpected())
		require.Empty(t, w.nmi.Unexpected())
	})
}

// A change back to what the subscription bills now cancels its pending
// change: nothing is charged, nothing stays pending, and the renewal bills
// the current price.
func TestSubscriptionChangeBack(t *testing.T) {
	t.Parallel()
	forEachRail(t, func(t *testing.T, rail string) {
		w := newWorld(t)
		group := "g" + uuid.NewString()[:8]
		basic := w.tierPrice(group, 1, 1000, monthHours, false)
		pro := w.tierPrice(group, 2, 2000, monthHours, false)
		c, sub := w.engineMember(rail, embedded, pro)
		end := *w.subscription(embedded, sub).CurrentPeriodEndsAt
		w.advance(time.Hour)
		charges := len(w.railLedger(rail))

		down, err := c.change(sub, billing.ChangeSubscriptionParams{PriceID: priceRef(basic.ID), IdempotencyKey: "down-" + uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, "period_end", down.Effective)
		require.NotNil(t, w.subscription(embedded, sub).ScheduledChange)

		preview, err := c.previewChange(sub, billing.ChangeSubscriptionParams{PriceID: priceRef(pro.ID)})
		require.NoError(t, err)
		require.Zero(t, preview.AmountDueNow)
		back, err := c.change(sub, billing.ChangeSubscriptionParams{PriceID: priceRef(pro.ID), IdempotencyKey: "back-" + uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, "succeeded", back.Status, "%+v", back)
		require.Zero(t, back.AmountDueNow)
		require.Equal(t, pro.ID, back.PriceID)
		require.Nil(t, w.subscription(remote, sub).ScheduledChange, "nothing is pending")
		_, err = c.change(sub, billing.ChangeSubscriptionParams{PriceID: priceRef(pro.ID), IdempotencyKey: "again-" + uuid.NewString()})
		requireCode(t, err, http.StatusConflict, billing.CodeResourceConflict)
		require.Len(t, w.railLedger(rail), charges, "nothing is charged")

		w.advanceHealthyTo(end.Add(time.Second))
		w.runRenewals()
		ledger := w.railLedger(rail)
		require.Len(t, ledger, charges+1)
		require.Equal(t, int64(2000), ledger[len(ledger)-1].Amount, fmt.Sprintf("%s: the renewal bills the current price", rail))
		require.Equal(t, pro.ID, w.subscription(embedded, sub).PriceID)
		require.True(t, c.entitled(pro.ent))
		require.Empty(t, w.stripe.Unexpected())
		require.Empty(t, w.nmi.Unexpected())
	})
}

// A source failure rolls back a no-charge staff schedule, so retry can capture
// the schedule and its fact together. Replaying the successful operation is silent.
func TestStaffScheduleNotificationRollback(t *testing.T) {
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	basic := w.tierPrice(group, 1, 1000, monthHours, false)
	pro := w.tierPrice(group, 2, 2000, monthHours, false)
	c, sub := w.engineMember("stripe", embedded, pro)
	ctx := t.Context()
	_, err := w.pool.Exec(ctx, w.q(`CREATE FUNCTION billing.reject_staff_notice() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN IF NEW.event_type = 'subscription_changed' THEN RAISE EXCEPTION 'staff notice unavailable'; END IF; RETURN NEW; END $$;
	CREATE TRIGGER reject_staff_notice BEFORE INSERT ON billing.notification_events FOR EACH ROW EXECUTE FUNCTION billing.reject_staff_notice();`))
	require.NoError(t, err)
	params := billing.ChangeSubscriptionParams{PriceID: priceRef(basic.ID), Reason: "customer requested downgrade", IdempotencyKey: "staff-" + uuid.NewString()}
	_, err = w.client[embedded].ChangeSubscription(ctx, sub, params)
	require.Error(t, err)
	require.Nil(t, w.subscription(embedded, sub).ScheduledChange, "a schedule cannot commit without its fact")
	_, err = w.pool.Exec(ctx, w.q(`DROP TRIGGER reject_staff_notice ON billing.notification_events`))
	require.NoError(t, err)
	params.IdempotencyKey = "staff-" + uuid.NewString()
	changed, err := w.client[embedded].ChangeSubscription(ctx, sub, params)
	require.NoError(t, err)
	require.Equal(t, "succeeded", changed.Status)
	w.requireStaffSchedule(c, sub, params.Reason)
	count := func() int {
		var n int
		require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.notification_events WHERE customer_id = $1 AND event_type = 'subscription_changed'`), c.cid().UUID()).Scan(&n))
		return n
	}
	require.Equal(t, 1, count())
	_, err = w.client[embedded].ChangeSubscription(ctx, sub, params)
	require.NoError(t, err)
	require.Equal(t, 1, count())
	params.PriceID, params.IdempotencyKey = priceRef(pro.ID), "staff-clear-"+uuid.NewString()
	_, err = w.client[embedded].ChangeSubscription(ctx, sub, params)
	require.NoError(t, err)
	require.Nil(t, w.subscription(embedded, sub).ScheduledChange)
	require.Equal(t, 2, count(), "canceling the schedule is a distinct fact")
}

// Provider completion also captures staff context from its frozen operation;
// retries neither repeat the provider mutation nor publish a second fact.
func TestProviderStaffDowngradeNotification(t *testing.T) {
	w := newWorld(t)
	w.armDestructive()
	group := "g" + uuid.NewString()[:8]
	pro := w.tierPrice(group, 2, 1999, monthHours, true)
	basic := w.tierPrice(group, 1, 499, monthHours, true)
	l := w.legacyOnTier(embedded, pro, 1999, monthHours, 10*day)
	params := billing.ChangeSubscriptionParams{PriceID: priceRef(basic.ID), Reason: "requested provider downgrade", IdempotencyKey: "staff-provider-" + uuid.NewString()}
	changed, err := w.client[embedded].ChangeSubscription(t.Context(), l.sub, params)
	require.NoError(t, err)
	require.Equal(t, "succeeded", changed.Status)
	w.requireStaffNotice(l.c, "")
	_, err = w.client[embedded].ChangeSubscription(t.Context(), l.sub, params)
	require.NoError(t, err)
	var count int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.notification_events WHERE customer_id = $1 AND event_type = 'subscription_changed'`), l.c.cid().UUID()).Scan(&count))
	require.Equal(t, 1, count)
	require.Len(t, w.nmi.ScheduleUpdates(l.railSub), 1)
	require.Equal(t, basic.ID, w.subscription(embedded, l.sub).ScheduledChange.PriceID)
}

// A staff retry of a historical pending operation is observation only. The
// customer may still resume the same accepted operation.
func TestStaffSubscriptionReplayCannotResumeACharge(t *testing.T) {
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	basic := w.tierPrice(group, 1, 1000, monthHours, false)
	pro := w.tierPrice(group, 2, 2000, monthHours, false)
	customer, sub := w.engineMember("stripe", embedded, basic)
	runtime := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	key := "historical-" + uuid.NewString()
	var operation gen.BillingProviderIntent
	require.NoError(t, runtime.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		current, err := runtime.SubscriptionService.GetByID(ctx, sub.UUID())
		if err != nil {
			return err
		}
		price, err := runtime.PriceService.GetCurrentByKey(ctx, current.MerchantID, pro.ProductID.UUID(), pro.Key)
		if err != nil {
			return err
		}
		payload := payment.StripeTierChangePayload{UserID: customer.id, SubscriptionID: sub.UUID(), RequestedPrice: pro.ID.String(), PriceID: price.ID, OldPriceID: current.PriceID,
			StripeSubscriptionID: "legacy-subscription", StripeItemID: "legacy-item", OldStripePriceID: "legacy-basic", StripePriceID: "legacy-pro", Action: "upgrade", PaymentBehavior: "error_if_incomplete",
			Currency: "USD", RecurringAmount: 20_000_000, AmountDueNow: 15_000_000, PeriodStart: *current.CurrentPeriodStartsAt, PeriodEnd: *current.CurrentPeriodEndsAt}
		operation, err = intents.NewStore(runtime.DB).Enqueue(ctx, intents.EnqueueParams{MerchantID: current.MerchantID, Provider: "stripe", PspID: current.PspID, IntentType: payment.TypeStripeTierChange,
			SubscriptionID: &current.ID, PriceID: &price.ID, Payload: payload, IdempotencyKey: "tier_change:" + customer.id + ":" + key, NextAttemptAt: time.Now().Add(24 * time.Hour), Origin: intents.OriginAdmin})
		return err
	}))
	recorder := &tierReplayExecutor{operation: operation}
	original := runtime.PurchaseService.Intents
	runtime.PurchaseService.Intents = recorder
	defer func() { runtime.PurchaseService.Intents = original }()
	before := len(w.railLedger("stripe"))
	params := billing.ChangeSubscriptionParams{PriceID: priceRef(pro.ID), IdempotencyKey: key, Reason: "inspect previous change"}
	result, err := w.client[remote].ChangeSubscription(t.Context(), sub, params)
	require.NoError(t, err)
	require.Equal(t, "processing", result.Status)
	require.Zero(t, recorder.calls.Load(), "staff cannot resume a pending saved-card charge")
	require.Len(t, w.railLedger("stripe"), before)
	params.Reason = "" // customer requests have no staff-only reason field
	result, err = customer.change(sub, params)
	require.NoError(t, err)
	require.Equal(t, "processing", result.Status)
	require.EqualValues(t, 1, recorder.calls.Load(), "the customer's retry still resumes its accepted operation")
}

type tierReplayExecutor struct {
	operation gen.BillingProviderIntent
	calls     atomic.Int32
}

func (r *tierReplayExecutor) EnqueueAndExecute(context.Context, intents.EnqueueParams) (gen.BillingProviderIntent, error) {
	r.calls.Add(1)
	return r.operation, nil
}
func (r *tierReplayExecutor) EnqueueOwnedAndExecute(ctx context.Context, p intents.EnqueueParams, owns func(gen.BillingProviderIntent) error) (gen.BillingProviderIntent, error) {
	if err := owns(r.operation); err != nil {
		return gen.BillingProviderIntent{}, err
	}
	return r.EnqueueAndExecute(ctx, p)
}
