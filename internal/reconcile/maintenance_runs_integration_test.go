//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestMaintenanceRunsKeepKindsAndAuthorizationEvidenceSeparate(t *testing.T) {
	database := startReconcilePostgres(t)
	mid := newReconcileMerchant(t, database)
	ctx := merchant.WithID(t.Context(), mid)
	require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := database.Gen(ctx)
		observation, err := q.CreateReconciliationRun(ctx, gen.CreateReconciliationRunParams{MerchantID: mid.UUID(), Mode: "advisory", Rails: []string{"nmi"}})
		require.NoError(t, err)
		_, err = q.GetDestructiveRun(ctx, gen.GetDestructiveRunParams{MerchantID: mid.UUID(), ID: observation.ID})
		require.Error(t, err)
		destructive, err := q.CreateDestructiveRun(ctx, gen.CreateDestructiveRunParams{ID: uuid.New(), MerchantID: mid.UUID(), Kind: "prune", Actor: "operator", Coverage: []byte(`{"verified":true}`)})
		require.NoError(t, err)
		_, err = q.GetReconciliationRun(ctx, destructive.ID)
		require.Error(t, err)
		_, err = q.FinishDestructiveRun(ctx, gen.FinishDestructiveRunParams{MerchantID: mid.UUID(), ID: destructive.ID, Status: "completed", Now: time.Now(), Affected: []byte(`{}`)})
		require.NoError(t, err)
		for _, column := range []string{"kind", "actor", "coverage", "expected_rows", "psp_id", "inventory_manifest", "inventory_total_rows"} {
			var canUpdate bool
			require.NoError(t, database.Qx(ctx).QueryRow(ctx, `SELECT has_column_privilege('openrails_app','billing.maintenance_runs',$1,'UPDATE')`, column).Scan(&canUpdate))
			require.False(t, canUpdate, "authorization and inventory evidence must be immutable: %s", column)
		}
		var canDelete bool
		require.NoError(t, database.Qx(ctx).QueryRow(ctx, `SELECT has_table_privilege('openrails_app','billing.maintenance_runs','DELETE')`).Scan(&canDelete))
		require.False(t, canDelete)
		_, err = database.Qx(ctx).Exec(ctx, `INSERT INTO billing.maintenance_runs(merchant_id,kind,status,finished_at,inventory_total_rows,inventory_manifest) VALUES($1,'purge_inventory','completed',now(),2,'{"total_rows":1}')`, mid.UUID())
		require.Error(t, err, "inventory header and manifest must agree")
		_, err = database.Qx(ctx).Exec(ctx, `INSERT INTO billing.maintenance_runs(merchant_id,kind,actor) VALUES($1,'unimplemented','operator')`, mid.UUID())
		require.Error(t, err, "unknown maintenance kinds must be refused")
		return nil
	}))
}

// Child references are restricted by run class, not only merchant+id: stamped
// rows and before-images name destructive runs, findings name observation runs.
func TestMaintenanceRunChildReferencesAreClassRestricted(t *testing.T) {
	database := startReconcilePostgres(t)
	baseCtx := merchant.WithID(t.Context(), dbtest.TestMerchantID)
	f := seedPruneFixture(t, database, baseCtx)
	mid := dbtest.TestMerchantID.UUID()
	require.NoError(t, database.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		q := database.Gen(ctx)
		observation, err := q.CreateReconciliationRun(ctx, gen.CreateReconciliationRunParams{MerchantID: mid, Mode: "advisory", Rails: []string{"nmi"}})
		require.NoError(t, err)
		destructive, err := q.CreateDestructiveRun(ctx, gen.CreateDestructiveRunParams{ID: uuid.New(), MerchantID: mid, PspID: &f.pspID, Kind: "prune", Actor: "operator"})
		require.NoError(t, err)
		inventory := uuid.New()
		_, err = database.Qx(ctx).Exec(ctx, `INSERT INTO billing.maintenance_runs(id,merchant_id,kind,status,finished_at,inventory_total_rows,inventory_manifest) VALUES($1,$2,'purge_inventory','completed',now(),1,'{"total_rows":1}')`, inventory, mid)
		require.NoError(t, err)

		refused := func(sql string, args ...any) {
			t.Helper()
			_, err := database.Qx(ctx).Exec(ctx, sql, args...)
			require.ErrorContains(t, err, "foreign key", sql)
		}
		for _, run := range []uuid.UUID{observation.ID, inventory} {
			refused(`UPDATE billing.payments SET destructive_run_id=$1 WHERE merchant_id=$2 AND id=$3`, run, mid, f.payID)
			refused(`UPDATE billing.subscriptions SET destructive_run_id=$1 WHERE merchant_id=$2 AND id=$3`, run, mid, f.subID)
			refused(`UPDATE billing.checkout_sessions SET destructive_run_id=$1 WHERE merchant_id=$2 AND id=$3`, run, mid, f.sessID)
			refused(`UPDATE billing.entitlements SET destructive_run_id=$1 WHERE merchant_id=$2 AND id=$3`, run, mid, f.entID)
			refused(`INSERT INTO billing.destructive_run_before_images(merchant_id,destructive_run_id,table_name,row_id,before) VALUES($1,$2,'subscriptions',$3,'{}')`, mid, run, f.subID)
		}
		refused(`INSERT INTO billing.reconciliation_findings(merchant_id,finding_type,subject_key,severity,status,first_seen_run,last_seen_run) VALUES($1,'pull.class_fk',$2,'low','reconcile_required',$3,$3)`, mid, uuid.NewString(), destructive.ID)

		_, err = database.Qx(ctx).Exec(ctx, `UPDATE billing.payments SET destructive_run_id=$1 WHERE merchant_id=$2 AND id=$3`, destructive.ID, mid, f.payID)
		require.NoError(t, err)
		_, err = database.Qx(ctx).Exec(ctx, `UPDATE billing.payments SET destructive_run_id=NULL WHERE merchant_id=$1 AND id=$2`, mid, f.payID)
		require.NoError(t, err)
		finding := uuid.NewString()
		_, err = database.Qx(ctx).Exec(ctx, `INSERT INTO billing.reconciliation_findings(merchant_id,finding_type,subject_key,severity,status,first_seen_run,last_seen_run) VALUES($1,'pull.class_fk',$2,'low','reconcile_required',$3,$3)`, mid, finding, observation.ID)
		require.NoError(t, err)
		_, err = database.Qx(ctx).Exec(ctx, `DELETE FROM billing.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, mid, finding)
		return err
	}))
}
