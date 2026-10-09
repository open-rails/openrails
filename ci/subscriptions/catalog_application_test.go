//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// A YAML catalog is a partial batch applied once by content. Replaying it must
// preserve subsequent API edits and other batches; it is not desired-state sync.
func TestCatalogApplicationContentReplay(t *testing.T) {
	w := prepareWorld(t, 12)
	w.start()
	key := "batch-" + uuid.NewString()[:8]
	file := func(title string, amount int64) *catalog.Application {
		params, err := catalog.ParseApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
products:
  %[1]s:
    display_name: %[2]s
    entitlements: ["%[1]s"]
    prices:
      monthly:
        currency: usd
        unit_amount: %[3]d
        billing_interval_hours: 720
        access_duration_hours: 720
`, key, title, amount)))
		require.NoError(t, err)
		return params
	}
	apply := func(tp topology, params *catalog.Application) *billing.CatalogApplicationReceipt {
		t.Helper()
		receipt, err := w.client[tp].ApplyCatalog(t.Context(), params)
		require.NoError(t, err)
		return receipt
	}
	revision := func() int64 {
		t.Helper()
		r, err := w.client[embedded].GetCatalogRevision(t.Context())
		require.NoError(t, err)
		return r.Revision
	}
	derived := func(r *billing.CatalogApplicationReceipt) {
		t.Helper()
		require.Regexp(t, `^sha256:[0-9a-f]{64}$`, r.ApplicationID)
		require.Equal(t, r.BaseRevision+1, r.AppliedRevision)
	}

	outside, err := w.client[embedded].CreateProduct(t.Context(), billing.CreateProductParams{Key: key + "-api-only", DisplayName: "API product"})
	require.NoError(t, err)
	start := revision()
	first := apply(embedded, file("Gold", 10_000_000))
	derived(first)
	require.False(t, first.Replayed)
	require.Equal(t, start, first.BaseRevision)
	require.Equal(t, 1, first.ProductsChanged)
	require.Equal(t, 1, first.PricesChanged)

	for _, tp := range []topology{embedded, remote} {
		again := apply(tp, file("Gold", 10_000_000))
		require.True(t, again.Replayed, tp)
		require.Equal(t, first.ApplicationID, again.ApplicationID, tp)
		require.Equal(t, first.AppliedRevision, revision(), tp)
	}

	// Changed content is a new batch, with no manually managed identity fields.
	edited := apply(remote, file("Gold", 12_000_000))
	derived(edited)
	require.False(t, edited.Replayed)
	require.Equal(t, first.AppliedRevision, edited.BaseRevision)
	require.NotEqual(t, first.ApplicationID, edited.ApplicationID)
	require.Equal(t, 1, edited.PricesChanged)
	price, err := w.client[embedded].GetPriceByKey(t.Context(), key, "monthly")
	require.NoError(t, err)
	require.EqualValues(t, 12_000_000, price.UnitAmount)

	// Both older batches remain permanent replays after an independent API edit.
	product, err := w.client[embedded].GetProductByKey(t.Context(), key)
	require.NoError(t, err)
	_, err = w.client[remote].UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Console title")})
	require.NoError(t, err)
	apiRevision := revision()
	require.Greater(t, apiRevision, edited.AppliedRevision)
	for _, original := range []struct {
		amount  int64
		receipt *billing.CatalogApplicationReceipt
	}{{10_000_000, first}, {12_000_000, edited}} {
		for _, tp := range []topology{embedded, remote} {
			replay := apply(tp, file("Gold", original.amount))
			require.True(t, replay.Replayed)
			require.Equal(t, original.receipt.ApplicationID, replay.ApplicationID)
			require.Equal(t, original.receipt.AppliedRevision, replay.AppliedRevision)
			require.Equal(t, apiRevision, revision())
		}
	}
	product, err = w.client[embedded].GetProductByKey(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, "Console title", product.DisplayName)
	price, err = w.client[embedded].GetPriceByKey(t.Context(), key, "monthly")
	require.NoError(t, err)
	require.EqualValues(t, 12_000_000, price.UnitAmount)
	outside, err = w.client[embedded].GetProduct(t.Context(), outside.ID)
	require.NoError(t, err)
	require.False(t, outside.Archived, "omitting a product from a partial batch preserves it")

	// Content identity has no ordering semantics: previously unseen content can
	// apply even when its author considers it an older declaration.
	unseen := apply(remote, file("Earlier unseen title", 12_000_000))
	require.False(t, unseen.Replayed)
	require.Equal(t, apiRevision, unseen.BaseRevision)
	product, err = w.client[embedded].GetProductByKey(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, "Earlier unseen title", product.DisplayName)

	// Deleted caller controls are refused, not silently ignored.
	for _, body := range []map[string]any{
		{"schema_version": 1, "application_id": "manual"},
		{"schema_version": 1, "expected_revision": 0},
		{"schema_version": 1, "catalog_version": 1},
	} {
		status, reply := w.staffJSON(http.MethodPost, "/v1/merchant/catalog/applications", body)
		require.Equal(t, http.StatusBadRequest, status, "%v", reply)
	}
	require.Equal(t, unseen.AppliedRevision, revision(), "refused request fields cannot mutate the catalog")

	// Two replicas accepting the same new content commit one application.
	type outcome struct {
		receipt *billing.CatalogApplicationReceipt
		err     error
	}
	outcomes := make(chan outcome, 2)
	params := file("Concurrent title", 12_000_000)
	for _, tp := range []topology{embedded, remote} {
		go func() {
			receipt, err := w.client[tp].ApplyCatalog(t.Context(), params)
			outcomes <- outcome{receipt, err}
		}()
	}
	a, b := <-outcomes, <-outcomes
	require.NoError(t, a.err)
	require.NoError(t, b.err)
	require.NotEqual(t, a.receipt.Replayed, b.receipt.Replayed)
	require.Equal(t, a.receipt.ApplicationID, b.receipt.ApplicationID)
	require.Equal(t, a.receipt.AppliedRevision, b.receipt.AppliedRevision)
	require.Equal(t, unseen.AppliedRevision+1, revision())
}
