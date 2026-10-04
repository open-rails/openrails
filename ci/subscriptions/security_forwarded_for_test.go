//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// SEC: behind a proxy that appends its own X-Forwarded-For line (HAProxy's
// option forwardfor), every line counts, in order, and the resolver walks them
// right to left as the proxy chain wrote them. A client that sends its own
// line naming a CCBill address does not pass CCBill's source allowlist.
func TestSecurityForwardedForReadsEveryLine(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	m := importCCBill(t, w)
	cancel := map[string]string{"subscriptionId": m.railSub, "source": "webAdmin"}

	status, body := w.postCCBillVia("Cancellation", []string{ccbillSourceIP, "203.0.113.9"}, cancel)
	require.Equal(t, http.StatusForbidden, status, "the client wrote the CCBill line: %v", body)
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, m.sub).Status)

	status, body = w.postCCBillVia("Cancellation", []string{"203.0.113.9", ccbillSourceIP}, cancel)
	require.Equal(t, http.StatusOK, status, "the proxy's line names CCBill: %v", body)
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, m.sub).Status)
}
