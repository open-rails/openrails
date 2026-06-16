//go:build integration

package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/merchant"
)

// pgx-side twin of TestRLSEnforcement_Under_OpenRailsAppRole (#334): proves
// the sqlc/pgx plumbing (Qx, WithMerchantConn dual-pin, MerchantTx) enforces the
// exact same migration-050 RLS semantics as the bun side. Same probe table,
// same policy form, same unprivileged role.

func pgxProbeVals(ctx context.Context, t *testing.T, d *DB) []string {
	t.Helper()
	rows, err := d.Qx(ctx).Query(ctx, `SELECT val FROM openrails.rls_probe ORDER BY val`)
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

func TestRLSEnforcement_PgxSide(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := startRLSContainer(t)

	super := newDBRetry(t, superDSN)
	defer super.Close()
	_, err := super.Pool().Exec(ctx, rlsSetupDDL)
	require.NoError(t, err)
	_, err = super.Qx(ctx).Exec(ctx, `DELETE FROM openrails.rls_probe WHERE id = ANY($1::uuid[])`,
		[]string{
			"00000000-0000-0000-0000-00000000000a",
			"00000000-0000-0000-0000-00000000000b",
			"00000000-0000-0000-0000-00000000000c",
		})
	require.NoError(t, err)
	// Seed through the pgx side (superuser bypasses RLS).
	_, err = super.Qx(ctx).Exec(ctx, `
		INSERT INTO openrails.rls_probe (id, merchant_id, val) VALUES
		('00000000-0000-0000-0000-00000000000a', $1, 'a-row'),
		('00000000-0000-0000-0000-00000000000b', $2, 'b-row')`,
		rlsTenantA.String(), rlsTenantB.String())
	require.NoError(t, err)

	app := newDBRetry(t, appDSN)
	defer app.Close()

	// (1) Fail-closed: no GUC anywhere -> the pgx pool sees NOTHING.
	require.Empty(t, pgxProbeVals(ctx, t, app), "no GUC => zero rows visible on pgx pool (fail-closed)")

	// (2) WithMerchantConn pins a pgx connection with merchant A's GUC: every
	// Qx(ctx) query on the returned ctx is scoped to merchant A.
	ctxA := merchant.WithID(ctx, rlsTenantA)
	pinnedCtx, release, err := app.WithMerchantConn(ctxA)
	require.NoError(t, err)
	require.Equal(t, []string{"a-row"}, pgxProbeVals(pinnedCtx, t, app),
		"pinned merchant-A connection must see exactly merchant A's row")

	// (3) RunInTx on the pinned connection inherits the session GUC.
	require.NoError(t, app.RunInTx(pinnedCtx, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM openrails.rls_probe`).Scan(&n); err != nil {
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
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM openrails.rls_probe`).Scan(&n); err != nil {
			return err
		}
		require.Equal(t, 1, n)
		tag, derr := tx.Exec(ctx, `DELETE FROM openrails.rls_probe WHERE val = 'b-row'`)
		require.NoError(t, derr)
		require.Equal(t, int64(0), tag.RowsAffected(), "merchant A must not delete merchant B's row")
		return nil
	}))

	// (6) WITH CHECK: a merchant-A transaction cannot insert a row stamped with
	// merchant B's id.
	err = app.MerchantTx(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		_, ierr := tx.Exec(ctx, `
			INSERT INTO openrails.rls_probe (id, merchant_id, val)
			VALUES ('00000000-0000-0000-0000-00000000000c', $1, 'cross')`,
			rlsTenantB.String())
		return ierr
	})
	require.Error(t, err, "WITH CHECK must reject a cross-merchant insert on the pgx side")

	// (7) Merchant B's row survived and is visible under merchant B's GUC.
	ctxB := merchant.WithID(ctx, rlsTenantB)
	require.NoError(t, app.MerchantTx(ctxB, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM openrails.rls_probe`).Scan(&n); err != nil {
			return err
		}
		require.Equal(t, 1, n)
		return nil
	}))
}
