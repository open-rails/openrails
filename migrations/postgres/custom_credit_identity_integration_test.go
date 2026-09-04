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
