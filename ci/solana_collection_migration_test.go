//go:build e2e && integration

package ci_test

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

// Upgrade a real pre-cutover schema: only the old Solana classification
// changes, and ordinary writes still cannot change collection authority.
func TestSolanaCollectionMigrationUpdatesOnlySolana(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	require.NotEmpty(t, dsn)
	pool := openPool(t, dsn)
	f := &fixture{pool: pool, schema: "e2e_solana_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{f.schema}.Sanitize()+" CASCADE")
	})
	files, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	before := fstest.MapFS{}
	for _, name := range files {
		if name == "0103_solana_engine_collection.up.sql" {
			break
		}
		body, err := fs.ReadFile(postgresmigrations.FS, name)
		require.NoError(t, err)
		before[name] = &fstest.MapFile{Data: body}
	}
	require.Less(t, len(before), len(files))
	cfg := f.config()
	cfg.DB = &openrails.DBConfig{URL: dsn}
	application, err := app.BootstrapWithOptions(t.Context(), &cfg, &app.BootstrapOptions{PGXPool: pool, Migrations: before})
	require.NoError(t, err)
	require.NoError(t, application.Close(t.Context()))
	table := func(name string) string { return pgx.Identifier{f.schema, name}.Sanitize() }
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(t.Context(), sql, args...)
		require.NoError(t, err)
	}
	mid, product, price := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO `+table("merchants")+` (id, slug, status) VALUES ($1, 'solana-collection-cutover', 'active')`, mid)
	exec(`INSERT INTO `+table("products")+` (merchant_id,id,key,display_name,tier_rank,archived) VALUES ($1,$2,'membership','Membership',0,false)`, mid, product)
	exec(`INSERT INTO `+table("prices")+` (merchant_id,id,product_id,key,amount,currency,billing_interval_hours,access_duration_hours,archived) VALUES ($1,$2,$3,'monthly',23000000,'USD',720,720,false)`, mid, price, product)
	now := time.Now().UTC().Truncate(time.Microsecond)
	subs := map[string]uuid.UUID{}
	for rail, policy := range map[string]string{"solana": "provider", "stripe": "provider", "nmi": "nmi_schedule"} {
		environment := "sandbox"
		if rail == "solana" {
			environment = "devnet"
		}
		customer, psp, id := uuid.New(), uuid.New(), uuid.New()
		subs[rail] = id
		exec(`INSERT INTO `+table("customers")+` (merchant_id,id) VALUES ($1,$2)`, mid, customer)
		exec(`INSERT INTO `+table("psps")+` (merchant_id,id,key,rail,account_id,environment) VALUES ($1,$2,$3,$3,$4,$5)`, mid, psp, rail, "migration-"+rail, environment)
		exec(`INSERT INTO `+table("subscriptions")+` (merchant_id,id,customer_id,product_id,price_id,psp_id,rail,collection_policy,rail_subscription_id,status,started_at,current_period_starts_at,current_period_ends_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'active',$10,$10,$11)`, mid, id, customer, product, price, psp, rail, policy, "accepted-"+rail, now, now.Add(720*time.Hour))
	}
	engine, err := openrails.New(t.Context(), f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: pool})
	require.NoError(t, err)
	require.NoError(t, engine.Close(t.Context()))
	for rail, want := range map[string]string{"solana": "engine", "stripe": "provider", "nmi": "nmi_schedule"} {
		var policy, binding string
		var end time.Time
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT collection_policy, rail_subscription_id, current_period_ends_at FROM `+table("subscriptions")+` WHERE merchant_id=$1 AND id=$2`, mid, subs[rail]).Scan(&policy, &binding, &end))
		require.Equal(t, want, policy)
		require.Equal(t, "accepted-"+rail, binding)
		require.Equal(t, now.Add(720*time.Hour), end)
	}
	_, err = pool.Exec(t.Context(), `UPDATE `+table("subscriptions")+` SET collection_policy='provider' WHERE merchant_id=$1 AND id=$2`, mid, subs["solana"])
	require.ErrorContains(t, err, "collection policy is immutable")
}
