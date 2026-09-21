//go:build integration

package migrate_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/stretchr/testify/require"
)

func TestCollectionPolicyUpgradePreservesLegacyBook(t *testing.T) {
	ctx := t.Context()
	pool := dbtest.SharedSuperuserPGXPool(t)
	schema := "policy_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	all, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	prior := append([]migratekit.Migration(nil), all[:2]...)
	for i := range prior {
		prior[i].Content, err = postgresmigrations.RewriteSchema(prior[i].Content, schema)
		require.NoError(t, err)
	}
	old, err := migratekit.NewPostgresFromPGXPool(pool, config.MigratekitApp)
	require.NoError(t, err)
	defer old.Close()
	require.NoError(t, old.WithSchema(schema).ApplyMigrations(ctx, prior))
	snapshot := func(query string) json.RawMessage {
		var v json.RawMessage
		require.NoError(t, pool.QueryRow(ctx, query).Scan(&v))
		return v
	}
	ledger := snapshot("SELECT jsonb_agg(to_jsonb(m) ORDER BY sequence) FROM public.migrations m WHERE app='openrails' AND schema='" + schema + "'")
	mid, cid := uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		q = strings.ReplaceAll(q, "openrails.", schema+".")
		_, err := pool.Exec(ctx, q, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.merchants(id,slug) VALUES($1,$2)`, mid, mid.String())
	exec(`INSERT INTO openrails.customers(id,merchant_id) VALUES($1,$2)`, cid, mid)
	type cohort struct {
		rail, driver, policy string
		sidecar              bool
	}
	for _, c := range []cohort{{"stripe", "provider", "provider", false}, {"nmi", "provider", "provider", false}, {"nmi", "openrails", "provider_dunning", false}, {"ccbill", "provider", "provider", false}, {"solana", "provider", "engine", true}, {"solana", "provider", "provider", false}} {
		psp, pm, product, price, sub := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
		exec(`INSERT INTO openrails.psps(id,merchant_id,rail,environment,account_id) VALUES($1,$2,$3,'test',$1::text)`, psp, mid, c.rail)
		exec(`INSERT INTO openrails.payment_methods(id,merchant_id,customer_id,psp_id,rail,rebill_driver,rail_customer_ref,initial_transaction_id) VALUES($1,$2,$3,$4,$5,$6,'original-vault','original-initial')`, pm, mid, cid, psp, c.rail, c.driver)
		exec(`INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,$1::text,'Preserved')`, product, mid)
		exec(`INSERT INTO openrails.prices(id,merchant_id,product_id,amount,currency) VALUES($1,$2,$3,5000000,'USD')`, price, mid, product)
		exec(`INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,payment_method_id,rail,rail_subscription_id,status,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$1::text,'active','2026-09-01','2026-10-01')`, sub, mid, cid, product, price, psp, pm, c.rail)
		if c.sidecar {
			exec(`INSERT INTO openrails.solana_subscriptions(merchant_id,subscription_id,subscriber_wallet,authority_pda,subscription_pda,plan_pda,merchant_address,mint,plan_created_at_fingerprint,next_pull_at) VALUES($1,$2,'wallet','authority',$2::text,'plan','merchant','mint',1,'2026-10-01')`, mid, sub)
		}
	}
	tables := []string{"subscriptions", "payment_methods", "products", "prices", "solana_subscriptions"}
	before := map[string]json.RawMessage{}
	for _, table := range tables {
		before[table] = snapshot("SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM " + schema + "." + table + " t")
	}
	for range 2 {
		require.NoError(t, migrate.ApplyPostgresMigrations(ctx, pool, migrate.Options{Schema: schema, HostRiver: true}))
	}
	for _, table := range tables {
		projection := "to_jsonb(t)"
		if table == "subscriptions" {
			projection += "-'collection_policy'"
		}
		after := snapshot("SELECT jsonb_agg(" + projection + " ORDER BY id) FROM " + schema + "." + table + " t")
		require.JSONEq(t, string(before[table]), string(after), table+" must retain all original values")
	}
	afterLedger := snapshot("SELECT jsonb_agg(to_jsonb(m) ORDER BY sequence) FROM public.migrations m WHERE app='openrails' AND schema='" + schema + "' AND sequence IN ('1','2')")
	require.JSONEq(t, string(ledger), string(afterLedger), "published migration records must never be restamped")
	var provider, dunning, engine int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FILTER(WHERE collection_policy='provider'),count(*) FILTER(WHERE collection_policy='provider_dunning'),count(*) FILTER(WHERE collection_policy='engine') FROM "+schema+".subscriptions").Scan(&provider, &dunning, &engine))
	require.Equal(t, 4, provider)
	require.Equal(t, 1, dunning)
	require.Equal(t, 1, engine)
}
