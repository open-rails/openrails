//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A customer sets its own low-balance line over HTTP: the PUT answers the
// effective line, a bad part or currency is 400 invalid_param, GET /v1/me
// shows the line of each balance with its source, and one customer's line is
// not another's. Staff read it in the customer's settings.
func TestLowBalanceLineRoutes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c, other := w.newCustomer(), w.newCustomer()
	for _, who := range []*customer{c, other} {
		_, err := w.client[embedded].CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{
			{CustomerID: who.cid(), Currency: "USD", Amount: 10_000_000, Source: "support", SourceID: "seed-" + who.id},
		})
		require.NoError(t, err)
	}
	lines := func(who *customer) []any {
		t.Helper()
		status, me := who.call(http.MethodGet, "", "", nil)
		require.Equal(t, http.StatusOK, status, "%v", me)
		return me["low_balance_lines"].([]any)
	}
	builtIn := map[string]any{"currency": "USD", "amount": "0", "runway_hours": float64(24), "source": "default"}
	require.Equal(t, []any{builtIn}, lines(c), "a 24-hour runway and no amount line")

	status, body := c.call(http.MethodPut, "/low-balance-lines/usd", "", map[string]any{"amount": "3000000", "runway_hours": 12})
	require.Equal(t, http.StatusOK, status, "%v", body)
	own := map[string]any{"currency": "USD", "amount": "3000000", "runway_hours": float64(12), "source": "customer"}
	require.Equal(t, own, body)
	require.Equal(t, []any{own}, lines(c))
	require.Equal(t, []any{builtIn}, lines(other), "one customer's line is not another's")
	staff, err := w.client[embedded].GetCustomer(t.Context(), c.cid())
	require.NoError(t, err)
	amount, hours := int64(3_000_000), 12
	require.Equal(t, []billing.LowBalanceLine{{Currency: "USD", Amount: &amount, RunwayHours: &hours}}, staff.Settings.LowBalanceLines)

	// Null inherits: only the runway is the customer's now.
	status, body = c.call(http.MethodPut, "/low-balance-lines/USD", "", map[string]any{"amount": nil, "runway_hours": 0})
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, map[string]any{"currency": "USD", "amount": "0", "runway_hours": float64(0), "source": "customer"}, body)

	for _, bad := range []struct {
		path  string
		body  map[string]any
		param string
	}{
		{"/low-balance-lines/USD", map[string]any{"amount": "-1", "runway_hours": nil}, "amount"},
		{"/low-balance-lines/USD", map[string]any{"amount": nil, "runway_hours": billing.MaxLowBalanceRunwayHours + 1}, "runway_hours"},
		{"/low-balance-lines/XXX", map[string]any{"amount": nil, "runway_hours": nil}, "currency"},
	} {
		status, body := c.call(http.MethodPut, bad.path, "", bad.body)
		require.Equal(t, http.StatusBadRequest, status, "%v", body)
		details := body["error"].(map[string]any)
		require.Equal(t, billing.CodeInvalidParam, details["code"], "%v", body)
		require.Equal(t, bad.param, details["param"], "%v", body)
	}
}
