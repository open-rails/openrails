//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// requireMerchantInitiated: the last charge cited the card's agreement as a
// merchant-initiated charge and stored nothing anew.
func (w *world) requireMerchantInitiated(rail string, charged ledgerEntry) {
	w.t.Helper()
	if rail == "stripe" {
		require.Equal(w.t, "true", w.stripe.PaymentIntent(charged.ID)["metadata"].(map[string]string)["openrails_merchant_initiated"])
		require.Empty(w.t, w.stripe.PaymentIntent(charged.ID)["setup_future_usage"], "an off-session charge saves nothing")
		return
	}
	sale := w.nmi.LastSale()
	require.Equal(w.t, []string{"merchant", "used"}, []string{sale.InitiatedBy, sale.Indicator})
	require.NotEmpty(w.t, sale.Initial, "the charge names the card's agreement")
}

// requireStaffCharge: the staff change's charge of cents on sub keeps its
// reason and staff member, its operation is the staff's, and the customer
// was told.
func (w *world) requireStaffCharge(c *customer, sub billing.SubscriptionID, cents int64, reason string) {
	w.t.Helper()
	var paid *billing.Payment
	for _, p := range w.payments(embedded, c.id) { // newest first
		if p.Amount == cents*10_000 {
			paid = &p
			break
		}
	}
	require.NotNil(w.t, paid, "the staff charge is a payment")
	require.NotNil(w.t, paid.Reason)
	require.Equal(w.t, reason, *paid.Reason)
	var changedBy, invoker string
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT metadata->>'changed_by', coalesce(metadata->>'staff_invoker', '') FROM billing.payments WHERE id = $1`), paid.ID.UUID()).Scan(&changedBy, &invoker))
	require.Equal(w.t, "staff", changedBy)
	require.NotEmpty(w.t, invoker)
	var origin, actor, why string
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT origin, coalesce(actor, ''), coalesce(origin_reason, '') FROM billing.provider_intents
		WHERE subscription_id = $1 AND intent_type = 'initial_membership' ORDER BY created_at DESC LIMIT 1`), sub.UUID()).Scan(&origin, &actor, &why))
	require.Equal(w.t, []string{"admin", invoker}, []string{origin, actor})
	require.True(w.t, strings.HasSuffix(why, ": "+reason), why)
	w.requireStaffNotice(c, strconv.FormatInt(cents*10_000, 10))
}

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
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT data->>'message', coalesce(data->>'amount', '') FROM billing.notifications
		WHERE customer_id = $1::uuid AND event_type = 'subscription_changed' ORDER BY created_at DESC, id DESC LIMIT 1`), c.id).Scan(&message, &charged))
	require.Equal(w.t, "Changed by support at your request.", message)
	require.Equal(w.t, amount, charged)
}

// Staff change a subscription at the customer's request as the customer
// would: an upgrade is charged now, merchant-initiated under the card's
// recurring agreement, with the reason and staff member kept and the customer
// told; a downgrade waits for the renewal; a change without a reason, or a
// charge on a card without an active agreement, is refused.
func TestStaffSubscriptionChange(t *testing.T) {
	t.Parallel()
	forEachRail(t, func(t *testing.T, rail string) {
		w := newWorld(t)
		group := "g" + uuid.NewString()[:8]
		basic := w.tierPrice(group, 1, 1000, monthHours, false)
		pro := w.tierPrice(group, 2, 2000, monthHours, false)
		top := w.tierPrice(group, 3, 3000, monthHours, false)
		c, sub := w.engineMember(rail, embedded, basic)
		w.advance(w.subscription(embedded, sub).CurrentPeriodEndsAt.Sub(w.clock.Now()) - 360*time.Hour)
		charges := len(w.railLedger(rail))

		_, err := w.client[remote].ChangeSubscription(t.Context(), sub, billing.ChangeSubscriptionParams{PriceID: priceRef(pro.ID), IdempotencyKey: "staff-" + uuid.NewString()})
		requireCode(t, err, http.StatusBadRequest, billing.CodeInvalidParam)
		require.Len(t, w.railLedger(rail), charges, "no reason, no change")

		preview, err := w.client[remote].PreviewSubscriptionChange(t.Context(), sub, billing.ChangeSubscriptionParams{PriceID: priceRef(pro.ID)})
		require.NoError(t, err)
		require.Equal(t, "now", preview.Effective)
		require.Equal(t, int64(1500*10_000), preview.AmountDueNow)
		up, err := w.client[remote].ChangeSubscription(t.Context(), sub, billing.ChangeSubscriptionParams{PriceID: priceRef(pro.ID), Reason: "upgrade by phone", IdempotencyKey: "staff-" + uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, "succeeded", up.Status, "%+v", up)
		require.Equal(t, "now", up.Effective)
		require.Equal(t, int64(1500*10_000), up.AmountDueNow, "2000 less half of 1000")
		ledger := w.railLedger(rail)
		require.Len(t, ledger, charges+1)
		require.Equal(t, int64(1500), ledger[len(ledger)-1].Amount)
		w.requireMerchantInitiated(rail, ledger[len(ledger)-1])
		w.requireStaffCharge(c, sub, 1500, "upgrade by phone")
		successor := *up.SubscriptionID
		require.Equal(t, pro.ID, w.subscription(embedded, successor).PriceID)
		require.True(t, c.entitled(pro.ent))

		down, err := w.client[embedded].ChangeSubscription(t.Context(), successor, billing.ChangeSubscriptionParams{PriceID: priceRef(basic.ID), Reason: "downgrade by email", IdempotencyKey: "staff-" + uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, "succeeded", down.Status, "%+v", down)
		require.Equal(t, "period_end", down.Effective)
		require.Zero(t, down.AmountDueNow)
		require.Len(t, w.railLedger(rail), charges+1, "a downgrade charges nothing now")
		pending := w.subscription(remote, successor).ScheduledChange
		require.NotNil(t, pending)
		require.Equal(t, basic.ID, pending.PriceID)
		w.requireStaffSchedule(c, successor, "downgrade by email")

		// Without an active agreement on the card, staff cannot charge it.
		_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.mandates SET status = 'ended', end_reason = 'closed', ended_at = now() WHERE subscription_id = $1`), successor.UUID())
		require.NoError(t, err)
		_, err = w.client[remote].ChangeSubscription(t.Context(), successor, billing.ChangeSubscriptionParams{PriceID: priceRef(top.ID), Reason: "upgrade again", IdempotencyKey: "staff-" + uuid.NewString()})
		requireCode(t, err, http.StatusConflict, billing.CodeStoredCredentialRequired)
		require.Len(t, w.railLedger(rail), charges+1)
		require.Equal(t, basic.ID, w.subscription(embedded, successor).ScheduledChange.PriceID, "the refused change leaves the schedule")
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
