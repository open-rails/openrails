//go:build e2e && integration

package ci_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// catalogQueries counts the statements a pool runs on the catalog's tables.
type catalogQueries struct{ n atomic.Int64 }

func (q *catalogQueries) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if sql := strings.ToLower(data.SQL); strings.Contains(sql, "products") || strings.Contains(sql, "prices") || strings.Contains(sql, "meters") {
		q.n.Add(1)
	}
	return ctx
}

func (*catalogQueries) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// uncached turns every cached read off: the oracle a cached read must match.
var uncached = openrails.ReadCache{openrails.ReadCatalog: {}, openrails.ReadMeters: {}, openrails.ReadPublicConfig: {}, openrails.ReadMerchantConfiguration: {}}

// An embedded engine answers repeated catalog reads from memory with what an
// uncached read answers, shows its own writes at once, and leaves customer
// and money reads live. Another replica's write shows after RefreshAfter.
func TestEmbeddedReadCache(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	slug := "reads-" + uuid.NewString()[:8]
	declare := func(cache openrails.ReadCache) openrails.Config {
		cfg := f.config()
		cfg.Merchants = map[string]openrails.MerchantDeclaration{slug: {DisplayName: slug}}
		cfg.ReadCache = cache
		return cfg
	}
	queries := &catalogQueries{}
	poolConfig, err := pgxpool.ParseConfig(f.dsn(t))
	require.NoError(t, err)
	poolConfig.ConnConfig.Tracer = queries
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, shop := f.boot(t, declare(nil), openrails.Deps{FXTransport: testFX.Transport(), Postgres: pool})
	// Two more replicas of the same merchant: one caching nothing, one
	// refreshing every 200ms.
	_, live := f.boot(t, declare(uncached), openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	_, replica := f.boot(t, declare(openrails.ReadCache{openrails.ReadCatalog: {RefreshAfter: 200 * time.Millisecond, MaxStale: time.Minute}}), openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	require.Equal(t, shop.MerchantID(), live.MerchantID())

	pro, err := shop.CreateProduct(ctx, billing.CreateProductParams{Key: "pro", DisplayName: "Pro", Entitlements: []string{"pro"}})
	require.NoError(t, err)
	price, err := shop.CreatePrice(ctx, billing.CreatePriceParams{ProductID: pro.ID, Key: "pro-usd", UnitAmount: 5_000_000, Currency: "USD"})
	require.NoError(t, err)
	granted, err := shop.CreateProduct(ctx, billing.CreateProductParams{Key: "granted", DisplayName: "Granted only", Entitlements: []string{"pro"}})
	require.NoError(t, err)
	retired, err := shop.CreateProduct(ctx, billing.CreateProductParams{Key: "retired", DisplayName: "Retired", Archived: true})
	require.NoError(t, err)
	_, err = shop.CreatePrice(ctx, billing.CreatePriceParams{ProductID: retired.ID, Key: "retired-usd", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	_, err = shop.SetMeter(ctx, "tokens", billing.SetMeterParams{EventType: "tokens", Aggregation: catalog.AggregationSum, ValueProperty: "count"})
	require.NoError(t, err)

	yes, no := true, false
	missing := billing.ProductID(uuid.New())
	keys := []string{"retired", "pro", "missing", "granted", "pro"}
	reads := map[string]func(*openrails.Client) (any, error){
		"product": func(c *openrails.Client) (any, error) { return c.GetProduct(ctx, pro.ID) },
		"price":   func(c *openrails.Client) (any, error) { return c.GetPrice(ctx, price.ID, billing.GetPriceParams{}) },
		"by keys": func(c *openrails.Client) (any, error) {
			return c.ListProducts(ctx, billing.ProductListParams{Keys: keys})
		},
		"on sale by keys": func(c *openrails.Client) (any, error) {
			return c.ListProducts(ctx, billing.ProductListParams{Keys: keys, Archived: &no, ForSale: &yes})
		},
		"archived by keys": func(c *openrails.Client) (any, error) {
			return c.ListProducts(ctx, billing.ProductListParams{Keys: keys, Archived: &yes})
		},
		"by ids": func(c *openrails.Client) (any, error) {
			return c.ListProducts(ctx, billing.ProductListParams{IDs: []billing.ProductID{retired.ID, pro.ID, granted.ID}})
		},
		"by entitlement": func(c *openrails.Client) (any, error) {
			return c.ListProducts(ctx, billing.ProductListParams{Entitlements: []string{"pro"}})
		},
		"first page": func(c *openrails.Client) (any, error) {
			return c.ListProducts(ctx, billing.ProductListParams{PageRequest: billing.PageRequest{Limit: 2}})
		},
		"prices of product": func(c *openrails.Client) (any, error) {
			return c.ListPrices(ctx, billing.PriceListParams{ProductKey: "pro"})
		},
		"meter":           func(c *openrails.Client) (any, error) { return c.GetMeter(ctx, "tokens") },
		"meters":          func(c *openrails.Client) (any, error) { return c.ListMeters(ctx, billing.PageRequest{}) },
		"missing product": func(c *openrails.Client) (any, error) { return c.GetProduct(ctx, missing) },
	}
	same := func(why string) {
		t.Helper()
		for name, read := range reads {
			want, wantErr := read(live)
			got, gotErr := read(shop)
			if wantErr != nil {
				require.Equal(t, wantErr.Error(), gotErr.Error(), "%s %s", name, why)
				continue
			}
			require.NoError(t, gotErr, "%s %s", name, why)
			require.Equal(t, want, got, "%s %s", name, why)
		}
	}
	same("matches the uncached read")
	page, err := shop.ListProducts(ctx, billing.ProductListParams{Keys: keys, Archived: &no, ForSale: &yes})
	require.NoError(t, err)
	require.Len(t, page.Items, 1, "filters hold: only pro is live and on sale")

	before := queries.n.Load()
	for range 20 {
		for name, read := range reads {
			_, err := read(shop)
			if name != "missing product" {
				require.NoError(t, err, name)
			}
		}
	}
	require.Equal(t, before, queries.n.Load(), "repeated reads run no catalog query")

	// The engine's own writes show at once, through any of its handles.
	_, err = shop.UpdateProduct(ctx, pro.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Pro Plus")})
	require.NoError(t, err)
	moved, err := shop.CreatePrice(ctx, billing.CreatePriceParams{ProductID: pro.ID, Key: "pro-usd", UnitAmount: 7_000_000, Currency: "USD"})
	require.NoError(t, err)
	_, err = shop.UpdateProduct(ctx, granted.ID, billing.UpdateProductParams{Archived: catalog.Value(true)})
	require.NoError(t, err)
	_, err = shop.SetMeter(ctx, "tokens", billing.SetMeterParams{EventType: "tokens", Aggregation: catalog.AggregationSum, ValueProperty: "count", Unit: "token"})
	require.NoError(t, err)
	same("after the engine's writes")
	got, err := shop.GetProduct(ctx, pro.ID)
	require.NoError(t, err)
	require.Equal(t, "Pro Plus", got.DisplayName)
	require.Equal(t, []billing.PriceID{moved.ID}, priceIDs(got.Prices), "the key moved to its new price")
	meter, err := shop.GetMeter(ctx, "tokens")
	require.NoError(t, err)
	require.Equal(t, "token", meter.Unit)

	// Another replica's write: stale until RefreshAfter, then shown.
	_, err = replica.GetProduct(ctx, pro.ID)
	require.NoError(t, err)
	_, err = live.UpdateProduct(ctx, pro.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Pro Max")})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		got, err := replica.GetProduct(ctx, pro.ID)
		return err == nil && got.DisplayName == "Pro Max"
	}, 10*time.Second, 50*time.Millisecond)

	// Customer and money reads are never cached: another replica's grant
	// and access show at once.
	customer := billing.CustomerID(uuid.New())
	balance := func() int64 {
		b, err := shop.GetBalance(ctx, customer, "USD")
		require.NoError(t, err)
		return b.BalanceAmount
	}
	holds := func() bool {
		check, err := shop.CheckEntitlements(ctx, billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"pro"}})
		require.NoError(t, err)
		return check.Entitlements["pro"]
	}
	require.Zero(t, balance())
	require.False(t, holds())
	_, err = live.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 2_000_000, Source: "support", SourceID: "reads"}})
	require.NoError(t, err)
	_, err = live.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: pro.ID}}})
	require.NoError(t, err)
	require.EqualValues(t, 2_000_000, balance())
	require.True(t, holds())
}

func priceIDs(prices []billing.Price) []billing.PriceID {
	out := make([]billing.PriceID, len(prices))
	for i, p := range prices {
		out[i] = p.ID
	}
	return out
}
