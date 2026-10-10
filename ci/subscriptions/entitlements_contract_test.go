//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestEntitlementListCatalogAPI(t *testing.T) {
	w := newWorld(t)
	for _, tp := range []topology{embedded, remote} {
		product, err := w.client[tp].CreateProduct(t.Context(), billing.CreateProductParams{
			Key: "omitted-entitlements-" + string(tp), DisplayName: "No entitlements declared",
		})
		require.NoError(t, err)
		require.NotNil(t, product.Entitlements)
		require.Empty(t, product.Entitlements)
		var stored int
		require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT count(*) FROM billing.product_entitlements WHERE product_id=$1`), product.ID.UUID()).Scan(&stored))
		require.Zero(t, stored, "a product without keys stores none")
	}
	var explicit billing.Product
	require.Equal(t, http.StatusCreated, w.staffCall(http.MethodPost, "/v1/admin/catalog/products", map[string]any{
		"key": "explicit-empty-entitlements", "display_name": "Explicit empty", "entitlements": []string{},
	}, &explicit))
	require.NotNil(t, explicit.Entitlements)
	require.Empty(t, explicit.Entitlements)
	var storedEmpty int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT count(*) FROM billing.product_entitlements WHERE product_id=$1`), explicit.ID.UUID()).Scan(&storedEmpty))
	require.Zero(t, storedEmpty)
	key := "opaque-list-" + uuid.NewString()[:8]
	document, err := catalog.ParseApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
products:
  %s:
    display_name: Opaque access
    entitlements: ["post:101", "premium", " private key "]
`, key)))
	require.NoError(t, err)
	initial, err := w.client[embedded].ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
	require.NoError(t, err)
	var replay billing.CatalogApplicationReceipt
	require.Equal(t, http.StatusOK, w.staffCall(http.MethodPost, "/v1/admin/catalog/applications", map[string]any{
		"schema_version": 1, "products": map[string]any{key: map[string]any{
			"display_name": "Opaque access", "entitlements": []string{" private key ", "premium", "post:101"},
		}},
	}, &replay))
	require.True(t, replay.Replayed)
	require.Equal(t, initial.ApplicationID, replay.ApplicationID, "reordering an entitlement list must not create another application")
	product, err := productByKey(t.Context(), w.client[remote], key)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"post:101", "premium", " private key "}, product.Entitlements)
	var wire map[string]any
	require.Equal(t, http.StatusOK, w.staffCall(http.MethodGet, "/v1/admin/catalog/products/"+product.ID.String(), nil, &wire))
	require.IsType(t, []any{}, wire["entitlements"])
	require.NotContains(t, wire, "entitlements_spec")

	_, err = w.client[remote].ApplyCatalog(t.Context(), &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{
		key: {DisplayName: catalog.Value("Renamed")},
	}}, billing.ApplyCatalogParams{})
	require.NoError(t, err)
	updated, err := w.client[embedded].GetProduct(t.Context(), product.ID)
	require.NoError(t, err)
	require.Equal(t, product.Entitlements, updated.Entitlements, "omitted entitlements must preserve the list")
	cleared, err := w.client[remote].UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{})})
	require.NoError(t, err)
	require.NotNil(t, cleared.Entitlements)
	require.Empty(t, cleared.Entitlements)
	_, err = w.client[embedded].UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Description: catalog.Value("Still empty")})
	require.NoError(t, err)
	cleared, err = w.client[remote].GetProduct(t.Context(), product.ID)
	require.NoError(t, err)
	require.NotNil(t, cleared.Entitlements)
	require.Empty(t, cleared.Entitlements)

	for _, invalid := range []map[string]any{
		{"entitlements": nil},
		{"entitlements": map[string]any{"premium": nil}},
		{"entitlements_spec": map[string]any{"premium": nil}},
		{"entitlements": []string{"premium", "premium"}},
		{"entitlements": []string{" "}},
	} {
		status, body := w.staffJSON(http.MethodPatch, "/v1/admin/catalog/products/"+product.ID.String(), invalid)
		require.Equal(t, http.StatusBadRequest, status, "%v", body)
	}
	for _, field := range []string{"entitlements", "entitlements_spec"} {
		status, body := w.staffJSON(http.MethodPost, "/v1/admin/catalog/products", map[string]any{
			"key": "invalid-" + uuid.NewString(), "display_name": "Invalid", field: nil,
		})
		require.Equal(t, http.StatusBadRequest, status, "%v", body)
	}
}

// A purchase is access to the product: its holders follow every edit of the
// product's keys. The accepted order replays unchanged.
func TestPurchasedAccessFollowsTheProduct(t *testing.T) {
	w := newWorld(t)
	for _, topology := range []topology{embedded, remote} {
		t.Run(string(topology), func(t *testing.T) {
			client := w.client[topology]
			product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{
				Key: "product-without-implicit-access-" + string(topology), DisplayName: "Mixed opaque benefits",
				Entitlements: []string{"post:101", "premium", " private key "},
			})
			require.NoError(t, err)
			price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "purchase", Currency: "USD", UnitAmount: 10_000_000})
			require.NoError(t, err)
			customer := w.newCustomer()
			method := customer.saveCard("nmi", visa)
			purchase := order{price: price.ID, rail: "nmi", method: method}
			session, err := customer.sell(topology, purchase)
			require.NoError(t, err)
			initial, err := session.buy(customer, purchase)
			require.NoError(t, err)
			require.Equal(t, "succeeded", initial.Status)
			w.settle()
			require.True(t, customer.entitled("post:101"))
			require.True(t, customer.entitled("premium"), "content and service names receive identical access")
			require.True(t, customer.entitled(" private key "), "opaque spelling must survive granting and lookup")
			require.False(t, customer.entitled("private key"), "lookup must not trim an opaque entitlement")
			for _, check := range []struct {
				key  string
				want bool
			}{{" private key ", true}, {"private key", false}} {
				has, err := heldKeys(t.Context(), client, customer.customerID(), w.clock.Now(), check.key)
				require.NoError(t, err)
				require.Equal(t, map[string]bool{check.key: check.want}, has, "both transports preserve exact names")
				members, err := client.ListEntitlements(t.Context(), billing.EntitlementListParams{Entitlements: []string{check.key}})
				require.NoError(t, err)
				if check.want {
					require.Contains(t, members.Items, billing.CustomerEntitlement{CustomerID: customer.customerID(), Entitlement: check.key})
				} else {
					require.Empty(t, members.Items)
				}
			}
			require.False(t, customer.entitled(product.Key), "the product key is not an implicit entitlement")
			_, err = client.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"post:202"})})
			require.NoError(t, err)
			replayed, err := session.buy(customer, purchase)
			require.NoError(t, err, "an accepted purchase must replay after its live product changes")
			require.Equal(t, "succeeded", replayed.Status)
			require.Equal(t, initial.PaymentID, replayed.PaymentID)
			require.True(t, customer.entitled("post:202"), "a key added to the product reaches its holder")
			require.False(t, customer.entitled("post:101"), "a key removed from the product leaves its holder")
			require.False(t, customer.entitled("premium"))

			next := w.newCustomer()
			nextMethod := next.saveCard("nmi", visa)
			next.mustCheckout(topology, order{price: price.ID, rail: "nmi", method: nextMethod})
			require.True(t, next.entitled("post:202"))
			require.False(t, next.entitled("post:101"))
			require.False(t, next.entitled("premium"))
			_, err = client.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{})})
			require.NoError(t, err)
			require.False(t, customer.entitled("post:202"), "a product without keys grants none")
			require.False(t, next.entitled("post:202"))
			access, err := heldProducts(t.Context(), client, customer.customerID(), product.ID)
			require.NoError(t, err)
			require.True(t, access[product.ID], "the customer still holds the product")
		})
	}
}

// One read answers every requested key of every requested customer from one
// query, on both transports: held, expired, revoked and never granted keys,
// another customer's grants, a past instant, the 100-key and 100-customer
// bounds, and the holders of one key. A key not listed is not held.
func TestListEntitlementsAnswersEveryKeyAndCustomer(t *testing.T) {
	w := newWorld(t)
	c, other := w.newCustomer(), w.newCustomer()
	staff := w.client[embedded]
	start := w.clock.Now()
	day, soon := 24, start.Add(time.Hour)
	c.grant(w.giftProduct("check:active"), &day, nil)
	c.grant(w.giftProduct("check:expired"), nil, &soon)
	revoked := c.grant(w.giftProduct("check:revoked"), &day, nil)
	other.grant(w.giftProduct("check:other", "check:active-too"), &day, nil)
	require.NoError(t, staff.DeleteProductAccess(t.Context(), c.customerID(), revoked.ID))
	w.advance(2 * time.Hour)

	keys := []string{"check:active", "check:expired", "check:revoked", "check:unknown", "check:active", "check:other"}
	both := []billing.CustomerID{c.customerID(), other.customerID()}
	rows := func(page *billing.ListPage[billing.CustomerEntitlement]) map[billing.CustomerEntitlement]bool {
		out := map[billing.CustomerEntitlement]bool{}
		for _, row := range page.Items {
			out[row] = true
		}
		return out
	}
	for _, tp := range []topology{embedded, remote} {
		client := w.client[tp]
		var got *billing.ListPage[billing.CustomerEntitlement]
		queries := w.count(func() {
			var err error
			got, err = client.ListEntitlements(t.Context(), billing.EntitlementListParams{CustomerIDs: both, Entitlements: keys})
			require.NoError(t, err)
		})
		require.Equal(t, map[billing.CustomerEntitlement]bool{
			{CustomerID: c.customerID(), Entitlement: "check:active"}:    true,
			{CustomerID: other.customerID(), Entitlement: "check:other"}: true,
		}, rows(got), tp)
		require.Equal(t, 1, queries["CheckDerivedEntitlements"], "every key of every customer in one query: %v", queries)
		require.Zero(t, queries["ListDerivedEntitlementsPage"], "named keys, no range read: %v", queries)

		past, err := heldKeys(t.Context(), client, c.customerID(), start.Add(30*time.Minute), keys...)
		require.NoError(t, err)
		require.Equal(t, map[string]bool{"check:active": true, "check:expired": true, "check:revoked": false, "check:unknown": false, "check:other": false}, past, "At reads that instant; a revocation is not undone")

		all, err := client.ListEntitlements(t.Context(), billing.EntitlementListParams{CustomerIDs: both, Prefix: "check:"})
		require.NoError(t, err)
		require.Len(t, all.Items, 3, "each customer's keys under the prefix: %v", all.Items)
		for i := 1; i < len(all.Items); i++ {
			a, b := all.Items[i-1], all.Items[i]
			require.True(t, a.CustomerID.String() < b.CustomerID.String() || a.CustomerID == b.CustomerID && a.Entitlement < b.Entitlement, "by customer, then key")
		}
		first, err := client.ListEntitlements(t.Context(), billing.EntitlementListParams{CustomerIDs: both, Prefix: "check:", PageRequest: billing.PageRequest{Limit: 2}})
		require.NoError(t, err)
		require.Len(t, first.Items, 2)
		require.NotEmpty(t, first.Next)
		rest, err := client.ListEntitlements(t.Context(), billing.EntitlementListParams{CustomerIDs: both, Prefix: "check:", PageRequest: billing.PageRequest{Limit: 2, Cursor: first.Next}})
		require.NoError(t, err)
		require.Equal(t, all.Items, append(first.Items, rest.Items...), "pages continue in order")
		require.Empty(t, rest.Next)

		holders, err := client.ListEntitlements(t.Context(), billing.EntitlementListParams{Entitlements: []string{"check:active"}})
		require.NoError(t, err)
		require.Equal(t, []billing.CustomerEntitlement{{CustomerID: c.customerID(), Entitlement: "check:active"}}, holders.Items, "without customers, one key's holders")

		full := make([]string, billing.MaxBatchItems)
		for i := range full {
			full[i] = fmt.Sprintf("check:bulk-%d", i)
		}
		full[0] = "check:active"
		bulk, err := heldKeys(t.Context(), client, c.customerID(), time.Time{}, full...)
		require.NoError(t, err)
		require.True(t, bulk["check:active"])
		require.False(t, bulk["check:bulk-1"])

		for _, refused := range []billing.EntitlementListParams{
			{CustomerIDs: both, Entitlements: append(full, "check:one-more")},
			{},
			{Entitlements: []string{"check:active", "check:other"}},
			{CustomerIDs: make([]billing.CustomerID, billing.MaxBatchItems+1)},
		} {
			_, err = client.ListEntitlements(t.Context(), refused)
			require.ErrorIs(t, err, billing.ErrInvalid, "the client refuses before any I/O")
		}
	}

	many := make([]string, billing.MaxBatchItems+1)
	for i := range many {
		many[i] = uuid.NewString()
	}
	for query, param := range map[string]string{
		"?customer_id=" + c.id + "&entitlement=check:active&entitlement=%20": "entitlement",
		"?customer_id=" + strings.Join(many, ","):                            "customer_id",
		"?customer_id=not-an-id":                                             "customer_id",
		"?entitlement=a&entitlement=b":                                       "entitlement",
		"?customer_id=" + c.id + "&prefix=check%3A%01":                       "prefix",
	} {
		status, refused := w.staffJSON(http.MethodGet, "/v1/admin/entitlements"+query, nil)
		require.Equal(t, http.StatusBadRequest, status, "%s: %v", query, refused)
		detail := refused["error"].(map[string]any)
		require.Contains(t, []string{billing.CodeInvalidParam, billing.CodeInvalidQuery}, detail["code"], query)
		require.Equal(t, param, detail["param"], query)
	}
}
