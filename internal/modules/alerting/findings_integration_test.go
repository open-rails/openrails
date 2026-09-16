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

func TestFindingNotificationPersistenceFailureAndConcurrentRetry(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mid := uuid.New()
	seedMerchant(t, pool, mid)
	svc := newService(t, appDB, nil)
	sink := newWebhookRecorder(t, 0)
	store := &reconcile.PGStore{DB: appDB}
	var rec reconcile.FindingRecord
	inConn(t, appDB, mid, func(ctx context.Context) {
		_, err := svc.CreateWebhook(ctx, alerting.CreateWebhookInput{Name: "retained", URL: sink.server.URL})
		require.NoError(t, err)
		run, err := store.CreateRun(ctx, reconcile.ModeAdvisory, []reconcile.Provider{reconcile.ProviderNMI}, nil, nil)
		require.NoError(t, err)
		rec, err = store.UpsertFinding(ctx, run, reconcile.Finding{Provider: reconcile.ProviderNMI, Type: reconcile.FindingChargebackActiveSub, SubjectKey: uuid.NewString(), Severity: reconcile.SeverityMedium, Status: reconcile.FindingStatusRequiresReview})
		require.NoError(t, err)
	})
	// An insertion failure must roll back the episode claim before any webhook
	// receives a delivery. The trigger affects only this test's merchant.
	exec(t, pool, `CREATE FUNCTION openrails.reject_test_notification() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.merchant_id = TG_ARGV[0]::uuid THEN RAISE EXCEPTION 'injected notification failure'; END IF; RETURN NEW; END $$`)
	exec(t, pool, `CREATE TRIGGER reject_test_notification BEFORE INSERT ON openrails.merchant_notifications FOR EACH ROW EXECUTE FUNCTION openrails.reject_test_notification('`+mid.String()+`')`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_test_notification ON openrails.merchant_notifications; DROP FUNCTION IF EXISTS openrails.reject_test_notification()`)
	})
	inConn(t, appDB, mid, func(ctx context.Context) {
		require.ErrorContains(t, svc.NotifyFinding(ctx, rec), "persist finding notification")
		current, err := store.GetFinding(ctx, rec.ID)
		require.NoError(t, err)
		require.Nil(t, current.NotifiedAt)
	})
	require.Zero(t, sink.callCount())
	require.Zero(t, countNotifications(t, pool, mid))
	exec(t, pool, `DROP TRIGGER reject_test_notification ON openrails.merchant_notifications; DROP FUNCTION openrails.reject_test_notification()`)
	results := make(chan error, 8)
	for range 8 {
		go func() {
			results <- appDB.RunInMerchantConn(mctx(mid), func(ctx context.Context) error { return svc.NotifyFinding(ctx, rec) })
		}()
	}
	for range 8 {
		require.NoError(t, <-results)
	}
	require.Equal(t, 1, countNotifications(t, pool, mid))
	require.Equal(t, 1, sink.callCount(), "concurrent stale observations must not repeat the external delivery")
}
