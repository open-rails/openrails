//go:build integration

package postgresmigrations_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/internal/dbtest"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/stretchr/testify/require"
)

// Inspect PostgreSQL's applied schema, not a second SQL parser maintained by tests.
// Behavioral isolation, cross-merchant uniqueness and immutable financial facts
// are exercised in invariantaudit and the owning modules' database workflows.
func TestAppliedBillingSchemaContract(t *testing.T) {
	ctx := t.Context()
	pool := dbtest.SharedSuperuserPGXPool(t)
	for kind, want := range map[string][]string{"r": postgresmigrations.OwnedTables, "v": postgresmigrations.OwnedViews} {
		rows, err := pool.Query(ctx, `SELECT relname FROM pg_class
			WHERE relnamespace='billing'::regnamespace AND relkind=$1 ORDER BY relname`, kind)
		require.NoError(t, err)
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		require.NotEmpty(t, names)
		require.ElementsMatch(t, want, names, "ownership inventory must cover every applied object")
	}

	// A restore receipt must cover all owned tables, including newly added ones.
	var restoreGuard string
	require.NoError(t, pool.QueryRow(ctx, `SELECT pg_get_functiondef('billing.guard_billing_restore_receipt()'::regprocedure)`).Scan(&restoreGuard))
	for _, table := range postgresmigrations.OwnedTables {
		require.Contains(t, restoreGuard, "'"+table+"'", "restore scope: %s", table)
	}

	// Keep merchant scope explicit and indexed even on deployments without RLS.
	rows, err := pool.Query(ctx, `SELECT c.relname, a.attnotnull, a.atttypid='uuid'::regtype,
		NOT a.atthasdef,
		EXISTS(SELECT 1 FROM pg_index i WHERE i.indrelid=c.oid AND i.indisvalid
			AND i.indpred IS NULL AND i.indkey[0]=a.attnum)
		FROM pg_class c JOIN pg_attribute a ON a.attrelid=c.oid
		WHERE c.relnamespace='billing'::regnamespace AND c.relkind='r'
			AND a.attname='merchant_id' AND NOT a.attisdropped`)
	require.NoError(t, err)
	defer rows.Close()
	scoped := 0
	for rows.Next() {
		var table string
		var notNull, uuidType, noDefault, indexed bool
		require.NoError(t, rows.Scan(&table, &notNull, &uuidType, &noDefault, &indexed))
		require.True(t, notNull && uuidType && noDefault && indexed, "explicit indexed merchant scope: %s", table)
		scoped++
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.NotZero(t, scoped, "merchant-index audit must not be vacuous")

	// Financial history must not disappear when a merchant row is deleted.
	for _, table := range strings.Fields(`products prices payment_methods checkout_sessions grants
		payments subscriptions invoices invoice_items invoice_payments money_settings ledger_accounts
		ledger_transfers usage_events entitlements rail_intents rail_customer_accounts maintenance_runs
		reconciliation_findings reconciliation_state notifications billing_policies invoker_spend_limits
		solana_subscriptions merchant_deks merchant_secrets`) {
		var restrict bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint k
			JOIN pg_attribute a ON a.attrelid=k.conrelid AND k.conkey=ARRAY[a.attnum]
			WHERE k.conrelid=to_regclass('billing.' || $1) AND k.contype='f'
				AND k.confrelid='billing.merchants'::regclass AND a.attname='merchant_id'
				AND k.confdeltype='r')`, table).Scan(&restrict))
		require.True(t, restrict, "merchant deletion must preserve %s", table)
	}

	// Exercise the actual currency predicates, including registrable non-fiat codes.
	rows, err = pool.Query(ctx, `SELECT c.relname, COALESCE(pg_get_expr(k.conbin,k.conrelid),'')
		FROM pg_class c JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='currency'
		LEFT JOIN pg_constraint k ON k.conrelid=c.oid AND k.conname=c.relname || '_currency_shape'
		WHERE c.relnamespace='billing'::regnamespace AND c.relkind='r' AND NOT a.attisdropped`)
	require.NoError(t, err)
	type currencyCheck struct{ table, expression string }
	checks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (currencyCheck, error) {
		var c currencyCheck
		err := r.Scan(&c.table, &c.expression)
		return c, err
	})
	require.NoError(t, err)
	require.NotEmpty(t, checks)
	for _, check := range checks {
		require.NotEmpty(t, check.expression, check.table)
		for value, want := range map[string]bool{"USD": true, "USDC": true, "usd": false, "": false, "US D": false} {
			var accepted bool
			require.NoError(t, pool.QueryRow(ctx, "SELECT ("+check.expression+") FROM (SELECT $1::text AS currency) c", value).Scan(&accepted))
			require.Equal(t, want, accepted, "%s.currency: %q", check.table, value)
		}
	}
}

func TestPriceAndRefundAmountConstraints(t *testing.T) {
	f := newPSPFixture(t)
	ctx := t.Context()
	_, err := f.pool.Exec(ctx, `INSERT INTO billing.prices
		(id,merchant_id,product_id,amount,currency,access_duration_hours,auto_renew)
		VALUES($1,$2,$3,-1,'USD',720,true)`, uuid.New(), f.merchant, f.product)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "23514", pgErr.Code)
	require.Equal(t, "prices_amount_nonneg_chk", pgErr.ConstraintName)
	// Refund facts use negative payment amounts; a broad nonnegative CHECK is wrong.
	_, err = f.pool.Exec(ctx, `INSERT INTO billing.payments
		(id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,
		currency,status,purchased_at,psp_id,money_movement)
		VALUES($1,$2,$3,$4,'nmi',$5,-1,-1,'USD','completed',now(),$6,'rail')`,
		uuid.New(), f.merchant, f.customer, f.price, uuid.NewString(), f.pspA)
	require.NoError(t, err)
}
