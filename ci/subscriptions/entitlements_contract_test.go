//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
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
		var stored []byte
		require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT entitlements FROM billing.products WHERE id=$1`), product.ID.UUID()).Scan(&stored))
		require.JSONEq(t, `[]`, string(stored), "new products must store explicit empty lists, not unknown benefits")
	}
	var explicit billing.Product
	require.Equal(t, http.StatusCreated, w.staffCall(http.MethodPost, "/v1/merchant/catalog/products", map[string]any{
		"key": "explicit-empty-entitlements", "display_name": "Explicit empty", "entitlements": []string{},
	}, &explicit))
	require.NotNil(t, explicit.Entitlements)
	require.Empty(t, explicit.Entitlements)
	var storedEmpty []byte
	require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT entitlements FROM billing.products WHERE id=$1`), explicit.ID.UUID()).Scan(&storedEmpty))
	require.JSONEq(t, `[]`, string(storedEmpty))
	key := "opaque-list-" + uuid.NewString()[:8]
	document, err := catalog.ParseApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
products:
  %s:
    display_name: Opaque access
    entitlements: ["post:101", "premium", " private key "]
`, key)))
	require.NoError(t, err)
	initial, err := w.client[embedded].ApplyCatalog(t.Context(), document)
	require.NoError(t, err)
	var replay billing.CatalogApplicationReceipt
	require.Equal(t, http.StatusOK, w.staffCall(http.MethodPost, "/v1/merchant/catalog/applications", map[string]any{
		"schema_version": 1, "products": map[string]any{key: map[string]any{
			"display_name": "Opaque access", "entitlements": []string{" private key ", "premium", "post:101"},
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

	_, err = w.client[remote].ApplyCatalog(t.Context(), &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{
		key: {DisplayName: catalog.Value("Renamed")},
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
			// A saved card is charged in process; over HTTP only on a session
			// its customer pays.
			charge := w.client[embedded]
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
			initial, err := charge.CreateCheckoutAttempt(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, billing.CheckoutAttemptSucceeded, initial.Status)
			w.settle()
			require.True(t, customer.entitled("post:101"))
			require.True(t, customer.entitled("premium"), "content and service names receive identical access")
			require.True(t, customer.entitled(" private key "), "opaque spelling must survive granting and lookup")
			require.False(t, customer.entitled("private key"), "lookup must not trim an opaque entitlement")
			for _, check := range []struct {
				key  string
				want bool
			}{{" private key ", true}, {"private key", false}} {
				has, err := client.CheckEntitlements(t.Context(), customer.customerID(), billing.CheckEntitlementsParams{Entitlements: []string{check.key}, At: w.clock.Now()})
				require.NoError(t, err)
				require.Equal(t, map[string]bool{check.key: check.want}, has, "both transports preserve exact names")
				members, err := client.ListEntitlementCustomers(t.Context(), check.key, billing.EntitlementCustomerListParams{})
				require.NoError(t, err)
				if check.want {
					require.Contains(t, members.Items, customer.customerID())
				} else {
					require.Empty(t, members.Items)
				}
			}
			require.False(t, customer.entitled(product.Key), "the product key is not an implicit entitlement")
			_, err = client.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"post:202"})})
			require.NoError(t, err)
			replayed, err := charge.CreateCheckoutAttempt(t.Context(), request)
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
			purchased, err := charge.CreateCheckoutAttempt(t.Context(), request)
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
			emptyPurchase, err := charge.CreateCheckoutAttempt(t.Context(), request)
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

// One check answers every requested key from one query, on both transports:
// held, expired, revoked and never granted keys, another customer's view, a
// past instant, an empty list, and the 100-key bound.
func TestCheckEntitlementsAnswersEveryKey(t *testing.T) {
	w := newWorld(t)
	c, other := w.newCustomer(), w.newCustomer()
	staff := w.client[embedded]
	start := w.clock.Now()
	day, soon := 24, start.Add(time.Hour)
	grant := func(params billing.CreateEntitlementParams) billing.EntitlementID {
		record, err := staff.CreateEntitlement(t.Context(), c.customerID(), params)
		require.NoError(t, err)
		return record.ID
	}
	grant(billing.CreateEntitlementParams{Entitlement: "check:active", Hours: &day})
	grant(billing.CreateEntitlementParams{Entitlement: "check:expired", EndsAt: &soon})
	revoked := grant(billing.CreateEntitlementParams{Entitlement: "check:revoked", Hours: &day})
	require.NoError(t, staff.DeleteEntitlement(t.Context(), c.customerID(), revoked))
	w.advance(2 * time.Hour)

	keys := []string{"check:active", "check:expired", "check:revoked", "check:unknown", "check:active"}
	none := map[string]bool{"check:active": false, "check:expired": false, "check:revoked": false, "check:unknown": false}
	for _, tp := range []topology{embedded, remote} {
		client := w.client[tp]
		var got map[string]bool
		queries := w.count(func() {
			var err error
			got, err = client.CheckEntitlements(t.Context(), c.customerID(), billing.CheckEntitlementsParams{Entitlements: keys})
			require.NoError(t, err)
		})
		require.Equal(t, map[string]bool{"check:active": true, "check:expired": false, "check:revoked": false, "check:unknown": false}, got, tp)
		require.Equal(t, 1, queries["CheckResourceEntitlements"], "every key in one query: %v", queries)

		past, err := client.CheckEntitlements(t.Context(), c.customerID(), billing.CheckEntitlementsParams{Entitlements: keys, At: start.Add(30 * time.Minute)})
		require.NoError(t, err)
		require.Equal(t, map[string]bool{"check:active": true, "check:expired": true, "check:revoked": false, "check:unknown": false}, past, "At reads that instant; a revocation is not undone")

		foreign, err := client.CheckEntitlements(t.Context(), other.customerID(), billing.CheckEntitlementsParams{Entitlements: keys})
		require.NoError(t, err)
		require.Equal(t, none, foreign, "another customer's grants never answer")

		for _, empty := range [][]string{nil, {}} {
			answer, err := client.CheckEntitlements(t.Context(), c.customerID(), billing.CheckEntitlementsParams{Entitlements: empty})
			require.NoError(t, err)
			require.NotNil(t, answer)
			require.Empty(t, answer)
		}

		full := make([]string, billing.MaxEntitlementChecks)
		for i := range full {
			full[i] = fmt.Sprintf("check:bulk-%d", i)
		}
		full[0] = "check:active"
		bulk, err := client.CheckEntitlements(t.Context(), c.customerID(), billing.CheckEntitlementsParams{Entitlements: full})
		require.NoError(t, err)
		require.Len(t, bulk, billing.MaxEntitlementChecks)
		require.True(t, bulk["check:active"])
		require.False(t, bulk["check:bulk-1"])

		_, err = client.CheckEntitlements(t.Context(), c.customerID(), billing.CheckEntitlementsParams{Entitlements: append(full, "check:one-more")})
		require.ErrorIs(t, err, billing.ErrInvalid, "the client refuses past the bound before any I/O")
	}

	path := "/v1/merchant/customers/" + c.id + "/entitlements/check"
	for _, body := range []map[string]any{
		{"entitlements": append(make([]string, billing.MaxEntitlementChecks), "one-more")},
		{"entitlements": []string{"check:active", " "}},
	} {
		status, refused := w.staffJSON(http.MethodPost, path, body)
		require.Equal(t, http.StatusBadRequest, status, "%v", refused)
		detail := refused["error"].(map[string]any)
		require.Equal(t, billing.CodeInvalidParam, detail["code"])
		require.Equal(t, "entitlements", detail["param"])
	}
}
