//go:build integration

package reconcile

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// fakePass lets a test inject deterministic findings into the Converge engine.
type fakePass struct {
	plane    string
	findings []ConvergeFinding
}

func (p fakePass) Plane() string { return p.plane }
func (p fakePass) Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error) {
	return p.findings, nil
}

// A clean scope (no findings) converges to a no-op: no run row, no writes.
func TestConverge_CleanScopeIsNoOp(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	e.passes = nil // no passes → nothing to find

	var res ConvergeResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var err error
		res, err = e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID})
		return err
	}))
	require.Equal(t, 0, res.Findings)
	require.Nil(t, res.RunID, "a clean scope creates no run")
}

// The confirmed-absence gate (§3.2): a MISSING/AUTO finding repairs immediately,
// but an EXCESS/AUTO finding is HELD until its source domain is reconciled — then
// the same finding's repair fires on the next pass.
func TestConverge_ConfirmedAbsenceGate(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	suffix := uuid.NewString()[:8]
	missingKey := "missing:" + suffix
	excessKey := "excess:" + suffix
	repaired := map[string]int{}

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key IN ($2,$3)`, merchantID, missingKey, excessKey)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_state WHERE merchant_id=$1`, merchantID)
			return nil
		})
	})

	e := NewConvergeEngine(appDB)
	e.passes = []Pass{fakePass{plane: "DERIVE", findings: []ConvergeFinding{
		{
			Type: "derive.grant_effect.missing", Shape: ShapeMissing,
			Class: ClassAuto, Severity: "high", SubjectKey: missingKey,
			Repair: func(ctx context.Context) error { repaired[missingKey]++; return nil },
		},
		{
			Type: "derive.grant.excess", Shape: ShapeExcess,
			Class: ClassAuto, Severity: "high", SubjectKey: excessKey, SourceDomain: DomainSubscriptions,
			Repair: func(ctx context.Context) error { repaired[excessKey]++; return nil },
		},
	}}}

	status := func(ctx context.Context, key string) string {
		var s string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`,
			merchantID, key).Scan(&s))
		return s
	}

	// Pass 1: subscriptions NOT reconciled → MISSING repaired, EXCESS held.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID})
		require.NoError(t, err)
		require.Equal(t, 2, res.Findings)
		require.Equal(t, 1, res.AutoFixed)
		require.Equal(t, 1, res.Held)
		require.Equal(t, 1, repaired[missingKey], "MISSING repairs immediately (additive, always safe)")
		require.Equal(t, 0, repaired[excessKey], "EXCESS held by the confirmed-absence gate")
		require.Equal(t, "auto_fixed", status(ctx, missingKey))
		require.Equal(t, "held", status(ctx, excessKey))
		return nil
	}))

	// Mark subscriptions fully reconciled (the gate opens).
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := appDB.Gen(ctx).UpsertReconciliationState(ctx, gen.UpsertReconciliationStateParams{
			MerchantID: merchantID, SourceDomain: "subscriptions", FullyReconciled: true,
		})
		return err
	}))

	// Pass 2: same EXCESS finding now retracts (repair fires; status flips).
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID})
		require.NoError(t, err)
		require.Equal(t, 1, repaired[excessKey], "EXCESS repaired once the domain is reconciled")
		require.Equal(t, "auto_fixed", status(ctx, excessKey))
		_ = res
		return nil
	}))
}
