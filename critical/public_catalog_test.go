//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func publicSale(t *testing.T, a *authorityWorld, key string, amount int64, hours *int) (*billing.Product, *billing.Price) {
	t.Helper()
	product, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: "Public contract " + key, Ownership: catalog.OwnershipConsumable, Entitlements: []string{"library:read"}})
	require.NoError(t, err)
	price, err := a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, UnitAmount: amount, Currency: "USD", AccessDurationHours: hours})
	require.NoError(t, err)
	return product, price
}

func publicPurchase(t *testing.T, a *authorityWorld, p *providerLedger, price billing.PriceID, quantity int, total int64, key string) billing.Order {
	t.Helper()
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price, Quantity: ptr(quantity)}}, ExpectedTotal: ptr(total), Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status)
	require.Equal(t, billing.CustomerID(uuid.MustParse(a.alice.ID)), order.CustomerID)
	require.Equal(t, total, order.Total)
	require.NotNil(t, order.Payment.PaymentID)
	return order
}

func TestPublicCatalog(t *testing.T) {
	t.Run("OR-J05-S01/exact-price-quantity-and-provider-settlement", func(t *testing.T) {
		a, provider := newPaymentWorld(t)
		product, price := publicSale(t, a, "exact-usd", 9_990_000, nil)
		read, err := a.alphaRemote.GetPrice(t.Context(), price.ID, billing.GetPriceParams{})
		require.NoError(t, err)
		require.Equal(t, product.ID, read.ProductID)
		require.EqualValues(t, 9_990_000, read.UnitAmount)
		require.Equal(t, "USD", read.Currency)
		preview, err := a.alphaRemote.PreviewOrder(t.Context(), billing.PreviewOrderParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Lines: []billing.OrderLineParams{{PriceID: price.ID, Quantity: ptr(3)}}})
		require.NoError(t, err)
		require.EqualValues(t, 29_970_000, preview.Total)
		require.Len(t, preview.Lines, 1)
		require.Nil(t, preview.Lines[0].Refusal)
		require.EqualValues(t, 29_970_000, preview.Lines[0].Amount)
		order := publicPurchase(t, a, provider, price.ID, 3, 29_970_000, "exact-three")
		require.Equal(t, price.ID, *order.Lines[0].PriceID)
		effects := provider.effects(t)
		require.Len(t, effects, 1)
		require.Equal(t, "usd", effects[0].Currency)
		require.EqualValues(t, 2997, effects[0].Amount, "USD native units are micros; the external charge settles exact cents")
	})
	t.Run("OR-J05-S02/invalid-money-quantity-and-trial-cannot-create-a-price", func(t *testing.T) {
		for _, amount := range []string{"9.9900001 USD", "9223372036854.775808 USD", "9.99 UNKNOWN", "-1 USD"} {
			t.Run(amount, func(t *testing.T) {
				_, err := catalog.ParseApplicationJSON([]byte(fmt.Sprintf(`{"schema_version":1,"products":{"bad":{"display_name":"Bad","prices":{"bad":{"amount":%q}}}}}`, amount)))
				require.Error(t, err, "a public catalog document must refuse rounding, overflow, unknown currencies and negative prices")
			})
		}
		a, provider := newPaymentWorld(t)
		product, price := publicSale(t, a, "input-boundaries", 9_990_000, nil)
		for _, invalid := range []billing.CreatePriceParams{
			{ProductID: product.ID, Key: "wrong-currency", UnitAmount: 1, Currency: "UNKNOWN"},
			{ProductID: product.ID, Key: "negative", UnitAmount: -1, Currency: "USD"},
			{ProductID: product.ID, Key: "unsupported-trial", UnitAmount: 1_000_000, Currency: "USD", TrialUnitAmount: ptr(int64(0)), TrialDurationHours: ptr(24)},
		} {
			_, err := a.alphaRemote.CreatePrice(t.Context(), invalid)
			require.ErrorIs(t, err, billing.ErrInvalid)
		}
		for _, quantity := range []int{-1, 0} {
			_, err := a.alphaRemote.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Lines: []billing.OrderLineParams{{PriceID: price.ID, Quantity: ptr(quantity)}}, IdempotencyKey: fmt.Sprint("invalid-quantity-", quantity)})
			require.Error(t, err)
		}
		prices, err := a.alphaRemote.ListPrices(t.Context(), billing.PriceListParams{ProductID: product.ID})
		require.NoError(t, err)
		require.Len(t, prices.Items, 1)
		require.Equal(t, price.ID, prices.Items[0].ID)
		orders, err := a.alphaRemote.ListOrders(t.Context(), billing.OrderListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Empty(t, orders.Items)
		require.Empty(t, provider.effects(t))
	})
	t.Run("OR-J05-S03/replay-manual-ownership-and-revision-conflicts", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		document, err := catalog.ParseApplicationJSON([]byte(`{"schema_version":1,"products":{"declared":{"display_name":"Declared name","entitlements":["declared:read"],"prices":{"standard":{"amount":"9.99 USD"}}}}}`))
		require.NoError(t, err)
		first, err := a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.Equal(t, 1, first.ProductsChanged)
		require.Equal(t, 1, first.PricesChanged)
		replay, err := a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		require.Equal(t, first.ApplicationID, replay.ApplicationID)
		require.Equal(t, first.AppliedRevision, replay.AppliedRevision)
		products, err := a.alphaRemote.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"declared"}})
		require.NoError(t, err)
		require.Len(t, products.Items, 1)
		product := products.Items[0]
		manual, err := a.alphaRemote.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Manual name"), ExpectedRevision: &product.Revision})
		require.NoError(t, err)
		_, err = a.alphaRemote.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Stale name"), ExpectedRevision: &product.Revision})
		var failure *billing.StatusError
		require.ErrorAs(t, err, &failure)
		require.Equal(t, "revision_mismatch", failure.Code)
		declaration := document.Products["declared"]
		declaration.DisplayName = catalog.Value("Reconciled name")
		document.Products["declared"] = declaration
		conflict, err := a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.Len(t, conflict.Conflicts, 1)
		require.Equal(t, "declared", conflict.Conflicts[0].Key)
		kept, err := a.alphaRemote.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		require.Equal(t, "Manual name", kept.DisplayName)
		require.Equal(t, manual.Revision, kept.Revision)
		forced, err := a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{Force: true})
		require.NoError(t, err)
		require.Empty(t, forced.Conflicts)
		final, err := a.alphaRemote.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		require.Equal(t, "Reconciled name", final.DisplayName)
		require.Greater(t, final.Revision, manual.Revision)
	})
	t.Run("OR-J05-S04/archive-keeps-purchased-access-and-money-history", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		product, price := publicSale(t, a, "archive", 9_990_000, nil)
		order := publicPurchase(t, a, p, price.ID, 1, 9_990_000, "before-archive")
		archived, err := a.alphaRemote.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Archived: catalog.Value(true)})
		require.NoError(t, err)
		require.True(t, archived.Archived)
		kept, err := a.alphaRemote.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderComplete, kept.Status)
		require.EqualValues(t, 9_990_000, kept.Total)
		require.Equal(t, product.ID, *kept.Lines[0].ProductID)
		payment, err := a.alphaRemote.GetPayment(t.Context(), *order.Payment.PaymentID)
		require.NoError(t, err)
		require.EqualValues(t, 9_990_000, payment.Amount)
		access, err := a.alphaRemote.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: order.CustomerID, Entitlements: []string{"library:read"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["library:read"])
		_, err = a.alphaRemote.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: order.CustomerID, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: "after-archive"})
		require.Error(t, err)
		require.Len(t, p.effects(t), 1)
	})
	t.Run("OR-J05-S05/accepted-order-terms-survive-price-and-PSP-edits", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		product, price := publicSale(t, a, "immutable", 9_990_000, ptr(24))
		order, err := a.alphaRemote.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Lines: []billing.OrderLineParams{{PriceID: price.ID, Quantity: ptr(3)}}, IdempotencyKey: "accepted-terms"})
		require.NoError(t, err)
		next, err := a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: price.Key, UnitAmount: 14_990_000, Currency: "USD", AccessDurationHours: ptr(48), ExpectedRevision: &price.Revision})
		require.NoError(t, err)
		require.NotEqual(t, price.ID, next.ID)
		psps, err := a.alphaRemote.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		_, err = a.alphaRemote.UpdatePSP(t.Context(), psps.Items[0].ID, billing.UpdatePSPParams{PublishableKey: ptr("pk_test_rotated"), ExpectedRevision: &psps.Items[0].Revision})
		require.NoError(t, err)
		kept, err := a.alphaRemote.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.EqualValues(t, 29_970_000, kept.Total)
		require.Equal(t, price.ID, *kept.Lines[0].PriceID)
		require.EqualValues(t, 9_990_000, kept.Lines[0].UnitAmount)
		require.Equal(t, 24, *kept.Lines[0].AccessDurationHours)
		status, raw := a.request(t, "POST", "/v1/me/orders/"+order.ID.String()+"/pay", a.aliceToken, billing.PayOrderParams{ExpectedTotal: 29_970_000, Payment: billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}})
		require.Equal(t, http.StatusOK, status, string(raw))
		var paid billing.Order
		require.NoError(t, json.Unmarshal(raw, &paid))
		require.Equal(t, order.ID, paid.ID)
		require.Equal(t, billing.OrderComplete, paid.Status)
		require.EqualValues(t, 29_970_000, paid.Total)
		effects := p.effects(t)
		require.Len(t, effects, 1)
		require.EqualValues(t, 2997, effects[0].Amount)
	})
}
