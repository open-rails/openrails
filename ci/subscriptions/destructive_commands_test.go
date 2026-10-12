//go:build e2e && integration

package subscriptions_test

import (
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
