//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/engine"
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// A real customer collection reopens an uncollectible order. The next staff
// write-off is a new logical command even when the injected clock never moves.
func TestUncollectibleCommandUsesCollectionEpisode(t *testing.T) {
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", card{Brand: "visa", Last4: "0002", Decline: "202"})
	invoice := observedInvoice(t, w, c, 50_000_000)
	w.refreshProviders()
	w.settleCollectionScans()
	frozen := w.clock.Now()
	_, err := w.client[remote].MarkOrderUncollectible(t.Context(), invoice)
	require.NoError(t, err)
	status, body := c.call(http.MethodPost, "/orders/"+invoice.String()+"/pay", "reopen-written-off-order", w.usagePay(invoice, method))
	require.Equal(t, http.StatusPaymentRequired, status, "%v", body)
	require.Equal(t, billing.OrderOpen, w.invoice(invoice).Status)
	_, err = w.client[remote].MarkOrderUncollectible(t.Context(), invoice)
	require.NoError(t, err)
	require.Equal(t, frozen, w.clock.Now(), "the admission identity must not depend on clock advancement")
	rows, err := w.pool.Query(t.Context(), w.q(`SELECT action_key,admitted_at FROM billing.destructive_actions WHERE kind='mark_uncollectible' AND target=$1 ORDER BY admission_sequence`), invoice.UUID().String())
	require.NoError(t, err)
	defer rows.Close()
	var keys []string
	var times []time.Time
	for rows.Next() {
		var key string
		var at time.Time
		require.NoError(t, rows.Scan(&key, &at))
		keys = append(keys, key)
		times = append(times, at)
	}
	require.NoError(t, rows.Err())
	require.Len(t, keys, 2)
	require.NotEqual(t, keys[0], keys[1])
	require.True(t, times[0].Equal(times[1]))
}

// tripRecordedFinancialActor asks the real admission boundary to admit an
// oversized batch under the actor of an already-recorded command. It must
// commit the hold without admitting any of that batch's work.
func tripRecordedFinancialActor(t *testing.T, w *world, kind destructive.Kind, target string) {
	t.Helper()
	var actor destructive.Actor
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT actor_issuer,actor_id,coalesce(credential,'') FROM billing.destructive_actions WHERE kind=$1 AND target=$2 ORDER BY admission_sequence DESC LIMIT 1`), string(kind), target).Scan(&actor.Issuer, &actor.ID, &actor.Credential))
	mid := w.client[embedded].MerchantID().UUID()
	_, err := engine.Graph(w.rt).Runtime.DestructiveAdmissions.Admit(t.Context(), destructive.Command{MerchantID: mid, Kind: kind, Target: "refused-batch", Key: uuid.NewString(), Digest: destructive.Digest("six refused targets"), Units: 6, Actor: actor})
	require.ErrorIs(t, err, destructive.ErrHeld)
}

func TestDestructiveHoldDoesNotStrandCommittedCancellation(t *testing.T) {
	w := newWorld(t)
	member := importLegacy(t, w, "stripe", embedded)
	w.converge()
	w.refreshProviders()
	w.settleCollectionScans()
	w.stripe.SubscriptionWritesUnavailable(true)
	status, body := w.staffJSON(http.MethodPost, "/v1/admin/subscriptions/"+member.sub.String()+"/cancel", map[string]any{"reason": "end membership"})
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, member.sub).Status)
	tripRecordedFinancialActor(t, w, destructive.CancelSubscription, member.sub.UUID().String())
	w.stripe.SubscriptionWritesUnavailable(false)
	w.until(func() bool { return w.stripe.Subscription(member.railSub)["cancel_at_period_end"] == true }, "the held actor's already-committed cancellation completes its required provider stop")
	w.until(func() bool {
		var state string
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT state FROM billing.destructive_actions WHERE kind='cancel_subscription' AND target=$1`), member.sub.UUID().String()).Scan(&state))
		return state == "succeeded"
	}, "the completed provider stop settles its original admission")
	require.Len(t, member.stripeSubWrites(), 1)
}

func TestDestructiveHoldAfterRefundSendPreservesFinalizationAndReplay(t *testing.T) {
	w := newWorld(t)
	member := enroll(t, w, "stripe", embedded)
	payment := completed(w.payments(embedded, member.c.id))[0]
	params := billing.RefundPaymentParams{Amount: 4_000_000, Reason: "requested_by_customer", IdempotencyKey: "held-refund"}
	gate := w.refundGateServed("stripe")
	t.Cleanup(func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	})
	staff := openrails.WithInvoker(context.WithoutCancel(t.Context()), openrails.Invoker{Issuer: "https://staff.example", ID: "refunder"})
	done := make(chan error, 1)
	go func() {
		_, err := w.client[embedded].RefundPayment(staff, payment.ID, params)
		done <- err
	}()
	select {
	case <-gate.arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("refund did not reach provider")
	}
	tripRecordedFinancialActor(t, w, destructive.Refund, payment.ID.UUID().String())
	close(gate.release)
	require.NoError(t, <-done)
	w.stripe.unhold()
	w.settle()
	_, err := w.client[embedded].RefundPayment(staff, payment.ID, params)
	require.NoError(t, err, "a completed request replays through a later actor hold")
	got, err := w.client[embedded].GetPayment(t.Context(), payment.ID)
	require.NoError(t, err)
	require.EqualValues(t, 4_000_000, got.AmountRefunded)
	var count int
	var state string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*),min(state) FROM billing.destructive_actions WHERE kind='refund' AND target=$1`), payment.ID.UUID().String()).Scan(&count, &state))
	require.Equal(t, 1, count)
	require.Equal(t, "succeeded", state)
}
