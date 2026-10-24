//go:build integration

package critical_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestCancellationCustody(t *testing.T) {
	t.Run("OR-J17-S02/cancel-cannot-discard-an-accepted-renewal", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		due := *sub.CurrentPeriodEndsAt
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		p.mu.Lock()
		p.responseGate = gate
		p.mu.Unlock()
		defer gate.unblock()
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 1)
		select {
		case <-gate.arrived:
		case <-time.After(15 * time.Second):
			t.Fatal("renewal did not reach the provider acceptance boundary")
		}
		require.Len(t, p.effects(t), 2, "the original purchase and admitted renewal have actually debited")
		status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Cancel during accepted payment"})
		require.Equal(t, http.StatusConflict, status, string(raw))
		pending, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, due, *pending.CurrentPeriodEndsAt)
		gate.unblock()
		renewalSettled(t, a, p, sub, due)
		status, raw = a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Stop after settlement"})
		require.Equal(t, http.StatusOK, status, string(raw))
		canceled, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionCanceled, canceled.Status)
		require.Equal(t, due.Add(24*time.Hour), *canceled.CurrentPeriodEndsAt)
		a.clock.Advance(25 * time.Hour)
		runRegistered(t, fleet, "openrails.dunning", 2)
		require.Len(t, p.effects(t), 2, "cancellation stops future work without losing the accepted payment")
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 2)
	})
}
