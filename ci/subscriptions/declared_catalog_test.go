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
  %[1]s-events:
    event_type: %[1]s.event
    aggregation: count
products:
  %[1]s:
    display_name: %[2]s
    entitlements: ["%[1]s"]
    prices:
      %[1]s-monthly:
        currency: usd
        unit_amount: %[3]d
        billing_interval_hours: 720
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
		Database: openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema}, TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesFull,
		Merchant: openrails.MerchantDeclaration{Slug: w.slug, DisplayName: w.slug, PSPs: w.psps},
		Catalog:  catalog,
	}, openrails.Deps{Postgres: w.pool, StripeTransport: w.stripe, NMITransport: w.nmi, Clock: w.clock})
}

// A startup batch shares the catalog with edits: staff edit over HTTP while the
// host's file applies on every start, and rebooting the same file never undoes
// those edits.
func TestDeclaredCatalog(t *testing.T) {
	w := prepareWorld(t, 12)
	key := "declared-" + uuid.NewString()[:8]
	title, amount := "Gold", int64(9_990_000)
	w.cfg = func(cfg *config.Config) { cfg.Catalog = declaredFile(t, key, title, amount) }
	w.start()
	original, err := priceByKey(t.Context(), w.client[embedded], key, key+"-monthly")
	require.NoError(t, err)
	buyer := w.newCustomer()
	buyer.subscribe(embedded, "nmi", original.ID.String(), key, buyer.saveCard("nmi", visa))
	require.True(t, buyer.entitled(key))
	product, err := productByKey(t.Context(), w.client[embedded], key)
	require.NoError(t, err)
	_, err = w.client[embedded].UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Edited in code")})
	require.NoError(t, err)
	var edited map[string]any
	status := w.staffCall(http.MethodPatch, "/v1/admin/catalog/products/"+product.ID.String(), map[string]any{"display_name": "HTTP edit"}, &edited)
	require.Equal(t, http.StatusOK, status, "Config.Catalog never closes the catalog routes: %v", edited)
	revision := w.catalogRevision()
	w.restart()
	require.Equal(t, revision, w.catalogRevision(), "reboot replays the batch despite later edits")
	product, err = productByKey(t.Context(), w.client[embedded], key)
	require.NoError(t, err)
	require.Equal(t, "HTTP edit", product.DisplayName)

	// Different content applies on the next start, except the title an edit set;
	// existing subscribers stay pinned.
	title, amount = "Platinum", 12_000_000
	w.restart()
	current, err := priceByKey(t.Context(), w.client[embedded], key, key+"-monthly")
	require.NoError(t, err)
	require.NotEqual(t, original.ID, current.ID)
	product, err = productByKey(t.Context(), w.client[embedded], key)
	require.NoError(t, err)
	require.Equal(t, "HTTP edit", product.DisplayName)
	require.Equal(t, []string{"catalog"}, w.openFindings(catalogConflictsFinding))
	require.True(t, buyer.entitled(key))
	replay, err := w.client[embedded].ApplyCatalog(t.Context(), declaredFile(t, key, "Gold", 9_990_000), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.True(t, replay.Replayed, "the original file cannot undo the newer batch")
	currentAgain, err := priceByKey(t.Context(), w.client[embedded], key, key+"-monthly")
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
			clients[i], errs[i] = w.bootDeclared(t.Context(), declaredFile(t, key, "HTTP edit", 14_000_000))
		})
	}
	close(start)
	wg.Wait()
	for i, c := range clients {
		require.NoError(t, errs[i])
		require.NoError(t, c.Close(t.Context()))
	}
	require.Equal(t, revision+1, w.catalogRevision())
	require.Empty(t, w.openFindings(catalogConflictsFinding), "a file that agrees closes the finding")
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
  %[1]s:
    display_name: Legacy
    prices:
      %[1]s-monthly:
        currency: usd
        unit_amount: 9990000
        billing_interval_hours: 720
        access_duration_hours: 720
        psps: [stripe]
        psp_links:
          stripe:
            price_id: price_legacy_%[1]s
`, key)))
	require.NoError(t, err)
	revision := w.catalogRevision()

	w.stripe.PriceReadsUnavailable(true)
	client, err := w.bootDeclared(t.Context(), params)
	require.NoError(t, err, "a provider outage never fails New")
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	_, err = riverkit.New(t.Context(), w.pool, &river.Config{Schema: w.schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 1}}}, client.RiverJobs())
	require.NoError(t, err)
	require.ErrorContains(t, client.Ready(t.Context()), "declared catalog not applied yet")
	_, err = priceByKey(t.Context(), client, key, key+"-monthly")
	require.ErrorIs(t, err, billing.ErrNotFound)
	require.Equal(t, revision, w.catalogRevision())

	w.stripe.PriceReadsUnavailable(false)
	require.Eventually(t, func() bool { return client.Ready(t.Context()) == nil }, 30*time.Second, 50*time.Millisecond)
	price, err := priceByKey(t.Context(), client, key, key+"-monthly")
	require.NoError(t, err)
	require.Equal(t, "price_legacy_"+key, price.PSPs["stripe"].IDs["price_id"])
	require.Equal(t, revision+1, w.catalogRevision())
}

// Replicas boot together: another one holds a PSP row (recording its
// credential fingerprint, say) while this New applies a catalog selling on that
// PSP. New waits its turn instead of failing.
func TestDeclaredCatalogWaitsForABusyPSP(t *testing.T) {
	w := prepareWorld(t, 12)
	w.start()
	w.stop()
	key := "busy-" + uuid.NewString()[:8]
	params, err := catalog.ParseApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
products:
  %[1]s:
    display_name: Busy
    entitlements: ["%[1]s"]
    prices:
      %[1]s-monthly:
        currency: usd
        unit_amount: 9990000
        billing_interval_hours: 720
        access_duration_hours: 720
        psps: [nmi]
`, key)))
	require.NoError(t, err)

	holder, err := w.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	var held int
	require.NoError(t, holder.QueryRow(t.Context(), w.sql(`SELECT count(*) FROM (SELECT 1 FROM billing.psps WHERE key = 'nmi' FOR UPDATE) held`)).Scan(&held))
	require.Equal(t, 1, held)
	time.AfterFunc(time.Second, func() { _ = holder.Rollback(context.Background()) })

	client, err := w.bootDeclared(t.Context(), params)
	require.NoError(t, err, "a PSP another replica holds delays New, never fails it")
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	_, err = priceByKey(t.Context(), client, key, key+"-monthly")
	require.NoError(t, err)
}
