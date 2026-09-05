//go:build integration

package postgresmigrations_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestCustomCreditIdentityRejectsMutableNames(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedSuperuserPGXPool(t)
	for _, tc := range []struct{ name, insert, constraint string }{
		{"product credit specification", `INSERT INTO openrails.products(id,merchant_id,key,display_name,credits_spec) VALUES(uuidv7(),$1,'tokens','Tokens','{"tokens":{"unit":"former-owner/tokens","amount":5}}'::jsonb)`, "products_credit_units_canonical"},
		{"financial history", `INSERT INTO openrails.ledger_accounts(merchant_id,account_type,currency,credits_posted) VALUES($1,'world','former-owner/tokens',37)`, "ledger_accounts_currency_shape"},
		{"catalog definition", `INSERT INTO openrails.catalog_credit_balances(merchant_id,key,unit) VALUES($1,'tokens','former-owner/tokens')`, "catalog_credit_balances_unit_identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(ctx) }()
			mid := uuid.New()
			_, err = tx.Exec(ctx, `INSERT INTO openrails.merchants(id,slug,status) VALUES($1,$2,'active')`, mid, "canonical-"+mid.String())
			require.NoError(t, err)
			_, err = tx.Exec(ctx, tc.insert, mid)
			require.ErrorContains(t, err, tc.constraint, "a reused display name cannot become durable financial identity")
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
