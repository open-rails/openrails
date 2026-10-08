//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/stretchr/testify/require"
)

// A restored observation cursor is not evidence of an applied financial book.
// Startup readonly refresh catches provider receipts up while leaving both the
// provider and policy-held local subscription lifecycle untouched.
func TestReadonlyProviderRecoveryAppliesReceiptsWithoutDestruction(t *testing.T) {
	w := prepareWorld(t, 12, func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeReadOnly })
	w.declare = func(psps map[string]openrails.PSPConfig) { delete(psps, "stripe"); delete(psps, "ccbill") }
	w.start()
	w.jobs.PeriodicJobs().Clear()
	l := importLegacy(t, w, "nmi", embedded)
	end := l.periodEnd()
	mid := w.client[embedded].MerchantID()
	w.stop()
	w.advance(end.Add(time.Hour).Sub(w.clock.Now()))
	_ = l.providerRenewal(true)
	renewal := w.nmi.LastSale().TransactionID
	w.nmi.DeleteSchedule(l.railSub) // provider truth may differ; readonly must not cancel locally
	w.advance(5 * 24 * time.Hour)
	_, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.psp_refresh_watermarks(merchant_id,psp_id,event_domain,watermark_at) VALUES($1,$2,'events',$3) ON CONFLICT(merchant_id,psp_id,event_domain) DO UPDATE SET watermark_at=EXCLUDED.watermark_at`), mid.UUID(), w.psp["nmi"].UUID(), w.clock.Now())
	require.NoError(t, err)
	before := len(w.nmi.Calls())
	readsBefore := w.nmi.Reads()
	w.start() // real RunOnStart, including bounded catch-up continuation
	require.Eventually(t, func() bool {
		var count int
		err := w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payments WHERE transaction_id=$1 AND psp_id=$2`), renewal, w.psp["nmi"].UUID()).Scan(&count)
		return err == nil && count == 1
	}, 60*time.Second, 50*time.Millisecond, "startup readonly refresh records the provider's paid receipt")
	require.Eventually(t, func() bool {
		var covered time.Time
		err := w.pool.QueryRow(t.Context(), w.q(`SELECT watermark_at FROM billing.psp_refresh_watermarks WHERE merchant_id=$1 AND psp_id=$2 AND event_domain='applied_events'`), mid.UUID(), w.psp["nmi"].UUID()).Scan(&covered)
		return err == nil && !covered.Before(w.clock.Now().Add(-5*time.Minute))
	}, 60*time.Second, 50*time.Millisecond, "only complete applied windows advance financial coverage")
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionActive, sub.Status, "provider cancellation stays policy-held")
	require.Nil(t, sub.CanceledAt)
	require.Zero(t, w.nmi.ScheduleDeletes(l.railSub), "no remote delete in readonly")
	require.Zero(t, len(w.nmi.Attempts()), "no recovery charge in readonly")
	require.Equal(t, before, len(w.nmi.Calls()), "read-only performs no provider mutations")
	require.Greater(t, w.nmi.Reads()["query:transaction"], readsBefore["query:transaction"], "provider transactions were actually read")
	require.Len(t, completed(w.payments(embedded, l.c.id)), 2)
	require.Empty(t, w.nmi.Unexpected())
}
