//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// counted is a real listener in front of the server that counts requests by
// path and, while down, answers 502 as a proxy whose upstream failed.
type counted struct {
	*httptest.Server
	mu    sync.Mutex
	paths map[string]int
	down  atomic.Bool
}

func count(t *testing.T, handler http.Handler) *counted {
	t.Helper()
	c := &counted{paths: map[string]int{}}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.paths[r.Method+" "+r.URL.Path]++
		c.mu.Unlock()
		if c.down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(c.Close)
	return c
}

// calls is how many requests reached each path under prefix.
func (c *counted) calls(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for path, calls := range c.paths {
		if strings.HasPrefix(path, prefix) {
			n += calls
		}
	}
	return n
}

// A remote handle answers repeated catalog and configuration reads from
// memory with what an uncached handle answers; its own writes show at once,
// another writer's after RefreshAfter; a failing server leaves it serving
// the last good answer; customer and money reads always cross the wire. The
// server's own engine handles see a write made over HTTP at once.
func TestRemoteReadCache(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, nil)
	ctx := t.Context()
	mid := provision(t, srv, uniqueName("reads"))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	api := count(t, handler)
	key := merchantKey(t, srv, mid, "owner").Secret
	remote := func(cache openrails.ReadCache) *openrails.Client {
		c, err := openrails.NewRemoteMerchant(api.URL, openrails.RemoteCredential{APIKey: key}, openrails.RemoteMerchantOptions{Timeout: 30 * time.Second, ReadCache: cache})
		require.NoError(t, err)
		return c
	}
	shop := remote(nil)
	live := remote(openrails.ReadCache{openrails.ReadCatalog: {}, openrails.ReadMeters: {}, openrails.ReadPublicConfig: {}, openrails.ReadMerchantConfiguration: {}})
	fast := remote(openrails.ReadCache{openrails.ReadCatalog: {RefreshAfter: 200 * time.Millisecond, MaxStale: time.Minute}})

	pro, err := live.CreateProduct(ctx, billing.CreateProductParams{Key: "pro", DisplayName: "Pro", Entitlements: []string{"pro"}})
	require.NoError(t, err)
	price, err := live.CreatePrice(ctx, billing.CreatePriceParams{ProductID: pro.ID, Key: "pro-usd", UnitAmount: 5_000_000, Currency: "USD"})
	require.NoError(t, err)
	retired, err := live.CreateProduct(ctx, billing.CreateProductParams{Key: "retired", DisplayName: "Retired", Archived: true})
	require.NoError(t, err)

	yes, no := true, false
	keys := []string{"pro", "retired", "missing"}
	reads := map[string]func(*openrails.Client) (any, error){
		"product": func(c *openrails.Client) (any, error) { return c.GetProduct(ctx, pro.ID) },
		"price":   func(c *openrails.Client) (any, error) { return c.GetPrice(ctx, price.ID, billing.GetPriceParams{}) },
		"by keys": func(c *openrails.Client) (any, error) {
			return c.ListProducts(ctx, billing.ProductListParams{Keys: keys})
		},
		"on sale by keys": func(c *openrails.Client) (any, error) {
			return c.ListProducts(ctx, billing.ProductListParams{Keys: keys, Archived: &no, ForSale: &yes})
		},
		"by ids": func(c *openrails.Client) (any, error) {
			return c.ListProducts(ctx, billing.ProductListParams{IDs: []billing.ProductID{pro.ID, retired.ID}})
		},
		"prices": func(c *openrails.Client) (any, error) {
			return c.ListPrices(ctx, billing.PriceListParams{ProductKey: "pro"})
		},
		"public config": func(c *openrails.Client) (any, error) { return c.GetPublicConfig(ctx) },
		"configuration": func(c *openrails.Client) (any, error) { return c.GetMerchantConfiguration(ctx) },
	}
	for name, read := range reads {
		want, err := read(live)
		require.NoError(t, err, name)
		got, err := read(shop)
		require.NoError(t, err, name)
		require.Equal(t, want, got, "%s matches the uncached read", name)
	}
	before := api.calls("GET ")
	for range 20 {
		for name, read := range reads {
			_, err := read(shop)
			require.NoError(t, err, name)
		}
	}
	require.Equal(t, before, api.calls("GET "), "repeated reads cross no wire")

	// The handle's own write shows at once; another writer's is stale until
	// RefreshAfter.
	_, err = fast.GetProduct(ctx, pro.ID)
	require.NoError(t, err)
	_, err = shop.UpdateProduct(ctx, pro.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Pro Plus")})
	require.NoError(t, err)
	got, err := shop.GetProduct(ctx, pro.ID)
	require.NoError(t, err)
	require.Equal(t, "Pro Plus", got.DisplayName)
	require.Eventually(t, func() bool {
		got, err := fast.GetProduct(ctx, pro.ID)
		return err == nil && got.DisplayName == "Pro Plus"
	}, 10*time.Second, 50*time.Millisecond)

	// The server's engine handle caches in process, and a write that reached
	// the engine over HTTP drops it at once.
	embedded, err := srv.Engine().Merchant(mid)
	require.NoError(t, err)
	got, err = embedded.GetProduct(ctx, pro.ID)
	require.NoError(t, err)
	require.Equal(t, "Pro Plus", got.DisplayName)
	_, err = live.UpdateProduct(ctx, pro.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Pro Max")})
	require.NoError(t, err)
	got, err = embedded.GetProduct(ctx, pro.ID)
	require.NoError(t, err)
	require.Equal(t, "Pro Max", got.DisplayName)

	// A failing server: the last good answer is served past RefreshAfter.
	require.Eventually(t, func() bool {
		got, err := fast.GetProduct(ctx, pro.ID)
		return err == nil && got.DisplayName == "Pro Max"
	}, 10*time.Second, 50*time.Millisecond)
	api.down.Store(true)
	_, err = live.GetProduct(ctx, pro.ID)
	require.ErrorIs(t, err, billing.ErrUnreachable, "the server is failing")
	failing := api.calls("GET /v1/catalog/products/")
	time.Sleep(300 * time.Millisecond)
	for range 3 {
		got, err = fast.GetProduct(ctx, pro.ID)
		require.NoError(t, err)
		require.Equal(t, "Pro Max", got.DisplayName)
	}
	require.Eventually(t, func() bool { return api.calls("GET /v1/catalog/products/") > failing }, 5*time.Second, 20*time.Millisecond, "a reload was tried")
	_, err = fast.GetProduct(ctx, billing.ProductID(uuid.New()))
	require.ErrorIs(t, err, billing.ErrUnreachable, "nothing to serve for a read never answered")
	api.down.Store(false)
	_, err = live.UpdateProduct(ctx, pro.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Pro Ultra")})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		got, err := fast.GetProduct(ctx, pro.ID)
		return err == nil && got.DisplayName == "Pro Ultra"
	}, 15*time.Second, 50*time.Millisecond, "recovered")

	// Customer and money reads are never cached.
	customer := billing.CustomerID(uuid.New())
	_, err = shop.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1_000_000, Source: "support", SourceID: "reads"}})
	require.NoError(t, err)
	before = api.calls("GET ") + api.calls("POST /v1/app/entitlements/check")
	for range 3 {
		balance, err := shop.GetBalance(ctx, customer, "USD")
		require.NoError(t, err)
		require.EqualValues(t, 1_000_000, balance.BalanceAmount)
		_, err = shop.ListOrders(ctx, billing.OrderListParams{CustomerID: customer})
		require.NoError(t, err)
		_, err = shop.CheckEntitlements(ctx, billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"pro"}})
		require.NoError(t, err)
		_, err = shop.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: customer})
		require.NoError(t, err)
	}
	require.Equal(t, before+12, api.calls("GET ")+api.calls("POST /v1/app/entitlements/check"), "each customer or money read is a request")
}
