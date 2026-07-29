//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/alerting"
)

// or#877: the two scheduled workers FC-16 turned up. Both are driven on the
// BARE context a River job actually receives — handing them a pinned merchant
// connection would do the worker's own job for it and turn an inert pass green,
// which is exactly the harness mistake that hid this whole family.

// TestFindingsDigestWorkerDigestsUnderMerchantScope: the digest sweep selects
// merchants with an undigested low-severity finding and must then actually
// digest it. It used to hand DigestFindings a merchant.WithID CONTEXT VALUE
// only — no pinned connection — so the cadence watermark and the pending count
// both read the base pool, the count came back 0, and the pass returned early
// having digested nothing for every merchant it selected.
func TestFindingsDigestWorkerDigestsUnderMerchantScope(t *testing.T) {
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	dbtest.EnsureTestMerchant(ctx, t, pool)

	subject := "or877-digest-" + uuid.NewString()
	var findingID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO openrails.reconciliation_findings
		    (merchant_id, finding_type, subject_key, severity, status, last_seen_at)
		VALUES ($1, 'consistency.or877_probe', $2, 'low', 'requires_review', now())
		RETURNING id`,
		dbtest.TestMerchantID.UUID(), subject).Scan(&findingID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.reconciliation_findings WHERE id = $1", findingID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.finding_digest_state WHERE merchant_id = $1",
			dbtest.TestMerchantID.UUID())
	})
	// A merchant that already digested inside the cadence window is skipped, so
	// clear the watermark this suite's other tests may have left behind.
	_, err := pool.Exec(ctx, "DELETE FROM openrails.finding_digest_state WHERE merchant_id = $1",
		dbtest.TestMerchantID.UUID())
	require.NoError(t, err)

	worker := FindingsDigestWorker{DB: dbi, Alerts: alerting.NewService(alerting.Deps{DB: dbi})}
	require.NoError(t, worker.Work(ctx, nil))

	var notifiedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT notified_at FROM openrails.reconciliation_findings WHERE id = $1", findingID).Scan(&notifiedAt))
	require.NotNil(t, notifiedAt, "the pending low-severity finding must actually be digested, not merely selected")

	var watermark time.Time
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT last_digested_at FROM openrails.finding_digest_state WHERE merchant_id = $1",
		dbtest.TestMerchantID.UUID()).Scan(&watermark))
	require.WithinDuration(t, time.Now(), watermark, time.Minute, "the cadence watermark must advance with the digest")
}

// TestCatalogReconciliationPullRunsPerMerchant: the alert-only pull reconciler
// used to run ONE pass on the bare job context, where the local catalog reads
// back empty and merchant.Require (in the persist leg) cannot succeed at all.
// It now fans out over the rail-armed work queue and runs each merchant's pass
// inside that merchant's scope — proven here by the persist leg's own effect:
// an open drift event with no matching divergence this pass is auto-resolved.
func TestCatalogReconciliationPullRunsPerMerchant(t *testing.T) {
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	dbtest.EnsureTestMerchant(ctx, t, pool)

	// The work queue is "armed on a reconcilable rail", so the merchant needs a
	// live PSP to be in it at all.
	accountID := "acct_or877_" + uuid.NewString()[:8]
	_, err := pool.Exec(ctx, `
		INSERT INTO openrails.psps (merchant_id, rail, environment, account_id)
		VALUES ($1, 'stripe', 'test', $2)`, dbtest.TestMerchantID.UUID(), accountID)
	require.NoError(t, err)

	var driftID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO openrails.catalog_drift_events
		    (merchant_id, rail, kind, openrails_resource_type, openrails_resource_id, external_resource_id, detected_at)
		VALUES ($1, 'stripe', 'orphan_in_stripe', 'product', $2, $3, now())
		RETURNING id`,
		dbtest.TestMerchantID.UUID(), uuid.NewString(), "prod_"+uuid.NewString()[:8]).Scan(&driftID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.catalog_drift_events WHERE id = $1", driftID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.psps WHERE account_id = $1", accountID)
	})

	// No rails wired: both provider passes skip, so the pass computes an empty
	// divergence set. What it must still do is reach the merchant's own rows.
	worker := CatalogReconciliationPullWorker{DB: dbi, Config: &config.Config{}}
	require.NoError(t, worker.Work(ctx, nil))

	var resolvedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT resolved_at FROM openrails.catalog_drift_events WHERE id = $1", driftID).Scan(&resolvedAt))
	require.NotNil(t, resolvedAt, "the per-merchant pass must reach this merchant's drift ledger")
}
