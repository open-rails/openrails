//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestResumeAfterCardRemovalRequiresDefault(t *testing.T) {
	w := newWorld(t)
	e := enroll(t, w, "nmi", remote)
	method, err := billing.ParsePaymentMethodID(e.method)
	require.NoError(t, err)
	e.c.must(http.MethodPost, "/subscriptions/"+e.sub.String()+"/cancel", "", map[string]any{"reason": "ending"})
	before := w.subscription(remote, e.sub)
	e.c.setDefault(e.method)
	var ordersBefore, versionsBefore int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.orders WHERE payment_method_id=$1`), method.UUID()).Scan(&ordersBefore))
	require.Positive(t, ordersBefore)
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payment_method_versions WHERE payment_method_id=$1`), method.UUID()).Scan(&versionsBefore))
	require.Positive(t, versionsBefore)
	w.refreshProviders()
	e.c.must(http.MethodDelete, "/payment-methods/"+e.method, "", nil)
	w.advance(5*time.Minute + time.Second)
	w.wake()
	w.until(func() bool {
		var status string
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status FROM billing.payment_methods WHERE id=$1`), method.UUID()).Scan(&status))
		return status == "removed"
	}, "the fake provider completes card removal")
	var ordersAfter, versionsAfter, defaults int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.orders WHERE payment_method_id=$1`), method.UUID()).Scan(&ordersAfter))
	require.Equal(t, ordersBefore, ordersAfter, "historical orders keep their original card")
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payment_method_versions WHERE payment_method_id=$1`), method.UUID()).Scan(&versionsAfter))
	require.Equal(t, versionsBefore, versionsAfter)
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.money_settings WHERE default_payment_method_id=$1`), method.UUID()).Scan(&defaults))
	require.Zero(t, defaults)
	require.Empty(t, e.c.must(http.MethodGet, "/payment-methods", "", nil)["data"])
	deletes := len(w.nmi.CallsTo(http.MethodDelete, "/customers/", nil))
	require.Equal(t, 1, deletes)
	e.c.must(http.MethodDelete, "/payment-methods/"+e.method, "", nil)
	require.Len(t, w.nmi.CallsTo(http.MethodDelete, "/customers/", nil), deletes, "repeat removal does not call the provider again")
	events, err := w.client[embedded].ListHostEvents(t.Context(), billing.HostEventListParams{})
	require.NoError(t, err)
	for _, event := range events.Items {
		_, err := w.client[embedded].AcknowledgeHostEvents(t.Context(), []billing.HostEventID{event.ID})
		require.NoError(t, err)
	}
	require.NoError(t, w.exportArchive(), "the retained deletion receipt is valid archive evidence")
	status, out := e.c.call(http.MethodPost, "/subscriptions/"+e.sub.String()+"/resume", "", nil)
	require.Equal(t, http.StatusPaymentRequired, status, "%v", out)
	require.Equal(t, "payment_method_stale", out["error"].(map[string]any)["code"])
	after := w.subscription(remote, e.sub)
	require.Equal(t, billing.SubscriptionCanceled, after.Status)
	require.Equal(t, before.CurrentPeriodEndsAt, after.CurrentPeriodEndsAt)
	// A legacy subscription with no own-card reference still gets the specific
	// missing-default refusal; it never turns a missing card into a 500.
	_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.subscriptions SET payment_method_id=NULL WHERE id=$1`), e.sub.UUID())
	require.NoError(t, err)
	status, out = e.c.call(http.MethodPost, "/subscriptions/"+e.sub.String()+"/resume", "", nil)
	require.Equal(t, http.StatusBadRequest, status, "%v", out)
	require.Equal(t, "default_payment_method_required", out["error"].(map[string]any)["code"])
}
