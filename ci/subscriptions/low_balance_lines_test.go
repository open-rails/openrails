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
	var got []any
	reads := w.count(func() { got = lines(c) })
	require.Equal(t, []any{builtIn}, got, "a 24-hour runway and no amount line")
	// A balance with no running charge projects nothing: its read costs what
	// it did before balances were watched.
	require.Equal(t, 1, reads["ListRunningChargesByCustomer"], "%v", reads)
	for _, name := range []string{"GetMoneyAccountSettings", "ListSpendableCreditLots", "ListLapsedCreditLots", "GetLedgerAccount"} {
		require.Zero(t, reads[name], "%s: %v", name, reads)
	}

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
		path, code, param string
		body              map[string]any
	}{
		{"/low-balance-lines/USD", billing.CodeInvalidParam, "amount", map[string]any{"amount": "-1", "runway_hours": nil}},
		{"/low-balance-lines/USD", billing.CodeInvalidParam, "runway_hours", map[string]any{"amount": nil, "runway_hours": billing.MaxLowBalanceRunwayHours + 1}},
		{"/low-balance-lines/XXX", "currency_unsupported", "currency", map[string]any{"amount": nil, "runway_hours": nil}},
	} {
		status, body := c.call(http.MethodPut, bad.path, "", bad.body)
		require.Equal(t, http.StatusBadRequest, status, "%v", body)
		details := body["error"].(map[string]any)
		require.Equal(t, bad.code, details["code"], "%v", body)
		require.Equal(t, bad.param, details["param"], "%v", body)
	}
}

// A low-balance line decides no billing mode: a customer that set one before
// any money settings still becomes arrears when staff give it a credit line,
// and keeps its line.
func TestLowBalanceLineLeavesTheBillingMode(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	status, body := c.call(http.MethodPut, "/low-balance-lines/USD", "", map[string]any{"amount": "1000000", "runway_hours": nil})
	require.Equal(t, http.StatusOK, status, "%v", body)
	staff, err := w.client[embedded].UpdateCustomer(t.Context(), c.cid(), billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 10_000_000}}})
	require.NoError(t, err)
	amount := int64(1_000_000)
	require.Equal(t, []billing.LowBalanceLine{{Currency: "USD", Amount: &amount}}, staff.Settings.LowBalanceLines)
	require.Equal(t, []billing.CreditLimit{{Currency: "USD", Amount: 10_000_000}}, staff.Settings.CreditLimits)
	balance, err := w.client[embedded].GetBalance(t.Context(), c.cid(), "USD")
	require.NoError(t, err)
	require.Equal(t, billing.BillingModeArrears, balance.BillingMode)
}
