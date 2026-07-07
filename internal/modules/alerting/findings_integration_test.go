//go:build integration

package alerting_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/reconcile"
)

// TestFindingNotifyDedupeEscalateResolve is the #787 no-mock proof: a finding
// created through the REAL reconcile.PGStore, notified through the REAL
// alerting store, deduped across a repeated reconcile pass, re-fired on a
// genuine escalation, and silenced once resolved.
func TestFindingNotifyDedupeEscalateResolve(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mid := uuid.New()
	seedMerchant(t, pool, mid)

	alertSvc := alerting.NewService(alerting.Deps{DB: appDB})
	findingStore := &reconcile.PGStore{DB: appDB}

	subjectKey := "chargeback-" + uuid.NewString()

	upsert := func(t *testing.T, sev reconcile.Severity) reconcile.FindingRecord {
		t.Helper()
		var rec reconcile.FindingRecord
		inConn(t, appDB, mid, func(ctx context.Context) {
			runID, err := findingStore.CreateRun(ctx, reconcile.ModeAdvisory, []reconcile.Provider{reconcile.ProviderNMI}, nil, nil)
			require.NoError(t, err)
			rec, err = findingStore.UpsertFinding(ctx, runID, reconcile.Finding{
				Provider:          reconcile.ProviderNMI,
				Type:              reconcile.FindingChargebackActiveSub,
				SubjectKey:        subjectKey,
				Severity:          sev,
				Status:            reconcile.FindingStatusRequiresReview,
				RecommendedAction: "review the chargeback before any local action",
			})
			require.NoError(t, err)
		})
		return rec
	}

	notify := func(t *testing.T, rec reconcile.FindingRecord) {
		t.Helper()
		inConn(t, appDB, mid, func(ctx context.Context) {
			require.NoError(t, alertSvc.NotifyFinding(ctx, rec))
		})
	}

	unreadCount := func(t *testing.T) int64 {
		t.Helper()
		var n int64
		inConn(t, appDB, mid, func(ctx context.Context) {
			var err error
			n, err = alertSvc.UnreadCount(ctx)
			require.NoError(t, err)
		})
		return n
	}

	// 1. Finding created (severity=high, requires_review) -> exactly one
	// notification through the real store.
	rec := upsert(t, reconcile.SeverityHigh)
	notify(t, rec)
	require.Equal(t, int64(1), unreadCount(t), "first requires_review finding must notify exactly once")

	// 2. A repeated reconcile pass re-observes the SAME open finding at the
	// SAME severity: the dedupe linkage (notified_at/notified_severity, set by
	// step 1) must block a second notification.
	rec = upsert(t, reconcile.SeverityHigh)
	require.NotNil(t, rec.NotifiedAt, "re-observed finding must carry the dedupe linkage from the first notify")
	notify(t, rec)
	require.Equal(t, int64(1), unreadCount(t), "re-observing an unchanged open finding must NOT re-notify")

	// 3. Escalation (severity increases while still open) re-fires exactly once
	// more.
	rec = upsert(t, reconcile.SeverityCritical)
	notify(t, rec)
	require.Equal(t, int64(2), unreadCount(t), "a genuine severity escalation must notify exactly once more")

	// 4. Resolve the finding — the resolution clears the notify linkage
	// (matching #736's silent alert-clear: resolving never itself pushes a
	// notification). Re-observing the now-resolved record must not re-fire.
	var resolved reconcile.FindingRecord
	inConn(t, appDB, mid, func(ctx context.Context) {
		ok, err := findingStore.AckFinding(ctx, rec.ID, "chargeback resolved with the processor")
		require.NoError(t, err)
		require.True(t, ok)
		resolved, err = findingStore.GetFinding(ctx, rec.ID)
		require.NoError(t, err)
	})
	require.Equal(t, reconcile.FindingStatusFixed, resolved.Status)
	require.Nil(t, resolved.NotifiedAt, "resolution must clear the notify linkage")
	notify(t, resolved)
	require.Equal(t, int64(2), unreadCount(t), "a resolved finding must never notify")
}
