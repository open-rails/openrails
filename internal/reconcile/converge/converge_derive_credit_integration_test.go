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
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Credit path of derive.grant_effect.excess: a terminated credit grant whose
// clawback never ran is detected and auto-retracted (unspent → revoked_credits),
// under the APP role (the production path the Convergence Engine runs as).
func TestConverge_DeriveGrantEffectExcess_CreditClawback(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	cur := "TC" + uuid.NewString()[:6]
	e := NewConvergeEngine(appDB)
	var customer, grantID uuid.UUID

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		gl := grants.New(appDB.Gen(ctx), merchantID)
		amt := int64(8000)
		g, err := gl.Grant(ctx, grants.GrantInput{
			Customer: customer, Kind: grants.Credit, Source: grants.Purchase,
			SourceID: "pay_" + uuid.NewString()[:8], Amount: &amt, Currency: &cur,
		})
		require.NoError(t, err)
		require.NoError(t, gl.MaterializeGrant(ctx, g))
		_, err = gl.Revoke(ctx, g.ID, "admin removed (clawback not yet run)")
		require.NoError(t, err)
		grantID = g.ID
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

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.AutoFixed)
		rem, err := appDB.Gen(ctx).GetCreditLotRemaining(ctx, gen.GetCreditLotRemainingParams{MerchantID: merchantID, GrantID: grantID})
		require.NoError(t, err)
		require.Equal(t, int64(0), rem)
		ml := ledger.New(appDB.Gen(ctx), merchantID)
		revAcc, err := ml.EnsureSystemAccount(ctx, ledger.RevokedCredits, cur)
		require.NoError(t, err)
		bal, err := ml.Balance(ctx, revAcc)
		require.NoError(t, err)
		require.Equal(t, int64(8000), bal, "unspent frozen in revoked_credits")
		return nil
	}))
}

// #677: two OVERLAPPING converge runs that both detect the same unclawed
// terminated credit grant apply exactly ONE clawback — the repair runs in a
// merchant tx under the per-customer spend lock (lockedMaterialize), so the
// loser blocks, re-reads remaining=0 and no-ops.
func TestConverge_OverlappingRuns_SingleClawback(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	cur := "TC" + uuid.NewString()[:6]
	e := NewConvergeEngine(appDB)
	var customer, grantID uuid.UUID

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		gl := grants.New(appDB.Gen(ctx), merchantID)
		amt := int64(8000)
		g, err := gl.Grant(ctx, grants.GrantInput{
			Customer: customer, Kind: grants.Credit, Source: grants.Purchase,
			SourceID: "pay_" + uuid.NewString()[:8], Amount: &amt, Currency: &cur,
		})
		require.NoError(t, err)
		require.NoError(t, gl.MaterializeGrant(ctx, g))
		_, err = gl.Revoke(ctx, g.ID, "admin removed (clawback not yet run)")
		require.NoError(t, err)
		grantID = g.ID
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

	run := func() error {
		return appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
			return err
		})
	}
	errs := make(chan error, 2)
	go func() { errs <- run() }()
	go func() { errs <- run() }()
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.ledger_transfers WHERE merchant_id=$1 AND grant_id=$2 AND transfer_type='credit_revoke'`,
			merchantID, grantID).Scan(&n))
		require.Equal(t, 1, n, "exactly one clawback across overlapping runs")
		ml := ledger.New(appDB.Gen(ctx), merchantID)
		revAcc, err := ml.EnsureSystemAccount(ctx, ledger.RevokedCredits, cur)
		require.NoError(t, err)
		bal, err := ml.Balance(ctx, revAcc)
		require.NoError(t, err)
		require.Equal(t, int64(8000), bal, "clawed once, not twice")
		return nil
	}))
}
