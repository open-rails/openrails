//go:build integration

package postgresmigrations_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/stretchr/testify/require"
)

func TestCustomCreditIdentityCutoverRefusesMutableState(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedSuperuserPGXPool(t)
	migration, err := postgresmigrations.FS.ReadFile("0021_custom_credit_identity.up.sql")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, restoreOldConstraint, insert, read, message string
	}{
		{
			name:                 "financial history",
			restoreOldConstraint: `ALTER TABLE openrails.ledger_accounts DROP CONSTRAINT ledger_accounts_currency_shape`,
			insert:               `INSERT INTO openrails.ledger_accounts(merchant_id,account_type,currency,credits_posted) VALUES($1,'world','former-owner/tokens',37)`,
			read:                 `SELECT currency FROM openrails.ledger_accounts WHERE merchant_id=$1 AND credits_posted=37`,
			message:              "openrails.ledger_accounts contains mutable unit codes",
		},
		{
			name:                 "catalog definition",
			restoreOldConstraint: `ALTER TABLE openrails.catalog_credit_balances DROP CONSTRAINT catalog_credit_balances_unit_identity`,
			insert:               `INSERT INTO openrails.catalog_credit_balances(merchant_id,key,unit) VALUES($1,'tokens','former-owner/tokens')`,
			read:                 `SELECT unit FROM openrails.catalog_credit_balances WHERE merchant_id=$1 AND key='tokens'`,
			message:              "catalog_credit_balances has name-based units",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(ctx) }()
			mid := uuid.New()
			_, err = tx.Exec(ctx, `INSERT INTO openrails.merchants(id,slug,status) VALUES($1,$2,'active')`, mid, "cutover-"+mid.String())
			require.NoError(t, err)
			_, err = tx.Exec(ctx, tc.restoreOldConstraint)
			require.NoError(t, err)
			_, err = tx.Exec(ctx, tc.insert, mid)
			require.NoError(t, err)
			_, err = tx.Exec(ctx, `SAVEPOINT before_cutover`)
			require.NoError(t, err)
			_, err = tx.Exec(ctx, string(migration))
			require.ErrorContains(t, err, tc.message)
			_, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT before_cutover`)
			require.NoError(t, err)
			var currency string
			require.NoError(t, tx.QueryRow(ctx, tc.read, mid).Scan(&currency))
			require.Equal(t, "former-owner/tokens", currency, "refusal leaves existing data for its owner to resolve")
		})
	}
}

func TestCustomCreditIdentityFinalCurrencyConstraints(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedSuperuserPGXPool(t)
	rows, err := pool.Query(ctx, `SELECT c.relname, COALESCE(k.convalidated,false)
 FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid
 JOIN pg_namespace n ON n.oid=c.relnamespace
 LEFT JOIN pg_constraint k ON k.conrelid=c.oid AND k.conname=c.relname||'_currency_shape'
 WHERE n.nspname='openrails' AND a.attname='currency' AND NOT a.attisdropped AND c.relkind='r'`)
	require.NoError(t, err)
	count := 0
	for rows.Next() {
		var name string
		var valid bool
		require.NoError(t, rows.Scan(&name, &valid))
		require.True(t, valid, "%s needs a validated currency constraint", name)
		count++
	}
	rows.Close()
	require.NoError(t, rows.Err())
	require.GreaterOrEqual(t, count, 16)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	mid := uuid.New()
	_, err = tx.Exec(ctx, `INSERT INTO openrails.merchants(id,slug,status) VALUES($1,$2,'active')`, mid, "shape-"+mid.String())
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO openrails.ledger_accounts(merchant_id,account_type,currency) VALUES($1,'world','former-owner/tokens')`, mid)
	require.ErrorContains(t, err, "ledger_accounts_currency_shape")
}
