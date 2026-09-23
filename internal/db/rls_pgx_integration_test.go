//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Connection binding supplies session scope to queries that explicitly test it.
// The probe has no policies, so there is no hidden database filter.

func pgxProbeVals(ctx context.Context, t *testing.T, d *db.DB) []string {
	t.Helper()
	rows, err := d.Qx(ctx).Query(ctx, `SELECT val FROM billing.rls_probe WHERE merchant_id=nullif(current_setting('app.merchant_id',true),'')::uuid ORDER BY val`)
	require.NoError(t, err)
	defer rows.Close()
	var vals []string
	for rows.Next() {
		var v string
		require.NoError(t, rows.Scan(&v))
		vals = append(vals, v)
	}
	require.NoError(t, rows.Err())
	return vals
}

func TestExplicitMerchantPredicate_PgxSide(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := startRLSContainer(t)

	super := newDBRetry(t, superDSN)
	defer super.Close()
	_, err := super.Pool().Exec(ctx, rlsSetupDDL)
	require.NoError(t, err)
	_, err = super.Qx(ctx).Exec(ctx, `DELETE FROM billing.rls_probe WHERE id = ANY($1::uuid[])`,
		[]string{
			"00000000-0000-0000-0000-00000000000a",
			"00000000-0000-0000-0000-00000000000b",
			"00000000-0000-0000-0000-00000000000c",
		})
	require.NoError(t, err)
	// Seed through the fixture owner.
	_, err = super.Qx(ctx).Exec(ctx, `
		INSERT INTO billing.rls_probe (id, merchant_id, val) VALUES
		('00000000-0000-0000-0000-00000000000a', $1, 'a-row'),
		('00000000-0000-0000-0000-00000000000b', $2, 'b-row')`,
		rlsTenantA.String(), rlsTenantB.String())
	require.NoError(t, err)

	app := newDBRetry(t, appDSN)
	defer app.Close()

	// (1) Fail-closed: no GUC anywhere -> the pgx pool sees NOTHING.
	require.Empty(t, pgxProbeVals(ctx, t, app), "no GUC => zero rows visible on pgx pool (fail-closed)")

	// (2) WithMerchantConn pins a pgx connection with merchant A's GUC: every
	// explicitly predicated query on the returned ctx is scoped to merchant A.
	ctxA := merchant.WithID(ctx, rlsTenantA)
	pinnedCtx, release, err := app.WithMerchantConn(ctxA)
	require.NoError(t, err)
	require.Equal(t, []string{"a-row"}, pgxProbeVals(pinnedCtx, t, app),
		"pinned merchant-A connection must see exactly merchant A's row")

	// (3) RunInTx on the pinned connection inherits the session GUC.
	require.NoError(t, app.RunInTx(pinnedCtx, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM billing.rls_probe WHERE merchant_id=nullif(current_setting('app.merchant_id',true),'')::uuid`).Scan(&n); err != nil {
			return err
		}
		require.Equal(t, 1, n, "tx begun on the pinned conn must inherit the merchant GUC")
		return nil
	}))

	// (4) Release resets the GUC: the pool is fail-closed again.
	release()
	require.Empty(t, pgxProbeVals(ctx, t, app), "after release the pool must be fail-closed again")

	// (5) MerchantTx scopes a transaction without a pinned connection: merchant A
	// sees only its row and cannot delete merchant B's (invisible).
	require.NoError(t, app.MerchantTx(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM billing.rls_probe WHERE merchant_id=nullif(current_setting('app.merchant_id',true),'')::uuid`).Scan(&n); err != nil {
			return err
		}
		require.Equal(t, 1, n)
		tag, derr := tx.Exec(ctx, `DELETE FROM billing.rls_probe WHERE val = 'b-row' AND merchant_id=nullif(current_setting('app.merchant_id',true),'')::uuid`)
		require.NoError(t, derr)
		require.Equal(t, int64(0), tag.RowsAffected(), "merchant A must not delete merchant B's row")
		return nil
	}))

	// (6) An explicit insert predicate rejects a row stamped with
	// merchant B's id.
	err = app.MerchantTx(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		tag, ierr := tx.Exec(ctx, `
            INSERT INTO billing.rls_probe (id,merchant_id,val)
            SELECT '00000000-0000-0000-0000-00000000000c',$1::uuid,'cross'
            WHERE $1::uuid=nullif(current_setting('app.merchant_id',true),'')::uuid`,
			rlsTenantB.String())
		require.EqualValues(t, 0, tag.RowsAffected())
		return ierr
	})
	require.NoError(t, err)

	// (7) Merchant B's row survived and is visible under merchant B's GUC.
	ctxB := merchant.WithID(ctx, rlsTenantB)
	require.NoError(t, app.MerchantTx(ctxB, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM billing.rls_probe WHERE merchant_id=nullif(current_setting('app.merchant_id',true),'')::uuid`).Scan(&n); err != nil {
			return err
		}
		require.Equal(t, 1, n)
		return nil
	}))
}
