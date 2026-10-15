//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/openrails"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// A failed bulk observation is diagnostic. A renewal whose own exact provider
// lookup succeeds can collect once, without waiting for a global recovery wake.
func TestFailedBulkRefreshDoesNotPostponeExactVerifiedRenewal(t *testing.T) {
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]openrails.PSPConfig) { delete(psps, "stripe"); delete(psps, "ccbill") }
	w.start()
	e := enrollEvery(t, w, "nmi", embedded, 8)
	end := e.periodEnd()
	w.holdRecovery("nmi")
	var unavailable atomic.Bool
	unavailable.Store(true)
	defer unavailable.Store(false)
	w.nmi.FailRequests(func(r *http.Request) bool {
		form, err := url.ParseQuery(readBody(r))
		return unavailable.Load() && err == nil && strings.HasSuffix(r.URL.Path, "/query.php") && form.Get("report_type") == "transaction" && form.Get("order_id") == "" && form.Get("transaction_id") == ""
	}, http.StatusServiceUnavailable, 100)
	w.advance(end.Add(time.Hour).Sub(w.clock.Now()))
	job, err := w.jobs.Insert(t.Context(), refreshMerchant{MerchantID: w.client[embedded].MerchantID().UUID()}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		observed, err := w.jobs.JobGet(t.Context(), job.Job.ID)
		return err == nil && observed.State == rivertype.JobStateRetryable
	}, 30*time.Second, 20*time.Millisecond, "the actual bulk provider read fails")
	w.runRenewals()
	require.Eventually(t, func() bool { return e.periodEnd().After(end) }, 20*time.Second, 30*time.Millisecond, "the exact-verified renewal does not wait for bulk coverage")
	require.Len(t, w.nmi.Attempts(), 2)
	require.Len(t, w.nmi.Ledger(""), 2)
	w.runRenewals()
	require.Len(t, w.nmi.Attempts(), 2, "re-evaluating the due pass cannot collect twice")
	require.Positive(t, w.workerFailures(riverjobs.KindProviderRefreshMerchant), "failed observations remain visible to operators")
}
