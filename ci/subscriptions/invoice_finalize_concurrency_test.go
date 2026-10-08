//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// Two real invoice workers arrive at finalization before either INSERT can
// complete. The payer lock must serialize the period lookup as well as the
// write; the uniqueness constraint alone leaves one pass failing its merchant.
// Replicas a second apart close different periods over the same items: the
// later pass finds them invoiced and writes nothing.
func TestInvoiceReplicasFinalizeOnePeriodWithoutError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		skew time.Duration
	}{{"same instant", 0}, {"a second apart", time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, 2, 0, tc.skew)
			c := f.any().newCustomer()
			client := f.any().client[remote]
			_, err := client.SetCreditLimit(t.Context(), c.cid(), billing.SetCreditLimitParams{Currency: "USD", Amount: nmiInvoiceAmount})
			require.NoError(t, err)
			_, err = recordUsage(t.Context(), client, billing.RecordUsageParams{CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: "concurrent-finalize", Amount: nmiInvoiceAmount, Source: "test", SourceID: uuid.NewString()})
			require.NoError(t, err)
			f.advance(time.Minute)
			var merchant uuid.UUID
			require.NoError(t, f.base.pool.QueryRow(t.Context(), f.q(`SELECT id FROM billing.merchants WHERE slug=$1`), f.base.slug).Scan(&merchant))
			failures := &invoiceFailureLog{merchant: merchant.String()}
			log.AddHook(failures)
			lock, err := f.base.pool.Begin(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { _ = lock.Rollback(context.Background()) })
			_, err = lock.Exec(t.Context(), f.q(`LOCK TABLE billing.invoices IN SHARE MODE`))
			require.NoError(t, err)
			passes := invoicePasses(f, false)
			require.Eventually(t, func() bool {
				var blocked int
				err := f.base.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE application_name LIKE $1 AND state='active' AND wait_event_type='Lock'`, strings.TrimPrefix(f.base.schema, "gf_subs_")+"-%").Scan(&blocked)
				return err == nil && blocked >= 2
			}, 10*time.Second, 20*time.Millisecond, "both replicas reached finalization while the first write is paused")
			require.NoError(t, lock.Commit(t.Context()))
			f.awaitPasses(passes)
			require.Empty(t, failures.messages(), "a concurrent retry must return the same invoice, not fail the merchant pass")
			invoices, err := client.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: c.cid()})
			require.NoError(t, err)
			require.Len(t, invoices.Items, 1)
			require.Equal(t, nmiInvoiceAmount, invoices.Items[0].AmountDue)
			require.Empty(t, f.base.nmi.Attempts())
		})
	}
}

type invoiceFailureLog struct {
	merchant string
	mu       sync.Mutex
	failed   []string
}

func (*invoiceFailureLog) Levels() []log.Level { return []log.Level{log.ErrorLevel} }
func (h *invoiceFailureLog) Fire(entry *log.Entry) error {
	if entry.Message != "merchant failed; continuing" || fmt.Sprint(entry.Data["merchant_id"]) != h.merchant {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failed = append(h.failed, fmt.Sprint(entry.Data["error"]))
	return nil
}
func (h *invoiceFailureLog) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.failed...)
}
