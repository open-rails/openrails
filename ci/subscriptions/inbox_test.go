//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/alerting"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// Ledger repairs and stalled workers land in the one merchant inbox, once per
// incident. A stall names the job kind, never the job's error text, which can
// name another merchant's records.
func TestOperationalAlertsLandInTheMerchantInbox(t *testing.T) {
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
	report := riverjobs.ProgressReport{CheckedAt: w.clock.Now(), Progressing: true, Kinds: []riverjobs.KindProgress{{Kind: kind, Reason: "3 consecutive failures"}}}
	require.NoError(t, monitor.RaiseAlerts(t.Context(), report))
	require.NoError(t, monitor.RaiseAlerts(t.Context(), report), "an alerted incident is not raised again")

	page, err := w.client[embedded].ListMerchantNotifications(t.Context(), billing.MerchantNotificationListParams{})
	require.NoError(t, err)
	var repairs, stalls []billing.MerchantNotification
	for _, n := range page.Items {
		switch {
		case strings.Contains(n.Body, "txn_e2e_repair"):
			repairs = append(repairs, n)
		case strings.Contains(n.Title, kind):
			stalls = append(stalls, n)
		}
	}
	require.Len(t, repairs, 1, "%+v", page.Items)
	require.Equal(t, billing.AlertSeverityCritical, repairs[0].Severity)
	require.Contains(t, repairs[0].Title, "chargeback_unmatched")
	require.Len(t, stalls, 1, "%+v", page.Items)
	require.Equal(t, billing.AlertSeverityCritical, stalls[0].Severity)

	status, raw := w.staff(http.MethodGet, "/v1/admin/notifications")
	require.Equal(t, http.StatusOK, status, raw)
	require.NotContains(t, raw, "cus_0000")

	unknown := billing.NotificationID(uuid.New())
	read, err := w.client[remote].MarkNotificationsRead(t.Context(), []billing.NotificationID{repairs[0].ID, stalls[0].ID, unknown})
	require.NoError(t, err)
	require.Nil(t, read[unknown])
	require.NotNil(t, read[repairs[0].ID].ReadAt)
	require.NotNil(t, read[stalls[0].ID].ReadAt)
	unread, err := w.client[embedded].GetUnreadNotificationCount(t.Context())
	require.NoError(t, err)
	require.Zero(t, unread.UnreadCount)
}
