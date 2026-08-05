//go:build integration

package converge

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #511/#570: the findings ledger admits self-describing qualified finding
// types and the current workflow statuses, and rejects everything else —
// including retired legacy PS-* finding codes and old lifecycle names.
func TestConvergenceFindings_PhaseA_Taxonomy(t *testing.T) {
	ctx := context.Background()
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(ctx, dbtest.TestMerchantID)
	// first_seen_run/last_seen_run FK into reconciliation_runs (#709): seed a run.
	runID := uuid.New()
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := appDB.Qx(ctx).Exec(ctx, `
			INSERT INTO openrails.reconciliation_runs (id, merchant_id, mode, rails, status)
			VALUES ($1, $2, 'advisory', '{self}', 'completed')`, runID, merchantID)
		return err
	}))
	suffix := uuid.NewString()[:8]

	cases := []struct{ findingType, status, provider string }{
		{"derive.grant.excess", "reconcile_required", "self"},        // EXCESS blocked by the confirmed-absence gate
		{"derive.grant_effect.excess", "reconcile_required", "self"}, // grant-effect excess (credit clawback)
		{"life.subscription.grace_exhausted", "auto_fixed", "self"},  // local lifecycle repair applied
		{"consistency.duplicate.provider_charge", "requires_review", "self"},
		{"pull.charge.missing", "fixed", "nmi"},
		{"pull.refund.missing", "ignored", "nmi"},
	}
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		for _, tc := range cases {
			var returnedType, returnedStatus string
			err := appDB.Qx(ctx).QueryRow(ctx, `
				INSERT INTO openrails.reconciliation_findings (
					merchant_id, finding_type, subject_key, severity, status,
					evidence, resolved_at, resolution, first_seen_run, last_seen_run
				) VALUES (
					$1, $2, $3, 'high', $4,
					jsonb_strip_nulls(jsonb_build_object('provider', NULLIF($5::text, 'self'))),
					CASE WHEN $4::text IN ('auto_fixed', 'fixed', 'ignored') THEN now() ELSE NULL END,
					CASE $4::text
						WHEN 'auto_fixed' THEN 'enforced'
						WHEN 'fixed' THEN 'admin_fixed'
						WHEN 'ignored' THEN 'ignored'
						ELSE NULL
					END,
					$6, $6
				)
				RETURNING finding_type, status`,
				merchantID, tc.findingType, tc.findingType+":"+suffix, tc.status, tc.provider, runID).Scan(&returnedType, &returnedStatus)
			require.NoError(t, err, "insert %s/%s must be admitted", tc.findingType, tc.status)
			require.Equal(t, tc.findingType, returnedType)
			require.Equal(t, tc.status, returnedStatus)
		}
		return nil
	}))

	// An unknown finding_type — and, post-Phase-F, a legacy PS-* code — violates
	// the CHECK (isolated conn each so a poisoned tx can't affect the assertions
	// above).
	for _, bad := range []string{"bogus", "PS-2"} {
		bad := bad
		require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			q := gen.New(appDB.Qx(ctx))
			_, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
				MerchantID: merchantID, FindingType: bad,
				SubjectKey: bad + ":" + suffix, Severity: "high", Status: "reconcile_required", RunID: &runID,
			})
			require.Error(t, err, "finding_type %q must be rejected by the CHECK (slugs only)", bad)
			return nil
		}))
	}

	for _, bad := range []string{"open", "held", "admin_pending", "admin_required", "resolved", "dismissed", "indeterminate"} {
		bad := bad
		require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			q := gen.New(appDB.Qx(ctx))
			_, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
				MerchantID: merchantID, FindingType: "derive.grant.excess",
				SubjectKey: "bad-status:" + bad + ":" + suffix, Severity: "high", Status: bad, RunID: &runID,
			})
			require.Error(t, err, "legacy status %q must be rejected by the CHECK", bad)
			return nil
		}))
	}
}

// #511 Phase A: the per-(merchant, source_domain) confirmed-absence gate (§3.2).
// A domain defaults to NOT reconciled (so destructive EXCESS repairs are reconcile_required);
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

		// Mark subscriptions fully reconciled.
		st, err := q.UpsertReconciliationState(ctx, gen.UpsertReconciliationStateParams{
			MerchantID: merchantID, SourceDomain: "subscriptions", FullyReconciled: true,
		})
		require.NoError(t, err)
		require.True(t, st.FullyReconciled)

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

		// Re-upsert updates in place rather than adding a second row.
		st2, err := q.UpsertReconciliationState(ctx, gen.UpsertReconciliationStateParams{
			MerchantID: merchantID, SourceDomain: "subscriptions", FullyReconciled: false,
		})
		require.NoError(t, err)
		require.False(t, st2.FullyReconciled)

		var stateCount int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.reconciliation_state WHERE merchant_id=$1`, merchantID,
		).Scan(&stateCount))
		require.Equal(t, 1, stateCount)
		return nil
	}))
}
