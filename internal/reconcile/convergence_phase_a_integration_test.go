//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #511 Phase A: the findings ledger admits self-describing qualified finding
// types and the held/indeterminate statuses, and rejects unknown names. The
// legacy PS-* codes stay admitted during the transition.
func TestConvergenceFindings_PhaseA_Taxonomy(t *testing.T) {
	ctx := context.Background()
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(ctx, dbtest.TestMerchantID)
	runID := uuid.New() // first_seen_run/last_seen_run are plain uuids (no FK)
	suffix := uuid.NewString()[:8]

	cases := []struct{ findingType, status, provider string }{
		{"derive.grant.excess", "held", "self"},               // EXCESS held by the confirmed-absence gate
		{"derive.grant_effect.excess", "held", "self"},        // grant-effect excess (credit clawback)
		{"life.subscription.grace_exhausted", "open", "self"}, // grace exhausted
		{"consistency.duplicate.provider_charge", "admin_pending", "self"},
		{"pull.charge.missing", "indeterminate", "nmi"}, // provider authority unreachable
		{"PS-2", "open", "nmi"},                         // legacy code still admitted (transition)
	}
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		q := gen.New(appDB.Qx(ctx))
		for _, tc := range cases {
			f, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
				MerchantID: merchantID, Provider: tc.provider, FindingType: tc.findingType,
				SubjectKey: tc.findingType + ":" + suffix, Severity: "high", Status: tc.status,
				RequiresAdmin: tc.status == "admin_pending", RunID: runID,
			})
			require.NoError(t, err, "insert %s/%s must be admitted", tc.findingType, tc.status)
			require.Equal(t, tc.findingType, f.FindingType)
			require.Equal(t, tc.status, f.Status)
		}
		return nil
	}))

	// An unknown finding_type violates the CHECK (isolated conn so a poisoned tx
	// can't affect the assertions above).
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		q := gen.New(appDB.Qx(ctx))
		_, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
			MerchantID: merchantID, Provider: "self", FindingType: "bogus",
			SubjectKey: "bogus:" + suffix, Severity: "high", Status: "open", RunID: runID,
		})
		require.Error(t, err, "an unknown finding_type must be rejected by the CHECK")
		return nil
	}))
}

// #511 Phase A: the per-(merchant, source_domain) confirmed-absence gate (§3.2).
// A domain defaults to NOT reconciled (so destructive EXCESS repairs are held);
// marking it fully reconciled flips it; the watermark is preserved across upserts.
func TestConvergenceState_ConfirmedAbsenceGate(t *testing.T) {
	ctx := context.Background()
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(ctx, dbtest.TestMerchantID)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		q := gen.New(appDB.Qx(ctx))
		t.Cleanup(func() {
			_, _ = appDB.Qx(context.Background()).Exec(context.Background(),
				`DELETE FROM openrails.reconciliation_state WHERE merchant_id=$1`, merchantID)
		})

		// Default: a domain with no row reads NOT reconciled → the gate holds EXCESS.
		for _, d := range []string{"subscriptions", "payments", "grants"} {
			got, err := q.IsSourceDomainReconciled(ctx, gen.IsSourceDomainReconciledParams{
				MerchantID: merchantID, SourceDomain: d,
			})
			require.NoError(t, err)
			require.NotNil(t, got)
			require.False(t, *got, "domain %s defaults to not-reconciled", d)
		}

		// Mark subscriptions fully reconciled with a pull watermark.
		now := time.Now().UTC().Truncate(time.Second)
		st, err := q.UpsertReconciliationState(ctx, gen.UpsertReconciliationStateParams{
			MerchantID: merchantID, SourceDomain: "subscriptions", FullyReconciled: true, LastFullPullAt: &now,
		})
		require.NoError(t, err)
		require.True(t, st.FullyReconciled)
		require.NotNil(t, st.LastFullPullAt)

		got, err := q.IsSourceDomainReconciled(ctx, gen.IsSourceDomainReconciledParams{
			MerchantID: merchantID, SourceDomain: "subscriptions",
		})
		require.NoError(t, err)
		require.True(t, *got, "subscriptions now reconciled")

		// Other domains remain ungated.
		got, err = q.IsSourceDomainReconciled(ctx, gen.IsSourceDomainReconciledParams{
			MerchantID: merchantID, SourceDomain: "payments",
		})
		require.NoError(t, err)
		require.False(t, *got, "payments still not reconciled")

		// Re-upsert with nil watermark preserves the prior last_full_pull_at (COALESCE).
		st2, err := q.UpsertReconciliationState(ctx, gen.UpsertReconciliationStateParams{
			MerchantID: merchantID, SourceDomain: "subscriptions", FullyReconciled: false, LastFullPullAt: nil,
		})
		require.NoError(t, err)
		require.False(t, st2.FullyReconciled)
		require.NotNil(t, st2.LastFullPullAt, "watermark preserved across upsert")

		list, err := q.ListReconciliationState(ctx, merchantID)
		require.NoError(t, err)
		require.Len(t, list, 1)
		return nil
	}))
}
