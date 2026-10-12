//go:build e2e && integration

package ci_test

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

func TestPriceIdentifierMigrationPreservesForeignKeys(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	require.NotEmpty(t, dsn)
	pool := openPool(t, dsn)
	f := &fixture{pool: pool, schema: "e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{f.schema}.Sanitize()+" CASCADE")
	})
	files, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	before := fstest.MapFS{}
	for _, name := range files {
		if strings.HasSuffix(name, "_public_price_identifiers.up.sql") {
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
	mid, product, first, second := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO `+table("merchants")+` (id, slug, status) VALUES ($1, 'legacy-price-test', 'active')`, mid)
	exec(`INSERT INTO `+table("products")+` (merchant_id,id,key,display_name,tier_rank,archived) VALUES ($1,$2,'premium','Premium',0,false)`, mid, product)
	exec(`INSERT INTO `+table("prices")+` (merchant_id,id,product_id,key,amount,currency,archived) VALUES ($1,$2,$3,'buy-usd',1000000,'USD',true)`, mid, first, product)
	exec(`INSERT INTO `+table("prices")+` (merchant_id,id,product_id,key,amount,currency,archived) VALUES ($1,$2,$3,'buy-usd',2000000,'USD',false)`, mid, second, product)
	exec(`CREATE TABLE ` + table("price_reference_probe") + ` (merchant_id uuid, price_id uuid, FOREIGN KEY (merchant_id,price_id) REFERENCES ` + table("prices") + `(merchant_id,id))`)
	exec(`INSERT INTO `+table("price_reference_probe")+` VALUES ($1,$2),($1,$3)`, mid, first, second)
	longProduct, longPrice := uuid.New(), uuid.New()
	productKey, priceKey := "", ""
	for range 40 {
		productKey += ":" + uuid.NewString()
		priceKey += "/" + uuid.NewString()
	}
	exec(`INSERT INTO `+table("products")+` (merchant_id,id,key,display_name,tier_rank,archived) VALUES ($1,$2,$3,'Long legacy key',0,false)`, mid, longProduct, productKey)
	exec(`INSERT INTO `+table("prices")+` (merchant_id,id,product_id,key,amount,currency,archived) VALUES ($1,$2,$3,$4,1000000,'USD',false)`, mid, longPrice, longProduct, priceKey)
	bill, err := openrails.New(t.Context(), f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: pool})
	require.NoError(t, err)
	require.NoError(t, bill.Close(t.Context()))
	expectedLongID, err := billing.NewPriceID(productKey, priceKey, 1)
	require.NoError(t, err)
	require.Greater(t, len(expectedLongID), 2704, "escaped historical keys can exceed a btree entry")
	database, err := db.NewWithPGXPool(pool, f.schema)
	require.NoError(t, err)
	resolved, err := catalog.NewPriceService(database).GetByPublicID(merchant.WithID(t.Context(), billing.MerchantID(mid)), expectedLongID)
	require.NoError(t, err)
	require.Equal(t, longPrice, resolved.ID)
	require.Equal(t, expectedLongID, resolved.PublicID)
	for i, id := range []uuid.UUID{first, second} {
		var public string
		var revision, amount int64
		var archived bool
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT price.public_id,price.revision,price.amount,price.archived FROM `+table("prices")+` price JOIN `+table("price_reference_probe")+` probe ON probe.merchant_id=price.merchant_id AND probe.price_id=price.id WHERE price.id=$1`, id).Scan(&public, &revision, &amount, &archived))
		require.Equal(t, []string{"premium:buy-usd:v1", "premium:buy-usd:v2"}[i], public)
		require.EqualValues(t, i, revision)
		require.EqualValues(t, (i+1)*1_000_000, amount)
		require.Equal(t, i == 0, archived)
	}
}
