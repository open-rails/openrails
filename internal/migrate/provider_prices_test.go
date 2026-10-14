package migrate

import (
	"context"
	"crypto/md5"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/migratekit"
	"github.com/stretchr/testify/require"
)

// A populated upgrade separates provider-owned agreements without changing the
// accepted purchases, payments, access or the engine-owned subscription.
func TestProviderPricesMigrationPreservesTheExistingBook(t *testing.T) {
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		t.Skip("SQLC_DATABASE_URL is required for the populated migration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := "provider_prices_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	migrations, err := loadMigrations(schema)
	require.NoError(t, err)
	at := -1
	for i, m := range migrations {
		if strings.HasPrefix(m.Name, "0104_") {
			at = i
		}
	}
	require.Positive(t, at)
	migrator, err := migratekit.NewPostgresFromPGXPool(pool, "openrails")
	require.NoError(t, err)
	defer migrator.Close()
	migrator.WithSchema(schema).WithStrictIntegrity().WithRender(loadMigrations)
	require.NoError(t, migrator.ApplyMigrations(ctx, migrations[:at]))
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, strings.ReplaceAll(sql, "billing.", schema+"."), args...)
		require.NoError(t, err)
	}
	query := func(sql string, args ...any) pgx.Row {
		return pool.QueryRow(ctx, strings.ReplaceAll(sql, "billing.", schema+"."), args...)
	}
	mid, product, price, next := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO billing.merchants(id,slug,status) VALUES($1,'provider-prices','active')`, mid)
	exec(`INSERT INTO billing.products(merchant_id,id,key,display_name,tier_rank,archived) VALUES($1,$2,'premium','Premium',0,false)`, mid, product)
	for _, p := range []struct {
		id       uuid.UUID
		key      string
		archived bool
	}{{price, "monthly", false}, {next, "next", false}, {uuid.New(), "monthly-nmi", true}} {
		exec(`INSERT INTO billing.prices(merchant_id,id,product_id,key,amount,currency,archived,access_duration_hours,billing_interval_hours) VALUES($1,$2,$3,$4,23000000,'USD',$5,720,720)`, mid, p.id, product, p.key, p.archived)
	}
	type agreement struct {
		key, rail, policy    string
		archived, superseded bool
		psp, sub             uuid.UUID
	}
	rows := []agreement{{"nmi", "nmi", "nmi_schedule", false, false, uuid.New(), uuid.New()}, {"stripe", "stripe", "provider", true, false, uuid.New(), uuid.New()}, {"ccbill", "ccbill", "provider", false, false, uuid.New(), uuid.New()}, {"old-gateway", "nmi", "nmi_schedule", true, true, uuid.New(), uuid.New()}, {"solana", "solana", "engine", false, false, uuid.New(), uuid.New()}, {"nmi", "nmi", "engine", false, false, uuid.Nil, uuid.New()}}
	start := time.Now().UTC().Add(-24 * time.Hour)
	end := start.Add(720 * time.Hour)
	for i := range rows {
		r := &rows[i]
		if r.psp == uuid.Nil {
			r.psp = rows[0].psp
		} else {
			environment := "test"
			if r.rail == "solana" {
				environment = "devnet"
			}
			exec(`INSERT INTO billing.psps(merchant_id,id,key,rail,environment,account_id,archived,superseded_at) VALUES($1,$2,$3,$4,$5,$2::text,$6,$7)`, mid, r.psp, r.key, r.rail, environment, r.archived, func() *time.Time {
				if r.superseded {
					return &start
				}
				return nil
			}())
			for _, bound := range []uuid.UUID{price, next} {
				var plan, ref, flex, rbo, pda *string
				switch r.rail {
				case "nmi":
					plan = new("plan-" + bound.String())
				case "stripe":
					ref = new("price-" + bound.String())
				case "ccbill":
					flex = new("flex-" + bound.String())
					rbo = new("rbo-" + bound.String())
				case "solana":
					pda = new("pda-" + bound.String())
				}
				exec(`INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,plan_id,price_ref,flex_id,recurring_billing_option_id,plan_pda,configuration) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'{}')`, mid, bound, r.psp, plan, ref, flex, rbo, pda)
			}
		}
		customer := uuid.New()
		exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid, customer)
		remote := new(fmt.Sprintf("agreement-%d", i))
		if r.policy == "engine" && r.rail != "solana" {
			remote = nil
		}
		exec(`INSERT INTO billing.subscriptions(merchant_id,id,customer_id,product_id,price_id,psp_id,status,rail,collection_policy,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at,access_duration_hours_snapshot) VALUES($1,$2,$3,$4,$5,$6,'active',$7,$8,$9,$10,$10,$11,720)`, mid, r.sub, customer, product, price, r.psp, r.rail, r.policy, remote, start, end)
		exec(`INSERT INTO billing.scheduled_changes(merchant_id,subscription_id,from_price_id,price_id,effective_at,source,status,acknowledged_short_notice) VALUES($1,$2,$3,$4,$5,'change','scheduled',false)`, mid, r.sub, price, next, end)
		order := uuid.New()
		exec(`INSERT INTO billing.orders(merchant_id,id,customer_id,origin,status,payment_status,currency,total,expires_at,embedded) VALUES($1,$2,$3,'merchant','open','requires_payment_method','USD',23000000,$4,false)`, mid, order, customer, end)
		exec(`INSERT INTO billing.order_lines(merchant_id,order_id,customer_id,position,price_id,product_id,description,unit_amount,amount,ownership,billing_interval_hours,access_duration_hours) VALUES($1,$2,$3,0,$4,$5,'Premium',23000000,23000000,'extend',720,720)`, mid, order, customer, price, product)
		exec(`INSERT INTO billing.payments(merchant_id,customer_id,channel,transaction_id,amount,list_amount,currency,status,subscription_id,purchased_at,order_id) VALUES($1,$2,'manual',$3,23000000,23000000,'USD','completed',$4,$5,$6)`, mid, customer, "manual-"+r.sub.String(), r.sub, start, order)
		grant := uuid.New()
		exec(`INSERT INTO billing.grants(merchant_id,id,customer_id,product_id,kind,source_type,source_id,event,spec_snapshot,starts_at,ends_at) VALUES($1,$2,$3,$4,'entitlement','subscription',$5,'grant','{"entitlements":["read"]}',$6,$7)`, mid, grant, customer, product, r.sub.String(), start, end)
		exec(`INSERT INTO billing.product_access(merchant_id,customer_id,product_id,grant_id,source_type,source_id,starts_at,ends_at) VALUES($1,$2,$3,$4,'subscription',$5,$6,$7)`, mid, customer, product, grant, r.sub.String(), start, end)
	}
	empty := uuid.New()
	exec(`INSERT INTO billing.prices(merchant_id,id,product_id,key,amount,currency,archived,access_duration_hours) VALUES($1,$2,$3,'once',5000000,'USD',false,720)`, mid, empty, product)
	exec(`INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,configuration) VALUES($1,$2,$3,'{}')`, mid, empty, rows[0].psp)
	snapshots := map[string]string{}
	for _, table := range []string{"order_lines", "payments", "grants", "product_access", "subscription_status_transitions"} {
		var data string
		require.NoError(t, query("SELECT jsonb_agg(to_jsonb(r) ORDER BY id)::text FROM billing."+table+" r").Scan(&data))
		snapshots[table] = data
	}
	require.NoError(t, migrator.ApplyMigrations(ctx, migrations))
	split := func(p, psp uuid.UUID) uuid.UUID { return uuid.UUID(md5.Sum([]byte(p.String() + ":" + psp.String()))) }
	for _, r := range rows {
		var got, from, to uuid.UUID
		var policy string
		var paidEnd time.Time
		require.NoError(t, query(`SELECT price_id,collection_policy,current_period_ends_at FROM billing.subscriptions WHERE id=$1`, r.sub).Scan(&got, &policy, &paidEnd))
		require.Equal(t, r.policy, policy)
		require.WithinDuration(t, end, paidEnd, time.Microsecond)
		expected := price
		if r.policy != "engine" {
			expected = split(price, r.psp)
		}
		require.Equal(t, expected, got, r.key+" "+r.policy)
		require.NoError(t, query(`SELECT from_price_id,price_id FROM billing.scheduled_changes WHERE subscription_id=$1`, r.sub).Scan(&from, &to))
		require.Equal(t, expected, from)
		expectedNext := next
		if r.policy != "engine" {
			expectedNext = split(next, r.psp)
		}
		require.Equal(t, expectedNext, to)
		if r.rail != "solana" && r.policy != "engine" {
			var owned bool
			var key string
			require.NoError(t, query(`SELECT provider_owned,key FROM billing.prices WHERE id=$1`, got).Scan(&owned, &key))
			require.True(t, owned)
			if r.key == "nmi" {
				require.Contains(t, key, "monthly-nmi-provider-", "an existing price key cannot collide")
			} else if r.superseded {
				require.Equal(t, "monthly-old-gateway-"+r.psp.String()[:8], key)
			} else {
				require.Equal(t, "monthly-"+r.key, key)
			}
		}
	}
	for table, want := range snapshots {
		var got string
		require.NoError(t, query("SELECT jsonb_agg(to_jsonb(r) ORDER BY id)::text FROM billing."+table+" r").Scan(&got))
		require.Equal(t, want, got, table+" keeps accepted history")
	}
	var emptyBindings, phantomProviders int
	require.NoError(t, query(`SELECT count(*) FROM billing.price_psp_bindings WHERE price_id=$1`, empty).Scan(&emptyBindings))
	require.Zero(t, emptyBindings, "a blank card eligibility binding is removed")
	require.NoError(t, query(`SELECT count(*) FROM billing.prices WHERE key='once-nmi'`).Scan(&phantomProviders))
	require.Zero(t, phantomProviders, "a blank binding is not a provider price")
	var count int
	require.NoError(t, query(`SELECT count(*) FROM billing.price_psp_bindings WHERE price_id=$1`, price).Scan(&count))
	require.Equal(t, 1, count, "ours retains only its Solana plan")
	_, err = pool.Exec(ctx, strings.ReplaceAll(`INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,configuration) VALUES($1,$2,$3,'{}')`, "billing.", schema+"."), mid, price, rows[0].psp)
	require.ErrorContains(t, err, "our prices bind only Solana")
	_, err = pool.Exec(ctx, strings.ReplaceAll(`INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,configuration) VALUES($1,$2,$3,'{}')`, "billing.", schema+"."), mid, split(price, rows[0].psp), rows[1].psp)
	require.ErrorContains(t, err, "binds one PSP")
}
