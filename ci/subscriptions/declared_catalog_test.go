//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
)

// declaredFile is a host's catalog.yaml: a monthly membership granting key,
// and a usage meter priced per event.
func declaredFile(t *testing.T, key, title string, amount int64) *catalog.Application {
	t.Helper()
	params, err := catalog.ParseApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
catalog_version: 1
meters:
- key: %[1]s-events
  event_type: %[1]s.event
  aggregation: count
products:
- key: %[1]s
  display_name: %[2]s
  entitlements_spec:
    %[1]s: null
  prices:
  - key: %[1]s-monthly
    currency: usd
    unit_amount: %[3]d
    auto_renew: true
    access_duration_hours: 720
  rate_cards:
  - meter: %[1]s-events
    price:
      model: per_unit
      currency: usd
      per_unit:
        unit_amount: "1000"
`, key, title, amount)))
	require.NoError(t, err)
	return params
}

func (w *world) catalogRevision() int64 {
	w.t.Helper()
	var revision int64
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.sql(`SELECT catalog_revision FROM billing.merchants WHERE slug = $1`), w.slug).Scan(&revision))
	return revision
}

// bootDeclared constructs one more process for w's merchant, outside the
// harness, so New's own outcome stays observable.
func (w *world) bootDeclared(ctx context.Context, catalog *catalog.Application) (*openrails.Client, error) {
	return openrails.New(ctx, openrails.Config{
		Schema: w.schema, River: openrails.RiverHostOwned, TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesFull,
		Merchant: openrails.MerchantDeclaration{Slug: w.slug, DisplayName: w.slug, PSPs: w.psps},
		Catalog:  catalog,
	}, openrails.Deps{Postgres: w.pool, StripeTransport: w.stripe, NMITransport: w.nmi, Clock: w.clock})
}

func requireCatalogDeclared(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, billing.ErrCatalogDeclared)
	var status *billing.StatusError
	require.True(t, errors.As(err, &status), "%v", err)
	require.Equal(t, http.StatusMethodNotAllowed, status.Status)
}

// A host's catalog.yaml is configuration (#1126): New applies it before it
// returns, a reboot replays or converges it, and nothing else writes it.
func TestDeclaredCatalog(t *testing.T) {
	w := prepareWorld(t, 12)
	key := "declared-" + uuid.NewString()[:8]
	title, amount := "Gold", int64(9_990_000)
	version := int64(1)
	w.cfg = func(cfg *config.Config) {
		cfg.Catalog = declaredFile(t, key, title, amount)
		cfg.Catalog.CatalogVersion = version
	}
	w.booted = func(c *openrails.Client) {
		price, err := c.GetPriceByKey(t.Context(), key+"-monthly")
		require.NoError(t, err, "applied before New returned")
		require.EqualValues(t, amount, price.UnitAmount)
	}
	w.start()

	// The first customer buys it at once.
	price, err := w.client[embedded].GetPriceByKey(t.Context(), key+"-monthly")
	require.NoError(t, err)
	buyer := w.newCustomer()
	buyer.subscribe(embedded, "nmi", price.ID.String(), key, buyer.saveCard("nmi", visa))
	require.True(t, buyer.entitled(key))

	// Nothing else writes the declared catalog, in process or over HTTP, and
	// a refused price never reaches a provider.
	product, err := w.client[embedded].GetProductByKey(t.Context(), key)
	require.NoError(t, err)
	revision := w.catalogRevision()
	stripeWrites := len(w.stripe.mutations("/v1/"))
	console, hours := "Console title", monthHours
	for _, tp := range []topology{embedded, remote} {
		c := w.client[tp]
		_, err = c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value(console)})
		requireCatalogDeclared(t, err)
		_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key + "-yearly", UnitAmount: 99_000_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
		requireCatalogDeclared(t, err)
		_, err = c.ApplyCatalog(t.Context(), declaredFile(t, key, "Edited at runtime", amount))
		requireCatalogDeclared(t, err)
		_, err = c.SetMeter(t.Context(), key+"-other", billing.SetMeterParams{EventType: key + ".other", Aggregation: catalog.AggregationCount})
		requireCatalogDeclared(t, err)
		state, err := c.GetCatalogRevision(t.Context())
		require.NoError(t, err)
		require.False(t, state.WritesAllowed, tp)
	}
	require.Equal(t, revision, w.catalogRevision(), "refusals change nothing")
	require.Len(t, w.stripe.mutations("/v1/"), stripeWrites)

	// A payer's negotiated rate is not the catalog's: it stays writable, and
	// the next boot's application keeps it.
	overrides := "/v1/merchant/customers/" + buyer.id + "/rate-overrides"
	status, reply := w.staffJSON(http.MethodPut, overrides+"/"+key+"-events",
		map[string]any{"price": map[string]any{"model": "per_unit", "currency": "usd", "per_unit": map[string]any{"unit_amount": "500"}}})
	require.Equal(t, http.StatusOK, status, "%v", reply)
	w.restart()
	status, kept := w.staff(http.MethodGet, overrides)
	require.Equal(t, http.StatusOK, status, kept)
	require.Contains(t, kept, `"meter_key":"`+key+`-events"`)

	// Unchanged, the next boot replays.
	revision = w.catalogRevision()
	w.restart()
	require.Equal(t, revision, w.catalogRevision())

	// A forgotten version bump cannot change the catalog.
	_, err = w.bootDeclared(t.Context(), declaredFile(t, key, "Accidental edit", amount))
	require.ErrorContains(t, err, "increase catalog_version")
	require.Equal(t, revision, w.catalogRevision())

	// Edited, it converges; the member keeps the price they bought.
	title, amount = "Platinum", 12_000_000
	version = 3 // Versions may skip: each file declares the intended catalog.
	w.restart()
	require.Equal(t, revision+1, w.catalogRevision())
	product, err = w.client[embedded].GetProductByKey(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, "Platinum", product.DisplayName)
	repriced, err := w.client[embedded].GetPriceByKey(t.Context(), key+"-monthly")
	require.NoError(t, err)
	require.NotEqual(t, price.ID, repriced.ID)
	require.True(t, buyer.entitled(key))
	for _, older := range []int64{1, 2} {
		stale := declaredFile(t, key, "Stale", 9_990_000)
		stale.CatalogVersion = older
		// Even an old version that never applied, with a now-invalid provider,
		// is superseded before resolving rows or contacting providers.
		stale.Products[0].Prices[0].PSPs = catalog.Value([]string{"nowhere"})
		c, bootErr := w.bootDeclared(t.Context(), stale)
		require.NoError(t, bootErr)
		require.NoError(t, c.Close(t.Context()))
		require.Equal(t, revision+1, w.catalogRevision())
	}

	// A catalog the engine refuses fails New with the reason and changes nothing.
	w.stop()
	refused := declaredFile(t, key, "Platinum", 12_000_000)
	refused.CatalogVersion = 4
	refused.Products[0].Prices[0].PSPs = catalog.Value([]string{"nowhere"})
	_, err = w.bootDeclared(t.Context(), refused)
	require.ErrorContains(t, err, "Config.Catalog")
	require.ErrorContains(t, err, `"nowhere"`)
	require.Equal(t, revision+1, w.catalogRevision())

	// Replicas booting together converge on one application.
	start := make(chan struct{})
	clients, errs := make([]*openrails.Client, 2), make([]error, 2)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			declaration := declaredFile(t, key, "Concurrent", 12_000_000)
			declaration.CatalogVersion = 4
			clients[i], errs[i] = w.bootDeclared(t.Context(), declaration)
		}()
	}
	close(start)
	wg.Wait()
	for i, c := range clients {
		require.NoError(t, errs[i])
		t.Cleanup(func() { _ = c.Close(context.Background()) })
	}
	require.Equal(t, revision+2, w.catalogRevision(), "one application, one replay")
	for _, c := range clients {
		product, err = c.GetProductByKey(t.Context(), key)
		require.NoError(t, err)
		require.Equal(t, "Concurrent", product.DisplayName)
	}
}

// A provider reference New cannot confirm does not hold New: the application
// finishes in the background, and Ready fails until it commits.
func TestDeclaredCatalogAwaitsItsProvider(t *testing.T) {
	w := prepareWorld(t, 12)
	w.start()
	w.stop()
	key := "legacy-" + uuid.NewString()[:8]
	params, err := catalog.ParseApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
catalog_version: 1
products:
- key: %[1]s
  display_name: Legacy
  prices:
  - key: %[1]s-monthly
    currency: usd
    unit_amount: 9990000
    auto_renew: true
    access_duration_hours: 720
    psps: [stripe]
    psp_links:
      stripe:
        price_id: price_legacy_%[1]s
`, key)))
	require.NoError(t, err)
	revision := w.catalogRevision()

	w.stripe.priceReadsUnavailable(true)
	client, err := w.bootDeclared(t.Context(), params)
	require.NoError(t, err, "a provider outage never fails New")
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	_, err = riverkit.New(t.Context(), w.pool, &river.Config{Schema: w.schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 1}}}, client.RiverJobs())
	require.NoError(t, err)
	require.ErrorContains(t, client.Ready(t.Context()), "declared catalog not applied yet")
	_, err = client.GetPriceByKey(t.Context(), key+"-monthly")
	require.ErrorIs(t, err, billing.ErrNotFound)
	require.Equal(t, revision, w.catalogRevision())

	w.stripe.priceReadsUnavailable(false)
	require.Eventually(t, func() bool { return client.Ready(t.Context()) == nil }, 30*time.Second, 50*time.Millisecond)
	price, err := client.GetPriceByKey(t.Context(), key+"-monthly")
	require.NoError(t, err)
	require.Equal(t, "price_legacy_"+key, price.PSPs["stripe"].IDs["price_id"])
	require.Equal(t, revision+1, w.catalogRevision())
}

// Different replicas may arrive in either order: the highest authored version
// wins, and subsequent boots of both artifacts cannot reverse it.
func TestDeclaredCatalogVersionRace(t *testing.T) {
	w := prepareWorld(t, 12)
	key := "versioned-" + uuid.NewString()[:8]
	docs := []*catalog.Application{
		declaredFile(t, key, "Old", 9_990_000),
		declaredFile(t, key, "New", 12_000_000),
	}
	docs[1].CatalogVersion = 2
	for range 2 {
		start := make(chan struct{})
		clients, errs := make([]*openrails.Client, 2), make([]error, 2)
		var wg sync.WaitGroup
		for i := range docs {
			wg.Go(func() {
				<-start
				clients[i], errs[i] = w.bootDeclared(t.Context(), docs[i])
			})
		}
		close(start)
		wg.Wait()
		for i, c := range clients {
			require.NoError(t, errs[i])
			product, err := c.GetProductByKey(t.Context(), key)
			require.NoError(t, err)
			require.Equal(t, "New", product.DisplayName)
			require.NoError(t, c.Close(t.Context()))
		}
	}
}

func TestCatalogVersionIsConfigurationOnly(t *testing.T) {
	w := newWorld(t)
	for _, tp := range []topology{embedded, remote} {
		_, err := w.client[tp].ApplyCatalog(t.Context(), declaredFile(t, "configuration-only", "Config", 9_990_000))
		require.ErrorContains(t, err, "catalog_version belongs to Config.Catalog")
	}
}
