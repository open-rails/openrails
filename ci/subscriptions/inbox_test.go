//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/alerting"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// Ledger repairs and stalled workers are findings in the one queue, once per
// incident; a stall resolves when its work progresses and names only its job
// kind, never its error text, which can name another merchant's records. The
// console's bell counts them via the metrics query; the inbox routes are gone.
func TestOperationalProblemsAreFindings(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	merchantID := w.client[embedded].MerchantID()

	repair := alerting.LedgerRepair{Provider: "nmi", Operation: "chargeback_unmatched", TransactionID: "txn_e2e_repair", IdempotencyKey: "e2e-repair", Err: errors.New("matched no single charge")}
	for range 2 {
		require.NoError(t, d.RunInMerchantScope(t.Context(), merchantID, "ledger repair", func(ctx context.Context) error {
			return alerting.RecordLedgerRepair(ctx, d, w.clock.Now(), repair)
		}))
	}

	const kind, secret = "openrails.e2e_stalled", "another merchant's cus_0000 failed"
	_, err = w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.worker_state (worker_kind, last_error_at, last_error, consecutive_failures, updated_at) VALUES ($1, $2, $3, 3, $2)`),
		kind, w.clock.Now(), secret)
	require.NoError(t, err)
	monitor := &riverjobs.ProgressMonitor{DB: d, Clock: w.clock}
	stalled := riverjobs.ProgressReport{CheckedAt: w.clock.Now(), Progressing: true, Kinds: []riverjobs.KindProgress{{Kind: kind, Reason: "3 consecutive failures"}}}
	require.NoError(t, monitor.RaiseAlerts(t.Context(), stalled))
	require.NoError(t, monitor.RaiseAlerts(t.Context(), stalled), "an alerted incident is not raised again")

	list := func(findingType string) []map[string]any {
		t.Helper()
		status, page := w.staffJSON(http.MethodGet, "/v1/admin/findings?type="+findingType, nil)
		require.Equal(t, http.StatusOK, status, "%v", page)
		var out []map[string]any
		for _, item := range page["data"].([]any) {
			out = append(out, item.(map[string]any))
		}
		return out
	}
	repairs := list(string(alerting.FindingLedgerUnbooked))
	require.Len(t, repairs, 1, "%v", repairs)
	require.Equal(t, "critical", repairs[0]["severity"])
	require.Equal(t, "requires_review", repairs[0]["status"])
	require.Contains(t, repairs[0]["recommended_action"], "txn_e2e_repair")
	require.Equal(t, "txn_e2e_repair", repairs[0]["evidence"].(map[string]any)["transaction_id"])
	stalls := list("life.worker.*")
	require.Len(t, stalls, 1, "%v", stalls)
	require.Equal(t, kind, stalls[0]["subject_key"])
	status, raw := w.staff(http.MethodGet, "/v1/admin/findings?type=life.worker.stalled")
	require.Equal(t, http.StatusOK, status, raw)
	require.NotContains(t, raw, "cus_0000")

	bell := func() int64 {
		t.Helper()
		status, res := w.staffJSON(http.MethodPost, "/v1/admin/metrics/query", map[string]any{
			"measures": []string{"open_findings"}, "range": nowRange(),
			"filters": map[string][]string{"finding_status": {"requires_review"}, "finding_type": {string(alerting.FindingLedgerUnbooked), string(alerting.FindingWorkerStalled)}},
		})
		require.Equal(t, http.StatusOK, status, "%v", res)
		rows := res["rows"].([]any)
		require.Len(t, rows, 1, "%v", res)
		row := rows[0].([]any)
		return number(t, row[len(row)-1])
	}
	require.EqualValues(t, 2, bell())

	recovered := riverjobs.ProgressReport{CheckedAt: w.clock.Now(), Progressing: true, Kinds: []riverjobs.KindProgress{{Kind: kind}}}
	require.NoError(t, monitor.RaiseAlerts(t.Context(), recovered))
	require.Empty(t, list("life.worker.*"), "a stall resolves itself when its work progresses")
	require.EqualValues(t, 1, bell())
	require.NoError(t, monitor.RaiseAlerts(t.Context(), stalled))
	require.Len(t, list("life.worker.*"), 1, "a fresh stall is raised again")

	for _, gone := range []string{"/v1/admin/notifications", "/v1/admin/notifications/unread-count", "/v1/admin/catalog/drift"} {
		status, raw := w.staff(http.MethodGet, gone)
		require.Equal(t, http.StatusNotFound, status, "%s: %s", gone, raw)
	}
	status, raw = w.staff(http.MethodGet, "/v1/admin/findings/summary")
	require.Equal(t, http.StatusBadRequest, status, "summary is no finding id: %s", raw)
	status, raw = w.staff(http.MethodPost, "/v1/admin/notifications/read")
	require.True(t, status == http.StatusNotFound || status == http.StatusMethodNotAllowed, "%d %s", status, raw)
	require.False(t, strings.Contains(raw, "unread_count"))
}

// nowRange is a metrics range around the wall clock, whose end is the instant
// a snapshot measure reads.
func nowRange() map[string]string {
	now := time.Now().UTC()
	return map[string]string{"from": now.Add(-24 * time.Hour).Format(time.RFC3339), "to": now.Add(time.Hour).Format(time.RFC3339)}
}
