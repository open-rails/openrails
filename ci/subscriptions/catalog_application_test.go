//go:build e2e && integration

package subscriptions_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A host's catalog.yaml is the desired state (#1125): applied on every boot
// without application_id/expected_revision, it replays while the catalog is
// unchanged and otherwise converges the catalog to the file.
func TestCatalogApplicationDeclarative(t *testing.T) {
	w := prepareWorld(t, 12)
	w.start()
	key := "decl-" + uuid.NewString()[:8]
	file := func(title string, amount int64) *billing.CatalogApplyParams {
		params, err := billing.ParseCatalogApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
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
`, key, title, amount)))
		require.NoError(t, err)
		require.True(t, params.Declarative())
		return params
	}
	apply := func(tp topology, params *billing.CatalogApplyParams) *billing.CatalogApplicationReceipt {
		t.Helper()
		receipt, err := w.client[tp].Catalog.Apply(t.Context(), params)
		require.NoError(t, err)
		return receipt
	}
	revision := func() int64 {
		t.Helper()
		r, err := w.client[embedded].Catalog.Revision(t.Context())
		require.NoError(t, err)
		return r.Revision
	}
	derived := func(r *billing.CatalogApplicationReceipt) {
		t.Helper()
		require.Regexp(t, fmt.Sprintf(`^sha256:[0-9a-f]{64}@%d$`, r.AppliedRevision), r.ApplicationID)
		require.Equal(t, r.BaseRevision+1, r.AppliedRevision)
	}

	start := revision()
	first := apply(embedded, file("Gold", 10_000_000))
	derived(first)
	require.False(t, first.Replayed)
	require.Equal(t, start, first.BaseRevision)
	require.Equal(t, 1, first.ProductsChanged)
	require.Equal(t, 1, first.PricesChanged)

	// Reboot with the same file: a replay over either transport, no new revision.
	for _, tp := range []topology{embedded, remote} {
		again := apply(tp, file("Gold", 10_000_000))
		require.True(t, again.Replayed, tp)
		require.Equal(t, first.ApplicationID, again.ApplicationID, tp)
		require.Equal(t, first.AppliedRevision, revision(), tp)
	}

	// An edited file needs no hand-bumped identity or revision.
	edited := apply(remote, file("Gold", 12_000_000))
	derived(edited)
	require.False(t, edited.Replayed)
	require.Equal(t, first.AppliedRevision, edited.BaseRevision)
	require.NotEqual(t, first.ApplicationID, edited.ApplicationID)
	require.Equal(t, 1, edited.PricesChanged)
	price, err := w.client[embedded].Prices.RetrieveByKey(t.Context(), key+"-monthly")
	require.NoError(t, err)
	require.EqualValues(t, 12_000_000, price.UnitAmount)

	// A console edit outside any application moves the revision; the next boot
	// applies the unchanged file again and the file wins.
	product, err := w.client[embedded].Products.RetrieveByKey(t.Context(), key)
	require.NoError(t, err)
	console := "Console title"
	_, err = w.client[remote].Products.Update(t.Context(), product.ID, &billing.ProductUpdateParams{DisplayName: &console})
	require.NoError(t, err)
	require.Greater(t, revision(), edited.AppliedRevision)
	boot := apply(embedded, file("Gold", 12_000_000))
	derived(boot)
	require.False(t, boot.Replayed)
	require.Equal(t, 1, boot.ProductsChanged)
	require.Zero(t, boot.PricesChanged)
	product, err = w.client[embedded].Products.RetrieveByKey(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, "Gold", product.DisplayName)
	require.True(t, apply(remote, file("Gold", 12_000_000)).Replayed, "converged again, the file replays")

	// Explicit identity keeps the guarded contract and both conflicts.
	guarded := func(id string, expected int64, title string) *billing.CatalogApplyParams {
		params := file(title, 12_000_000)
		params.ApplicationID, params.ExpectedRevision = id, &expected
		return params
	}
	conflict := func(params *billing.CatalogApplyParams, code string) {
		t.Helper()
		_, err := w.client[remote].Catalog.Apply(t.Context(), params)
		var status *billing.StatusError
		require.True(t, errors.As(err, &status), "%v", err)
		require.Equal(t, http.StatusConflict, status.Status)
		require.Equal(t, code, status.Code)
	}
	base := revision()
	id := "guarded-" + key
	one := apply(embedded, guarded(id, base, "Guarded"))
	require.Equal(t, id, one.ApplicationID)
	require.Equal(t, base+1, one.AppliedRevision)
	conflict(guarded(id, base, "Other content"), "catalog_application_conflict")
	conflict(guarded("stale-"+key, base, "Guarded"), "catalog_revision_conflict")
	require.True(t, apply(remote, guarded(id, base, "Guarded")).Replayed)

	// Exactly one identity field is ambiguous: refused before any change.
	for _, body := range []map[string]any{
		{"schema_version": 1, "application_id": "half-" + key},
		{"schema_version": 1, "expected_revision": base + 1},
	} {
		status, reply := w.staffJSON(http.MethodPost, "/v1/merchant/catalog/applications", body)
		require.Equal(t, http.StatusBadRequest, status, "%v", reply)
		require.Contains(t, fmt.Sprint(reply), "go together")
	}
	half := file("Gold", 12_000_000)
	half.ApplicationID = "half-" + key
	_, err = w.client[embedded].Catalog.Apply(t.Context(), half)
	require.ErrorContains(t, err, "go together")
	require.Equal(t, base+1, revision(), "refusals and replays change nothing")

	// The guarded edit is just another intervening write to the boot file.
	after := apply(embedded, file("Gold", 12_000_000))
	require.True(t, strings.HasPrefix(after.ApplicationID, "sha256:"))
	require.False(t, after.Replayed)
	require.Equal(t, base+1, after.BaseRevision)
	product, err = w.client[embedded].Products.RetrieveByKey(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, "Gold", product.DisplayName)
}
