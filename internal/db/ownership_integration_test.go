//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

func TestOperationalForeignKeysCarryMerchantOwnership(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)
	rows, err := pool.Query(ctx, `
		SELECT child.relname || '.' || fk.conname
		FROM pg_constraint fk
		JOIN pg_class child ON child.oid=fk.conrelid
		JOIN pg_namespace ns ON ns.oid=child.relnamespace AND ns.nspname='openrails'
		JOIN pg_attribute cm ON cm.attrelid=fk.conrelid AND cm.attname='merchant_id'
		JOIN pg_attribute pm ON pm.attrelid=fk.confrelid AND pm.attname='merchant_id'
		WHERE fk.contype='f' AND NOT EXISTS (
			SELECT 1 FROM generate_subscripts(fk.conkey,1) i
			WHERE fk.conkey[i]=cm.attnum AND fk.confkey[i]=pm.attnum
		)
		UNION ALL
		SELECT child.relname || '.' || key.relname
		FROM pg_index ix
		JOIN pg_class child ON child.oid=ix.indrelid
		JOIN pg_class key ON key.oid=ix.indexrelid
		JOIN pg_namespace ns ON ns.oid=child.relnamespace AND ns.nspname='openrails'
		JOIN pg_attribute payer ON payer.attrelid=ix.indrelid
		  AND payer.attname IN ('customer_id','payer_id') AND payer.attnum=ANY(ix.indkey)
		JOIN pg_attribute owner ON owner.attrelid=ix.indrelid AND owner.attname='merchant_id'
		WHERE ix.indisunique AND NOT owner.attnum=ANY(ix.indkey)`)
	require.NoError(t, err)
	defer rows.Close()
	var unscoped []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		unscoped = append(unscoped, name)
	}
	require.NoError(t, rows.Err())
	require.Empty(t, unscoped)
	var primaryKey []string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT array_agg(a.attname ORDER BY k.ordinality)
		FROM pg_constraint c, unnest(c.conkey) WITH ORDINALITY k(attnum,ordinality), pg_attribute a
		WHERE c.conrelid='openrails.customers'::regclass AND c.contype='p'
		AND a.attrelid=c.conrelid AND a.attnum=k.attnum`).Scan(&primaryKey))
	require.Equal(t, []string{"merchant_id", "id"}, primaryKey)
	var historyLinks int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint c JOIN pg_attribute a
		ON a.attrelid=c.conrelid AND a.attnum=ANY(c.conkey)
		WHERE c.conrelid='openrails.ledger_transfers'::regclass AND c.contype='f'
		AND a.attname IN ('grant_id','customer_id','invoice_id')`).Scan(&historyLinks))
	require.Zero(t, historyLinks, "immutable attribution must not acquire control-plane deletion dependencies")
}

type ownedBillingRows struct {
	pool                                                       *pgxpool.Pool
	merchant, payer, extraPayer, product, price, psp           uuid.UUID
	method, extraMethod, subscription, payment, invoice, grant uuid.UUID
}

func seedOwnedBillingRows(t *testing.T, ctx context.Context, payer uuid.UUID) ownedBillingRows {
	t.Helper()
	r := ownedBillingRows{
		merchant: uuid.New(), payer: payer, extraPayer: uuid.New(), product: uuid.New(), price: uuid.New(),
		method: uuid.New(), extraMethod: uuid.New(), subscription: uuid.New(), payment: uuid.New(), invoice: uuid.New(), grant: uuid.New(),
	}
	r.pool = dbtest.SharedMerchantPool(t, r.merchant)
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := r.pool.Exec(ctx, query, args...)
		require.NoError(t, err)
	}
	exec("INSERT INTO openrails.merchants(id,slug) VALUES($1,$2)", r.merchant, "ownership-"+r.merchant.String())
	exec("INSERT INTO openrails.customers(id,merchant_id) VALUES($1,$3),($2,$3)", payer, r.extraPayer, r.merchant)
	r.psp = dbtest.EnsureTestPSP(ctx, t, r.pool, r.merchant, "nmi")
	exec("INSERT INTO openrails.products(id,merchant_id,key,display_name,tier_group) VALUES($1,$2,'product','Product','membership')", r.product, r.merchant)
	exec("INSERT INTO openrails.prices(id,merchant_id,product_id,key,amount,currency) VALUES($1,$2,$3,'price',1000000,'USD')", r.price, r.merchant, r.product)
	for method, customer := range map[uuid.UUID]uuid.UUID{r.method: payer, r.extraMethod: r.extraPayer} {
		exec(`INSERT INTO openrails.payment_methods(id,merchant_id,customer_id,psp_id,rail,initial_transaction_id,rail_method_ref)
			VALUES($1,$2,$3,$4,'nmi',$5::text,$5::text)`, method, r.merchant, customer, r.psp, method.String())
	}
	exec(`INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,payment_method_id,rail,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,'nmi','active')`, r.subscription, r.merchant, payer, r.product, r.price, r.psp, r.method)
	exec(`INSERT INTO openrails.payments(id,merchant_id,customer_id,price_id,subscription_id,psp_id,rail,transaction_id,amount,list_amount,currency)
		VALUES($1,$2,$3,$4,$5,$6,'nmi',$7,1000000,1000000,'USD')`, r.payment, r.merchant, payer, r.price, r.subscription, r.psp, r.payment.String())
	exec(`INSERT INTO openrails.invoices(id,merchant_id,customer_id,currency,period_from,period_to)
		VALUES($1,$2,$3,'USD',now(),now()+interval '1 day')`, r.invoice, r.merchant, payer)
	exec(`INSERT INTO openrails.grants(id,merchant_id,customer_id,kind,source_type,payment_id)
		VALUES($1,$2,$3,'entitlement','purchase',$4)`, r.grant, r.merchant, payer, r.payment)
	return r
}

func TestOwnedRowsRejectForeignParentsAndPreserveDeletionScope(t *testing.T) {
	ctx := context.Background()
	payer := uuid.New()
	a, b := seedOwnedBillingRows(t, ctx, payer), seedOwnedBillingRows(t, ctx, payer)
	var privileged bool
	require.NoError(t, a.pool.QueryRow(ctx, "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&privileged))
	require.False(t, privileged)
	deposit, err := ledger.New(gen.New(a.pool), a.merchant).Deposit(ctx, payer, "USD", 1000,
		ledger.Coord{Operation: ledger.OpDeposit, Source: "ownership", SourceID: uuid.NewString()}, uuid.Nil)
	require.NoError(t, err)
	cases := []struct {
		name, constraint, query string
		args                    []any
		index                   int
		foreign                 any
	}{
		{"catalog", "prices_product_id_fkey", `INSERT INTO openrails.prices(merchant_id,product_id,key,amount,currency) VALUES($1,$2,'second',2000000,'USD')`, []any{a.merchant, a.product}, 1, b.product},
		{"customer", "ledger_accounts_customer_fk", `INSERT INTO openrails.ledger_accounts(merchant_id,customer_id,account_type,currency) VALUES($1,$2,'customer_balance','EUR')`, []any{a.merchant, a.extraPayer}, 1, b.extraPayer},
		{"payment_method", "payment_methods_psp_fk", `INSERT INTO openrails.payment_methods(merchant_id,customer_id,psp_id,rail,initial_transaction_id) VALUES($1,$2,$3,'nmi','new-method')`, []any{a.merchant, payer, a.psp}, 2, b.psp},
		{"subscription_payer", "subscriptions_payment_method_id_fkey", `INSERT INTO openrails.subscriptions(merchant_id,customer_id,product_id,price_id,psp_id,payment_method_id,rail,status) VALUES($1,$2,$3,$4,$5,$6,'nmi','unknown')`, []any{a.merchant, payer, a.product, a.price, a.psp, a.method}, 5, a.extraMethod},
		{"checkout", "checkout_sessions_payment_id_fkey", `INSERT INTO openrails.checkout_sessions(merchant_id,customer_id,price_id,psp_id,payment_id,mode,rail,status,amount,currency) VALUES($1,$2,$3,$4,$5,'one_off','nmi','pending',1000000,'USD')`, []any{a.merchant, payer, a.price, a.psp, a.payment}, 4, b.payment},
		{"invoice_unit", "invoice_items_invoice_fk", `INSERT INTO openrails.invoice_items(merchant_id,customer_id,invoice_id,currency,source_type,source_id,invoice_at,amount,status) VALUES($1,$2,$3,$4,'usage','ownership-item',now(),100,'invoiced')`, []any{a.merchant, payer, a.invoice, "USD"}, 3, "EUR"},
		{"grant", "grants_payment_fk", `INSERT INTO openrails.grants(merchant_id,customer_id,kind,source_type,payment_id) VALUES($1,$2,'entitlement','purchase',$3)`, []any{a.merchant, payer, a.payment}, 2, b.payment},
		{"entitlement_payer", "entitlements_grant_fk", `INSERT INTO openrails.entitlements(merchant_id,customer_id,grant_id,source_id,source_type,entitlement,start_at) VALUES($1,$2,$3,$3,'grant','ownership-access',now())`, []any{a.merchant, payer, a.grant}, 1, a.extraPayer},
		{"usage_unit", "usage_events_ledger_transfer_fk", `INSERT INTO openrails.usage_events(merchant_id,customer_id,currency,ledger_transfer_id,invoker_id,event_type,amount,source,source_id) VALUES($1,$2,$3,$4,'invoker','usage',1000,'ownership','event')`, []any{a.merchant, payer, "USD", deposit.ID}, 2, "EUR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := append([]any(nil), tc.args...)
			bad[tc.index] = tc.foreign
			_, err := a.pool.Exec(ctx, tc.query, bad...)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			require.Equal(t, "23503", pgErr.Code)
			require.Equal(t, tc.constraint, pgErr.ConstraintName)
			_, err = a.pool.Exec(ctx, tc.query, tc.args...)
			require.NoError(t, err, "the corresponding owned relationship remains legal")
		})
	}

	// Nullable references clear their reference column, never the owner tuple.
	_, err = a.pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id=$1", a.method)
	require.NoError(t, err)
	var merchantID, customerID uuid.UUID
	var method *uuid.UUID
	require.NoError(t, a.pool.QueryRow(ctx, "SELECT merchant_id,customer_id,payment_method_id FROM openrails.subscriptions WHERE id=$1", a.subscription).Scan(&merchantID, &customerID, &method))
	require.Equal(t, a.merchant, merchantID)
	require.Equal(t, payer, customerID)
	require.Nil(t, method)

	// Both merchants can hold this other subject too. Deleting A's relationship
	// nulls only A's optional history link; B's customer and history remain.
	temporary := uuid.New()
	for _, r := range []ownedBillingRows{a, b} {
		_, err = r.pool.Exec(ctx, "INSERT INTO openrails.customers(id,merchant_id) VALUES($1,$2)", temporary, r.merchant)
		require.NoError(t, err)
		_, err = r.pool.Exec(ctx, `INSERT INTO openrails.imported_dunning_history(merchant_id,customer_id,event_type,rail,occurred_at,source) VALUES($1,$2,'notice','nmi',now(),'ownership-delete')`, r.merchant, temporary)
		require.NoError(t, err)
	}
	_, err = a.pool.Exec(ctx, "DELETE FROM openrails.customers WHERE id=$1", temporary)
	require.NoError(t, err)
	var customer *uuid.UUID
	require.NoError(t, a.pool.QueryRow(ctx, "SELECT merchant_id,customer_id FROM openrails.imported_dunning_history WHERE source='ownership-delete'").Scan(&merchantID, &customer))
	require.Equal(t, a.merchant, merchantID)
	require.Nil(t, customer)
	require.NoError(t, b.pool.QueryRow(ctx, "SELECT merchant_id,customer_id FROM openrails.imported_dunning_history WHERE source='ownership-delete'").Scan(&merchantID, &customer))
	require.Equal(t, b.merchant, merchantID)
	require.Equal(t, temporary, *customer)
}
