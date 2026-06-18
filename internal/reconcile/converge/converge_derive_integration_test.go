//go:build integration

package converge

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #511 Phase D (DERIVE plane): a live grant whose derived effect is missing is
// detected as `derive.grant_effect.missing` and auto-repaired by running
// derive-2 (grants.MaterializeGrant). Idempotent: a second pass finds nothing.
func TestConverge_DeriveGrantEffectMissing(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	cur := "TC" + uuid.NewString()[:6]
	e := NewConvergeEngine(appDB)

	var customer, grantID uuid.UUID

	// Seed a credit grant WITHOUT materializing it → its #512 deposit is missing.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		amt := int64(5000)
		g, err := grants.New(appDB.Gen(ctx), merchantID).Grant(ctx, grants.GrantInput{
			Customer: customer, Kind: grants.Credit, Source: grants.Purchase,
			SourceID: "pay_" + uuid.NewString()[:8], Amount: &amt, Currency: &cur,
		})
		require.NoError(t, err)
		grantID = g.ID
		dep, err := appDB.Gen(ctx).GrantCreditDeposited(ctx, gen.GrantCreditDepositedParams{MerchantID: merchantID, GrantID: g.ID})
		require.NoError(t, err)
		require.False(t, dep, "precondition: effect not yet materialized")
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "grant_effect:"+grantID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.ledger_transfers WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			return nil
		})
	})

	// Converge → detect + auto-repair (run derive-2).
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.AutoFixed)

		dep, err := appDB.Gen(ctx).GrantCreditDeposited(ctx, gen.GrantCreditDepositedParams{MerchantID: merchantID, GrantID: grantID})
		require.NoError(t, err)
		require.True(t, dep, "repair materialized the credit deposit")

		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='derive.grant_effect.missing' AND subject_key=$2`,
			merchantID, "grant_effect:"+grantID.String()).Scan(&status))
		require.Equal(t, "auto_fixed", status)
		return nil
	}))

	// Idempotent: the effect now exists → no DERIVE finding on a second pass.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 0, res.Findings, "already materialized → converged, no finding")
		return nil
	}))
}

// #511 Phase D (DERIVE plane): a TERMINATED entitlement grant whose entitlement
// window was never retracted (a bare revoke event, no derive-2) is detected as
// `derive.grant_effect.excess` and auto-retracted (RevokeEntitlementsByGrant).
// AUTO + not gated: the grant + its revoke are both present (a recorded decision,
// not a confirmed-absence case). Idempotent.
func TestConverge_DeriveGrantEffectExcess_TerminatedNotRetracted(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	feature := "feat_" + uuid.NewString()[:8]

	var customer, grantID uuid.UUID

	// Seed a materialized entitlement grant, then append a BARE revoke event (no
	// MaterializeGrant) → terminated grant, entitlement window still live.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		gl := grants.New(appDB.Gen(ctx), merchantID)
		// Grace-sourced so the materialized entitlement (which now preserves its
		// source via grant_id, #511) isn't also flagged by the CON orphan-source
		// checks — this test is about grant_effect.excess retraction, not CON.
		g, err := gl.Grant(ctx, grants.GrantInput{
			Customer: customer, Kind: grants.Entitlement, Source: grants.Grace,
			SourceID: uuid.NewString(), Spec: &grants.Spec{Entitlements: []string{feature}},
		})
		require.NoError(t, err)
		require.NoError(t, gl.MaterializeGrant(ctx, g)) // entitlement window exists
		_, err = gl.Revoke(ctx, g.ID, "admin removed (retraction not yet run)")
		require.NoError(t, err)
		grantID = g.ID
		live, err := appDB.Gen(ctx).GrantHasLiveEntitlement(ctx, gen.GrantHasLiveEntitlementParams{MerchantID: merchantID, GrantID: g.ID})
		require.NoError(t, err)
		require.True(t, live, "precondition: entitlement still live despite the revoke")
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "grant_effect:"+grantID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2 AND event<>'grant'`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			return nil
		})
	})

	// Converge → detect excess → retract the entitlement window.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.AutoFixed, "terminated grant retraction is AUTO (recorded revoke, not absence)")

		live, err := appDB.Gen(ctx).GrantHasLiveEntitlement(ctx, gen.GrantHasLiveEntitlementParams{MerchantID: merchantID, GrantID: grantID})
		require.NoError(t, err)
		require.False(t, live, "entitlement window retracted by the repair")

		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='derive.grant_effect.excess' AND subject_key=$2`,
			merchantID, "grant_effect:"+grantID.String()).Scan(&status))
		require.Equal(t, "auto_fixed", status)
		return nil
	}))

	// Idempotent: retracted → no finding on a second pass.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 0, res.Findings, "retracted → converged, no finding")
		return nil
	}))
}
