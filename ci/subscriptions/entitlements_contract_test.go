//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestEntitlementListCatalogAPI(t *testing.T) {
	w := newWorld(t)
	key := "opaque-list-" + uuid.NewString()[:8]
	document, err := catalog.ParseApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
products:
- key: %s
  display_name: Opaque access
  entitlements: ["post:101", "premium", " private key "]
`, key)))
	require.NoError(t, err)
	initial, err := w.client[embedded].ApplyCatalog(t.Context(), document)
	require.NoError(t, err)
	var replay billing.CatalogApplicationReceipt
	require.Equal(t, http.StatusOK, w.staffCall(http.MethodPost, "/v1/merchant/catalog/applications", map[string]any{
		"schema_version": 1, "products": []any{map[string]any{
			"key": key, "display_name": "Opaque access", "entitlements": []string{" private key ", "premium", "post:101"},
		}},
	}, &replay))
	require.True(t, replay.Replayed)
	require.Equal(t, initial.ApplicationID, replay.ApplicationID, "reordering an entitlement list must not create another application")
	product, err := w.client[remote].GetProductByKey(t.Context(), key)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"post:101", "premium", " private key "}, product.Entitlements)
	var wire map[string]any
	require.Equal(t, http.StatusOK, w.staffCall(http.MethodGet, "/v1/merchant/catalog/products/"+product.ID.String(), nil, &wire))
	require.IsType(t, []any{}, wire["entitlements"])
	require.NotContains(t, wire, "entitlements_spec")

	_, err = w.client[remote].ApplyCatalog(t.Context(), &catalog.Application{SchemaVersion: 1, Products: []catalog.ApplyProduct{
		{Key: key, DisplayName: catalog.Value("Renamed")},
	}})
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
		status, body := w.staffJSON(http.MethodPatch, "/v1/merchant/catalog/products/"+product.ID.String(), invalid)
		require.Equal(t, http.StatusBadRequest, status, "%v", body)
	}
	for _, field := range []string{"entitlements", "entitlements_spec"} {
		status, body := w.staffJSON(http.MethodPost, "/v1/merchant/catalog/products", map[string]any{
			"key": "invalid-" + uuid.NewString(), "display_name": "Invalid", field: nil,
		})
		require.Equal(t, http.StatusBadRequest, status, "%v", body)
	}
}

func TestEntitlementListsKeepAcceptedPurchaseBenefits(t *testing.T) {
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
			request := billing.CreateCheckoutAttemptParams{
				Customer: customer.identity(), PriceID: price.ID, OfferKind: billing.OfferPermanent, Entitlement: "post:101",
				IdempotencyKey: "opaque-purchase-" + uuid.NewString(),
				PaymentOptions: billing.CheckoutPaymentOptions{PSP: "nmi", PaymentMethodID: pmid(method)},
			}
			initial, err := client.CreateCheckoutAttempt(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, billing.CheckoutAttemptSucceeded, initial.Status)
			w.settle()
			require.True(t, customer.entitled("post:101"))
			require.True(t, customer.entitled("premium"), "content and service names receive identical access")
			require.True(t, customer.entitled(" private key "), "opaque spelling must survive granting and lookup")
			require.False(t, customer.entitled("private key"), "lookup must not trim an opaque entitlement")
			require.False(t, customer.entitled(product.Key), "the product key is not an implicit entitlement")
			_, err = client.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"post:202"})})
			require.NoError(t, err)
			replayed, err := client.CreateCheckoutAttempt(t.Context(), request)
			require.NoError(t, err, "an accepted purchase must replay after its live product changes")
			require.Equal(t, initial.ID, replayed.ID)
			require.True(t, customer.entitled("post:101"))
			require.True(t, customer.entitled("premium"))
			require.False(t, customer.entitled("post:202"), "a product edit cannot rewrite accepted benefits")

			next := w.newCustomer()
			nextMethod := next.saveCard("nmi", visa)
			request.Customer, request.Entitlement = next.identity(), "post:202"
			request.IdempotencyKey = "next-opaque-purchase-" + uuid.NewString()
			request.PaymentOptions.PaymentMethodID = pmid(nextMethod)
			purchased, err := client.CreateCheckoutAttempt(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, billing.CheckoutAttemptSucceeded, purchased.Status)
			w.settle()
			require.True(t, next.entitled("post:202"))
			require.False(t, next.entitled("post:101"))
			require.False(t, next.entitled("premium"))
			_, err = client.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{})})
			require.NoError(t, err)
			require.True(t, customer.entitled("premium"), "clearing the live catalog does not revoke an earlier purchase")
			require.True(t, next.entitled("post:202"))

			empty := w.newCustomer()
			emptyMethod := empty.saveCard("nmi", visa)
			request.Customer, request.Entitlement = empty.identity(), ""
			request.IdempotencyKey = "empty-opaque-purchase-" + uuid.NewString()
			request.PaymentOptions.PaymentMethodID = pmid(emptyMethod)
			emptyPurchase, err := client.CreateCheckoutAttempt(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, billing.CheckoutAttemptSucceeded, emptyPurchase.Status)
			require.NotNil(t, emptyPurchase.PaymentID)
			var accepted []byte
			require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT entitlements_snapshot FROM billing.payments WHERE id=$1`), emptyPurchase.PaymentID.UUID()).Scan(&accepted))
			require.JSONEq(t, `[]`, string(accepted), "an accepted empty list must not become an unknown snapshot")
			require.False(t, empty.entitled("post:202"))
			require.False(t, empty.entitled(product.Key), "an empty list must not imply the product key")
		})
	}
}
