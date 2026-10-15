//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestResumeRequiresUsableCard(t *testing.T) {
	w := newWorld(t)
	e := enroll(t, w, "nmi", remote)
	replacement := e.c.saveCard("nmi", mastercard)
	e.c.must(http.MethodPost, "/subscriptions/"+e.sub.String()+"/cancel", "", map[string]any{"reason": "ending"})
	before := w.subscription(remote, e.sub)
	w.holdRecovery("nmi")
	status, out := e.c.call(http.MethodDelete, "/payment-methods/"+e.method, "", nil)
	require.Equal(t, http.StatusNoContent, status, "%v", out)
	status, out = e.c.call(http.MethodPost, "/subscriptions/"+e.sub.String()+"/resume", "", nil)
	require.Equal(t, http.StatusPaymentRequired, status, "%v", out)
	require.Equal(t, "payment_method_stale", out["error"].(map[string]any)["code"])

	sales := len(w.nmi.Sales())
	e.c.must(http.MethodPut, "/subscriptions/"+e.sub.String()+"/payment-method", "", map[string]any{"payment_method_id": replacement})
	changed := w.subscription(remote, e.sub)
	require.Equal(t, billing.SubscriptionCanceled, changed.Status)
	require.Equal(t, before.CanceledAt, changed.CanceledAt)
	require.Equal(t, before.CurrentPeriodEndsAt, changed.CurrentPeriodEndsAt)
	require.Equal(t, before.DeletionScheduledAt, changed.DeletionScheduledAt)
	require.Equal(t, before.CollectionPolicy, changed.CollectionPolicy)
	require.Len(t, w.nmi.Sales(), sales, "selecting a replacement charges no payment")
	e.c.must(http.MethodPost, "/subscriptions/"+e.sub.String()+"/resume", "", nil)
	require.Equal(t, billing.SubscriptionActive, w.subscription(remote, e.sub).Status)
	require.Len(t, w.nmi.Sales(), sales, "resuming schedules the next renewal without charging now")
}

func TestExpiredCanceledSubscriptionCannotChangeCard(t *testing.T) {
	w := newWorld(t)
	e := enroll(t, w, "nmi", remote)
	replacement := e.c.saveCard("nmi", mastercard)
	e.c.must(http.MethodPost, "/subscriptions/"+e.sub.String()+"/cancel", "", map[string]any{"reason": "ending"})
	w.advance(e.periodEnd().Add(time.Second).Sub(w.clock.Now()))
	status, out := e.c.call(http.MethodPut, "/subscriptions/"+e.sub.String()+"/payment-method", "", map[string]any{"payment_method_id": replacement})
	require.Equal(t, http.StatusConflict, status, "%v", out)
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(remote, e.sub).Status)
}
