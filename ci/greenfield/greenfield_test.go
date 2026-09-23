//go:build greenfield && integration

// Package greenfield contains the first replacement CI slice. It deliberately
// talks only to the public embedded API: the old integration harness and
// application tables are not part of this fixture.
package greenfield_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
)

type fixture struct {
	pool   *pgxpool.Pool
	schema string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_GREENFIELD_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN"))
	}
	if dsn == "" {
		t.Fatal("OPENRAILS_GREENFIELD_DSN must point at a disposable PostgreSQL database")
	}

	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(t.Context()))

	f := &fixture{pool: pool, schema: "greenfield_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = f.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{f.schema}.Sanitize()+" CASCADE")
		f.pool.Close()
	})

	// The fixture owns no migration files. OpenRails applies its own public
	// migrations, and the second call proves replay is safe before a runtime is
	// constructed.
	for range 2 {
		require.NoError(t, embed.ApplyMigrations(t.Context(), pool, embed.MigrationOptions{
			Schema:      f.schema,
			River:       embed.RiverManagedByOpenRails(f.schema),
			RuntimePool: pool,
		}))
	}
	return f
}

func (f *fixture) runtime(t *testing.T, slug string) (*embed.Runtime, *openrails.Client) {
	t.Helper()
	runtime, err := embed.New(t.Context(), embed.Options{
		Config: &config.Config{
			TestMode:            config.CredentialPostureSandbox,
			AllowCatalogUpdates: true,
			ProviderWriteMode:   config.ProviderWriteModeReadOnly,
			DB:                  &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
		},
		Merchant: &embed.MerchantDeclaration{
			Slug:   slug,
			Config: embed.MerchantConfig{DisplayName: slug},
		},
		PGXPool: f.pool,
		River:   embed.RiverManagedByOpenRails(f.schema),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	client, err := runtime.Client()
	require.NoError(t, err)
	return runtime, client
}

func (f *fixture) dsn(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_GREENFIELD_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN"))
	}
	return dsn
}

func TestFreshBootstrapAndReplay(t *testing.T) {
	f := newFixture(t)
	_, client := f.runtime(t, "bootstrap-"+uuid.NewString()[:8])

	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{
		Key:              "welcome-" + uuid.NewString()[:8],
		DisplayName:      "Welcome",
		EntitlementsSpec: map[string]*int{"content:welcome": nil},
	})
	require.NoError(t, err)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{
		ProductID:  product.ID,
		Key:        "welcome-usd-" + uuid.NewString()[:8],
		UnitAmount: 1_000_000,
		Currency:   "USD",
	})
	require.NoError(t, err)
	require.Equal(t, product.ID, price.ProductID)

	offers, err := client.ListOffersForEntitlement(t.Context(), "content:welcome", openrails.OfferListParams{Kind: openrails.OfferPermanent})
	require.NoError(t, err)
	require.Len(t, offers.Data, 1)
	require.Equal(t, price.ID, offers.Data[0].PriceID)
}

func TestMerchantCatalogAndCustomerIsolation(t *testing.T) {
	f := newFixture(t)
	_, alice := f.runtime(t, "merchant-a-"+uuid.NewString()[:8])
	_, bob := f.runtime(t, "merchant-b-"+uuid.NewString()[:8])

	productA, err := alice.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "alice-post", DisplayName: "Alice post"})
	require.NoError(t, err)
	productB, err := bob.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "bob-post", DisplayName: "Bob post"})
	require.NoError(t, err)

	_, err = alice.Products.Retrieve(t.Context(), productB.ID)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	page, err := alice.Products.List(t.Context(), &openrails.ProductListParams{})
	require.NoError(t, err)
	require.EqualValues(t, 1, page.Total)
	require.Equal(t, productA.ID, page.Items[0].ID)

	customerA := uuid.NewString()
	customerB := uuid.NewString()
	_, err = alice.EnsureCustomer(t.Context(), customerA)
	require.NoError(t, err)
	_, err = bob.EnsureCustomer(t.Context(), customerB)
	require.NoError(t, err)
	_, err = alice.GrantEntitlement(t.Context(), customerA, openrails.GrantEntitlementRequest{Entitlement: "content:" + productA.Key})
	require.NoError(t, err)

	owned, err := alice.CheckEntitlements(t.Context(), customerA, []string{"content:" + productA.Key}, time.Time{})
	require.NoError(t, err)
	require.True(t, owned["content:"+productA.Key])
	foreign, err := bob.CheckEntitlements(t.Context(), customerA, []string{"content:" + productA.Key}, time.Time{})
	require.NoError(t, err)
	require.False(t, foreign["content:"+productA.Key])
}

func TestCatalogEnsureIsIdempotent(t *testing.T) {
	f := newFixture(t)
	_, client := f.runtime(t, "idempotent-"+uuid.NewString()[:8])
	key := "stable-product-" + uuid.NewString()[:8]

	first, err := client.Products.Ensure(t.Context(), &openrails.ProductCreateParams{Key: key, DisplayName: "First title"})
	require.NoError(t, err)
	second, err := client.Products.Ensure(t.Context(), &openrails.ProductCreateParams{Key: key, DisplayName: "Changed title"})
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.DisplayName, second.DisplayName)

	read, err := client.Products.RetrieveByKey(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, first.ID, read.ID)
	require.Equal(t, first.DisplayName, read.DisplayName)
}
