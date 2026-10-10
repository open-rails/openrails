//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

// keyRow is one stored row of a product's key history.
type keyRow struct {
	key       string
	addedAt   time.Time
	removedAt *time.Time
	addedBy   string
	removedBy *string
}

func (w *world) keyHistory(product billing.ProductID) []keyRow {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.sql(`SELECT entitlement, added_at, removed_at, added_by, removed_by
		FROM billing.product_entitlements WHERE product_id = $1 ORDER BY entitlement, added_at`), product.UUID())
	require.NoError(w.t, err)
	defer rows.Close()
	var out []keyRow
	for rows.Next() {
		var r keyRow
		require.NoError(w.t, rows.Scan(&r.key, &r.addedAt, &r.removedAt, &r.addedBy, &r.removedBy))
		out = append(out, r)
	}
	require.NoError(w.t, rows.Err())
	return out
}

func productKeys(t *testing.T, page *billing.ListPage[billing.Product]) []string {
	t.Helper()
	var keys []string
	for _, p := range page.Items {
		keys = append(keys, p.Key)
	}
	slices.Sort(keys)
	return keys
}

// A product's keys are rows with valid time: an edit closes the keys it drops
// and opens the ones it adds, at the engine's instant, and never rewrites a
// row. The product's revision steps once per edit.
func TestProductKeysKeepValidTimeHistory(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[embedded]
	created := w.clock.Now()
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "history", DisplayName: "History", Entitlements: []string{"b", "a"}})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, product.Entitlements)
	w.advance(time.Hour)
	edited := w.clock.Now()
	updated, err := w.client[remote].UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"c", "b"})})
	require.NoError(t, err)
	require.Equal(t, []string{"b", "c"}, updated.Entitlements)
	require.Equal(t, product.Revision+1, updated.Revision, "a key edit is a product change")
	same, err := c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"b", "c"})})
	require.NoError(t, err)
	require.Equal(t, updated.Revision, same.Revision, "the same keys are no edit")

	history := w.keyHistory(product.ID)
	require.Len(t, history, 3)
	require.Equal(t, "a", history[0].key)
	require.True(t, history[0].addedAt.Equal(created))
	require.NotNil(t, history[0].removedAt)
	require.True(t, history[0].removedAt.Equal(edited), "removal is recorded at the engine's instant")
	require.Equal(t, "b", history[1].key)
	require.Nil(t, history[1].removedAt, "an unchanged key keeps its row")
	require.Equal(t, "c", history[2].key)
	require.True(t, history[2].addedAt.Equal(edited))

	granting, err := c.ListProducts(t.Context(), billing.ProductListParams{Entitlements: []string{"c"}})
	require.NoError(t, err)
	require.Equal(t, []string{"history"}, productKeys(t, granting))
	dropped, err := c.ListProducts(t.Context(), billing.ProductListParams{Entitlements: []string{"a"}})
	require.NoError(t, err)
	require.Empty(t, dropped.Items, "only live keys select a product")

	_, err = w.pool.Exec(t.Context(), w.sql(`UPDATE billing.product_entitlements SET entitlement = 'z' WHERE product_id = $1`), product.ID.UUID())
	require.ErrorContains(t, err, "only closes", "history rows are immutable")
	_, err = w.pool.Exec(t.Context(), w.sql(`DELETE FROM billing.product_entitlements WHERE product_id = $1`), product.ID.UUID())
	require.ErrorContains(t, err, "immutable")
}

// An application's entitlement_replacements move a key across every product
// granting it in the same catalog edit: each product loses From and gains To,
// a product already granting To keeps one row, an empty To removes, and the
// receipt lists each changed product.
func TestEntitlementReplacementsMoveKeysAcrossProducts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			c := w.client[tp]
			n := uuid.NewString()[:6]
			a, b, x, gone := "content:"+n+":a", "content:"+n+":b", "content:"+n+":x", "content:"+n+":gone"
			bundle, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "bundle-" + n, DisplayName: "Bundle", Entitlements: []string{a, x, gone}})
			require.NoError(t, err)
			single, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "single-" + n, DisplayName: "Single", Entitlements: []string{a}})
			require.NoError(t, err)
			both, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "both-" + n, DisplayName: "Both", Entitlements: []string{a, b}})
			require.NoError(t, err)
			before, err := c.GetCatalogRevision(t.Context())
			require.NoError(t, err)

			replace := func(pairs ...catalog.EntitlementReplacement) (*billing.CatalogApplicationReceipt, error) {
				return c.ApplyCatalog(t.Context(), &catalog.Application{SchemaVersion: catalog.ApplicationSchemaVersion, EntitlementReplacements: pairs}, billing.ApplyCatalogParams{})
			}
			receipt, err := replace(catalog.EntitlementReplacement{From: a, To: b}, catalog.EntitlementReplacement{From: gone})
			require.NoError(t, err)
			require.Equal(t, before.Revision, receipt.BaseRevision)
			require.Equal(t, before.Revision+1, receipt.AppliedRevision, "one catalog edit")
			require.Equal(t, 3, receipt.ProductsChanged)
			require.Equal(t, []billing.EntitlementChange{
				{ProductID: both.ID, ProductKey: both.Key, Added: []string{}, Removed: []string{a}},
				{ProductID: bundle.ID, ProductKey: bundle.Key, Added: []string{b}, Removed: []string{a, gone}},
				{ProductID: single.ID, ProductKey: single.Key, Added: []string{b}, Removed: []string{a}},
			}, receipt.EntitlementChanges)

			for product, want := range map[billing.ProductID][]string{bundle.ID: {b, x}, single.ID: {b}, both.ID: {b}} {
				got, err := c.GetProduct(t.Context(), product)
				require.NoError(t, err)
				require.Equal(t, want, got.Entitlements)
			}
			withA, err := c.ListProducts(t.Context(), billing.ProductListParams{Entitlements: []string{a}})
			require.NoError(t, err)
			require.Empty(t, withA.Items)
			withB, err := c.ListProducts(t.Context(), billing.ProductListParams{Entitlements: []string{b}})
			require.NoError(t, err)
			require.Equal(t, []string{both.Key, bundle.Key, single.Key}, productKeys(t, withB))
			for _, row := range w.keyHistory(bundle.ID) {
				if row.key == a {
					require.NotNil(t, row.removedBy)
					require.Equal(t, receipt.ApplicationID, *row.removedBy, "history names the application")
				}
				if row.key == b {
					require.Equal(t, receipt.ApplicationID, row.addedBy)
				}
			}
			var recorded int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT count(*) FROM billing.catalog_applications WHERE application_id = $1`), receipt.ApplicationID).Scan(&recorded))
			require.Equal(t, 1, recorded)

			again, err := replace(catalog.EntitlementReplacement{From: gone}, catalog.EntitlementReplacement{From: a, To: b})
			require.NoError(t, err)
			require.True(t, again.Replayed, "the same replacements are the same application")
			require.Equal(t, receipt.ApplicationID, again.ApplicationID)
			later, err := replace(catalog.EntitlementReplacement{From: a, To: b})
			require.NoError(t, err)
			require.Zero(t, later.ProductsChanged, "nothing grants the key any more")

			for name, pairs := range map[string][]catalog.EntitlementReplacement{
				"duplicate":  {{From: x, To: b}, {From: x, To: "y"}},
				"chained":    {{From: x, To: b}, {From: b, To: "y"}},
				"blank from": {{From: " ", To: b}},
			} {
				_, err := replace(pairs...)
				require.ErrorIs(t, err, billing.ErrInvalid, name)
			}
		})
	}
}

// A declarative application reports how it changed existing products' keys;
// a new product's keys are its definition, not a change. A replay reports none.
func TestCatalogApplicationReportsKeyChanges(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[remote]
	apply := func(keys string) *billing.CatalogApplicationReceipt {
		t.Helper()
		document, err := catalog.ParseApplicationYAML(fmt.Appendf(nil, "schema_version: 1\nproducts:\n  course-bundle:\n    display_name: Course bundle\n    entitlements: %s\n", keys))
		require.NoError(t, err)
		receipt, err := c.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		return receipt
	}
	first := apply(`["course:101", "course:102"]`)
	require.Empty(t, first.EntitlementChanges)
	second := apply(`["course:101", "course:103"]`)
	require.Len(t, second.EntitlementChanges, 1)
	require.Equal(t, "course-bundle", second.EntitlementChanges[0].ProductKey)
	require.Equal(t, []string{"course:103"}, second.EntitlementChanges[0].Added)
	require.Equal(t, []string{"course:102"}, second.EntitlementChanges[0].Removed)
	replay := apply(`["course:103", "course:101"]`)
	require.True(t, replay.Replayed)
	require.Equal(t, []billing.EntitlementChange{}, replay.EntitlementChanges)
	for _, row := range w.keyHistory(second.EntitlementChanges[0].ProductID) {
		if row.key == "course:103" {
			require.Equal(t, second.ApplicationID, row.addedBy)
		}
	}
}

// A product no live price sells is granted only: the merchant lists it, the
// buyer's catalog does not. Selling it again lists it again.
func TestProductsWithoutALivePriceAreNotForSale(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[embedded]
	n := uuid.NewString()[:6]
	granted, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "granted-" + n, DisplayName: "Granted", Entitlements: []string{"beta-access"}})
	require.NoError(t, err)
	sold, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "sold-" + n, DisplayName: "Sold", Entitlements: []string{"post:1"}})
	require.NoError(t, err)
	_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: sold.ID, Key: "once", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)

	public := func() map[string]bool {
		listed := map[string]bool{}
		for _, item := range w.public("/v1/catalog/products?keys=" + sold.Key + "&keys=" + granted.Key)["data"].([]any) {
			listed[item.(map[string]any)["key"].(string)] = true
		}
		return listed
	}
	listed := public()
	require.True(t, listed[sold.Key])
	require.False(t, listed[granted.Key], "a product without a live price is not on sale")

	forSale, notForSale := true, false
	page, err := c.ListProducts(t.Context(), billing.ProductListParams{ForSale: &notForSale})
	require.NoError(t, err)
	require.Contains(t, productKeys(t, page), granted.Key)
	require.NotContains(t, productKeys(t, page), sold.Key)
	page, err = c.ListProducts(t.Context(), billing.ProductListParams{ForSale: &forSale})
	require.NoError(t, err)
	require.Equal(t, []string{sold.Key}, productKeys(t, page))
	status, body := w.staff(http.MethodGet, "/v1/admin/catalog/products?for_sale=false")
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, granted.Key)

	_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: granted.ID, Key: "once", UnitAmount: 2_000_000, Currency: "USD"})
	require.NoError(t, err)
	require.True(t, public()[granted.Key], "selling it lists it")
}

// One product grants at most catalog.MaxProductEntitlements keys.
func TestProductKeyCap(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[embedded]
	keys := make([]string, catalog.MaxProductEntitlements+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("content:cap:post:%05d", i)
	}
	_, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "too-big", DisplayName: "Too big", Entitlements: keys})
	require.ErrorIs(t, err, billing.ErrInvalid)
	full, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "full", DisplayName: "Full", Entitlements: keys[:catalog.MaxProductEntitlements]})
	require.NoError(t, err)
	require.Len(t, full.Entitlements, catalog.MaxProductEntitlements)
	_, err = c.UpdateProduct(t.Context(), full.ID, billing.UpdateProductParams{Entitlements: catalog.Value(keys)})
	require.ErrorIs(t, err, billing.ErrInvalid)
}

// The public catalog is a lookup by product key: the products on sale named
// by ?keys=, at most billing.MaxBatchItems, in one page. It refuses a missing
// keys, an entitlement filter and any other parameter. The Go client's
// ListOffers is the host backend's read: by entitlement too, in process and
// remote.
func TestPublicCatalogIsAKeyLookup(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[embedded]
	n := uuid.NewString()[:6]
	product := func(key string, entitlements []string, priced, archived bool) *billing.Product {
		p, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: key + "-" + n, DisplayName: key, Entitlements: entitlements})
		require.NoError(t, err)
		if priced {
			_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: p.ID, Key: "purchase", UnitAmount: 4_990_000, Currency: "USD"})
			require.NoError(t, err)
		}
		if archived {
			_, err = c.UpdateProduct(t.Context(), p.ID, billing.UpdateProductParams{Archived: catalog.Value(true)})
			require.NoError(t, err)
		}
		return p
	}
	course := product("course-101", []string{"course:101"}, true, false)
	bundle := product("bundle", []string{"course:101", "course:102"}, true, false)
	member := product("membership", []string{"channel:membership"}, true, false)
	granted := product("granted", []string{"course:101"}, false, false)
	retired := product("retired", []string{"course:101"}, true, true)

	get := func(query string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, w.server.URL+mountPrefix+"/v1/catalog/products?"+query, nil)
		require.NoError(t, err)
		req.Header.Set("OpenRails-Merchant", w.slug)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		var body map[string]any
		require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
		return res.StatusCode, body
	}
	sorted := func(keys ...string) []string { slices.Sort(keys); return keys }
	public := func(keys ...string) []string {
		t.Helper()
		q := url.Values{"keys": keys}
		status, body := get(q.Encode())
		require.Equal(t, http.StatusOK, status, body)
		require.Nil(t, body["next_cursor"], "one page")
		var listed []string
		for _, item := range body["data"].([]any) {
			listed = append(listed, item.(map[string]any)["key"].(string))
		}
		return sorted(listed...)
	}
	require.Equal(t, sorted(course.Key, bundle.Key), public(course.Key, bundle.Key, granted.Key, retired.Key), "the named products on sale")
	require.Equal(t, []string{member.Key}, public(member.Key))
	atCap := []string{course.Key}
	for i := 1; i < billing.MaxBatchItems; i++ {
		atCap = append(atCap, fmt.Sprintf("absent-%d-%s", i, n))
	}
	require.Equal(t, []string{course.Key}, public(atCap...), "up to the cap")

	for _, refused := range []struct{ query, param string }{
		{"", "keys"},
		{"entitlement=course:101", "entitlement"},
		{"entitlement=course:102&keys=" + bundle.Key, "entitlement"},
		{"keys=", "keys"},
		{"keys=" + course.Key + "&limit=1", "limit"},
		{url.Values{"keys": append(atCap, "one-more")}.Encode(), "keys"},
	} {
		status, body := get(refused.query)
		require.Equal(t, http.StatusBadRequest, status, refused.query)
		require.Equal(t, billing.CodeInvalidQuery, errorCode(body), refused.query)
		require.Equal(t, refused.param, body["error"].(map[string]any)["param"], refused.query)
	}

	for _, top := range []topology{embedded, remote} {
		page, err := w.client[top].ListOffers(t.Context(), billing.OfferListParams{Entitlements: []string{"course:101"}})
		require.NoError(t, err, top)
		require.Equal(t, sorted(course.Key, bundle.Key), productKeys(t, page), top)
		for _, p := range page.Items {
			require.Len(t, p.Prices, 1, top)
		}
		page, err = w.client[top].ListOffers(t.Context(), billing.OfferListParams{Entitlements: []string{"course:102", "channel:membership"}, Keys: []string{bundle.Key, course.Key}})
		require.NoError(t, err, top)
		require.Equal(t, []string{bundle.Key}, productKeys(t, page), "%s: both filters apply", top)
		_, err = w.client[top].ListOffers(t.Context(), billing.OfferListParams{})
		require.Error(t, err, top)
	}
	status, body := w.merchantCall(w.auth.hostToken(t), http.MethodGet, "/v1/app/catalog/products")
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Contains(t, body, billing.CodeInvalidQuery, "the backend's read needs a filter too")
}
