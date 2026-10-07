//go:build e2e && integration

package subscriptions_test

import (
	"context"
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

// A startup batch does not own the catalog: client edits remain available when
// HTTP writes are disabled, and rebooting the same file never undoes those edits.
func TestDeclaredCatalog(t *testing.T) {
	w := prepareWorld(t, 12)
	key := "declared-" + uuid.NewString()[:8]
	title, amount := "Gold", int64(9_990_000)
	w.cfg = func(cfg *config.Config) {
		cfg.Catalog = declaredFile(t, key, title, amount)
		cfg.AllowCatalogUpdates = false
	}
	w.start()
	original, err := w.client[embedded].GetPriceByKey(t.Context(), key, key+"-monthly")
	require.NoError(t, err)
	buyer := w.newCustomer()
	buyer.subscribe(embedded, "nmi", original.ID.String(), key, buyer.saveCard("nmi", visa))
	require.True(t, buyer.entitled(key))
	product, err := w.client[embedded].GetProductByKey(t.Context(), key)
	require.NoError(t, err)
	_, err = w.client[embedded].UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Edited in code")})
	require.NoError(t, err)
	status := w.staffCall(http.MethodPatch, "/v1/merchant/catalog/products/"+product.ID.String(), map[string]any{"display_name": "HTTP edit"}, nil)
	require.Equal(t, http.StatusMethodNotAllowed, status, "HTTP writes are absent while reads remain available")
	revision := w.catalogRevision()
	w.restart()
	require.Equal(t, revision, w.catalogRevision(), "reboot replays the batch despite a later programmatic edit")
	product, err = w.client[embedded].GetProductByKey(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, "Edited in code", product.DisplayName)

	// Different content is a new partial batch; existing subscribers stay pinned.
	title, amount = "Platinum", 12_000_000
	w.restart()
	current, err := w.client[embedded].GetPriceByKey(t.Context(), key, key+"-monthly")
	require.NoError(t, err)
	require.NotEqual(t, original.ID, current.ID)
	require.True(t, buyer.entitled(key))
	replay, err := w.client[embedded].ApplyCatalog(t.Context(), declaredFile(t, key, "Gold", 9_990_000))
	require.NoError(t, err)
	require.True(t, replay.Replayed, "the original file cannot undo the newer batch")
	currentAgain, err := w.client[embedded].GetPriceByKey(t.Context(), key, key+"-monthly")
	require.NoError(t, err)
	require.Equal(t, current.ID, currentAgain.ID)

	// Concurrent startup instances of the same new batch commit only once.
	w.stop()
	revision = w.catalogRevision()
	start := make(chan struct{})
	clients, errs := make([]*openrails.Client, 2), make([]error, 2)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			<-start
			clients[i], errs[i] = w.bootDeclared(t.Context(), declaredFile(t, key, "Concurrent", amount))
		})
	}
	close(start)
	wg.Wait()
	for i, c := range clients {
		require.NoError(t, errs[i])
		require.NoError(t, c.Close(t.Context()))
	}
	require.Equal(t, revision+1, w.catalogRevision())
}

// A provider reference New cannot confirm does not hold New: the application
// finishes in the background, and Ready fails until it commits.
func TestDeclaredCatalogAwaitsItsProvider(t *testing.T) {
	w := prepareWorld(t, 12)
	w.start()
	w.stop()
	key := "legacy-" + uuid.NewString()[:8]
	params, err := catalog.ParseApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
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
	_, err = client.GetPriceByKey(t.Context(), key, key+"-monthly")
	require.ErrorIs(t, err, billing.ErrNotFound)
	require.Equal(t, revision, w.catalogRevision())

	w.stripe.priceReadsUnavailable(false)
	require.Eventually(t, func() bool { return client.Ready(t.Context()) == nil }, 30*time.Second, 50*time.Millisecond)
	price, err := client.GetPriceByKey(t.Context(), key, key+"-monthly")
	require.NoError(t, err)
	require.Equal(t, "price_legacy_"+key, price.PSPs["stripe"].IDs["price_id"])
	require.Equal(t, revision+1, w.catalogRevision())
}
