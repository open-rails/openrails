//go:build integration

package converge

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/merchant"
)

// #665: life.provider_intent.stuck moved from the legacy pull engine's PS-10
// into the LIFE pass. Non-terminal rail_intents older than the hardcoded
// thresholds surface on the merchant-wide (sweep/post-pull) scope; mode/
// kill-switch parks stay informational (reconcile_required, low); fresh
// intents stay invisible; converge never touches the intent rows; and a
// recovered intent's finding auto-resolves subject-first.
func TestConverge_LifeStuckIntent(t *testing.T) {
	appDB := startReconcilePostgres(t)
	suffix := uuid.NewString()[:8]
	// Dedicated merchant: the stuck lane runs merchant-wide, so the shared
	// merchant's fixture noise would leak into the assertions.
	mID := merchant.ID(uuid.New())
	baseCtx := merchant.WithID(context.Background(), mID)
	_, err := appDB.Pool().Exec(context.Background(),
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		mID.UUID(), "stuck-"+suffix)
	require.NoError(t, err)

	oldPending := uuid.New()    // pending 26h, genuine failure -> requires_review
	parkedPending := uuid.New() // pending 26h, kill-switch park -> informational
	freshPending := uuid.New()  // pending 1h -> no finding
	oldUnknown := uuid.New()    // unknown_needs_verify 3h -> requires_review

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		seed := func(id uuid.UUID, status string, age time.Duration, reason *string) {
			t.Helper()
			_, err := appDB.Qx(ctx).Exec(ctx,
				`INSERT INTO openrails.rail_intents
				   (id, rail, intent_type, idempotency_key,
				    status, attempts, next_attempt_at, origin, last_failure_reason, created_at, merchant_id)
				 VALUES ($1, 'mobius', 'nmi_delete_subscription', $2, $3, 2, now(), 'system', $4, now() - make_interval(mins => $5), $6)`,
				id, fmt.Sprintf("stuck-%s-%s", id.String()[:8], suffix),
				status, reason, int(age.Minutes()), mID.UUID())
			require.NoError(t, err)
		}
		failure := "nmi error: connection refused"
		park := "nmi client is read-only (mode=readonly)"
		ambiguous := "ambiguous: timeout after send"
		seed(oldPending, "pending", 26*time.Hour, &failure)
		seed(parkedPending, "pending", 26*time.Hour, &park)
		seed(freshPending, "pending", time.Hour, nil)
		seed(oldUnknown, "unknown_needs_verify", 3*time.Hour, &ambiguous)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, table := range []string{"reconciliation_findings", "reconciliation_runs", "rail_intents"} {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.`+table+` WHERE merchant_id=$1`, mID.UUID())
			}
			return nil
		})
		_, _ = appDB.Pool().Exec(context.Background(), `DELETE FROM openrails.merchants WHERE id=$1`, mID.UUID())
	})

	e := NewConvergeEngine(appDB)
	findingStatus := func(ctx context.Context, intentID uuid.UUID) (string, string, bool) {
		var status, severity string
		err := appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status, severity FROM openrails.reconciliation_findings
			 WHERE merchant_id=$1 AND finding_type='life.provider_intent.stuck' AND subject_key=$2`,
			mID.UUID(), intentID.String()).Scan(&status, &severity)
		if err != nil {
			return "", "", false
		}
		return status, severity, true
	}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: mID}) // merchant-wide = the sweep scope
		require.NoError(t, err)
		require.Equal(t, 3, res.Findings, "old pending + parked + old unknown")
		require.Equal(t, 2, res.RequiresReview)
		require.Equal(t, 1, res.ReconcileRequired, "mode-parked is informational")

		status, severity, ok := findingStatus(ctx, oldPending)
		require.True(t, ok)
		require.Equal(t, "requires_review", status)
		require.Equal(t, "high", severity)

		status, severity, ok = findingStatus(ctx, parkedPending)
		require.True(t, ok)
		require.Equal(t, "reconcile_required", status, "kill-switch park is informational")
		require.Equal(t, "low", severity)

		status, _, ok = findingStatus(ctx, oldUnknown)
		require.True(t, ok)
		require.Equal(t, "requires_review", status)

		_, _, ok = findingStatus(ctx, freshPending)
		require.False(t, ok, "a fresh pending intent is not stuck")

		// Converge never touches the intents themselves.
		var intentStatus string
		var attempts int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status, attempts FROM openrails.rail_intents WHERE id=$1`, oldPending).Scan(&intentStatus, &attempts))
		require.Equal(t, "pending", intentStatus)
		require.Equal(t, 2, attempts)
		return nil
	}))

	// Recovery: succeeded/superseded intents no longer meet the stuck criteria;
	// their findings auto-resolve subject-first on the next pass.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := appDB.Qx(ctx).Exec(ctx,
			`UPDATE openrails.rail_intents SET status='succeeded', executed_at=now() WHERE id=$1`, oldPending)
		require.NoError(t, err)
		_, err = appDB.Qx(ctx).Exec(ctx,
			`UPDATE openrails.rail_intents SET status='superseded' WHERE id=$1`, oldUnknown)
		require.NoError(t, err)

		res, err := e.Converge(ctx, Scope{Merchant: mID})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings, "only the parked intent is still stuck")

		for _, id := range []uuid.UUID{oldPending, oldUnknown} {
			var status, resolution string
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
				`SELECT status, COALESCE(resolution,'') FROM openrails.reconciliation_findings
				 WHERE merchant_id=$1 AND finding_type='life.provider_intent.stuck' AND subject_key=$2`,
				mID.UUID(), id.String()).Scan(&status, &resolution))
			require.Equal(t, "fixed", status)
			require.Equal(t, "auto_vanished", resolution)
		}
		return nil
	}))
}
