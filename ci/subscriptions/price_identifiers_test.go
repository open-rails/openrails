//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
)

func TestPriceIdentifiersPreserveAcceptedVersions(t *testing.T) {
	w := orderWorld(t)
	c := w.client[remote]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "premium", DisplayName: "Premium"})
	require.NoError(t, err)
	create := func(amount int64) *billing.Price {
		t.Helper()
		price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "buy-usd", UnitAmount: amount, Currency: "USD"})
		require.NoError(t, err)
		return price
	}
	first := create(1_000_000)
	require.Equal(t, "premium:buy-usd:v1", first.ID.String())
	require.EqualValues(t, 1, first.Version)
	storageID := w.priceStorageID(first.ID)
	customer := w.newCustomer()
	order, err := c.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: customer.cid(), Lines: []billing.OrderLineParams{{PriceID: first.ID}}, IdempotencyKey: "version-one-order"})
	require.NoError(t, err)
	second := create(2_000_000)
	require.Equal(t, "premium:buy-usd:v2", second.ID.String())
	require.EqualValues(t, 2, second.Version)
	read, err := c.GetOrder(t.Context(), order.ID)
	require.NoError(t, err)
	require.Equal(t, first.ID, *read.Lines[0].PriceID)
	require.EqualValues(t, 1_000_000, read.Total)
	archived, err := c.GetPrice(t.Context(), first.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.True(t, archived.Archived)
	restored := create(1_000_000)
	require.Equal(t, first.ID, restored.ID)
	require.Equal(t, storageID, w.priceStorageID(restored.ID))
	third := create(3_000_000)
	require.Equal(t, "premium:buy-usd:v3", third.ID.String())
	filtered, err := c.ListPrices(t.Context(), billing.PriceListParams{IDs: []billing.PriceID{first.ID, third.ID}})
	require.NoError(t, err)
	require.Len(t, filtered.Items, 2)
	require.Equal(t, http.StatusBadRequest, w.staffCall(http.MethodGet, "/v1/catalog/prices/price_"+storageID.String(), nil, nil))

	// Historical receipts and archives retain private row identities; reading
	// an old stored wire receipt resolves them without accepting UUID inputs.
	runtime := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(t.Context(), c.MerchantID())
	require.NoError(t, runtime.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		raw, err := json.Marshal(map[string]any{"price_id": "price_" + storageID.String(), "amount": "9007199254740993"})
		if err != nil {
			return err
		}
		projected, err := db.StoredPriceReferences(ctx, runtime.DB.Gen(ctx), c.MerchantID().UUID(), raw)
		if err != nil {
			return err
		}
		require.JSONEq(t, `{"price_id":"premium:buy-usd:v1","amount":"9007199254740993"}`, string(projected))
		_, err = db.StoredPriceReferences(ctx, runtime.DB.Gen(ctx), uuid.New(), raw)
		require.Error(t, err, "a reference in another merchant is never resolved")
		return nil
	}))
}

func TestPriceIdentifierEscapesKeysAndHTTPPaths(t *testing.T) {
	w := newWorld(t)
	c := w.client[remote]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "book:é/100%", DisplayName: "Book"})
	require.NoError(t, err)
	price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "buy:USD/+", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	expected, err := billing.NewPriceID(product.Key, price.Key, 1)
	require.NoError(t, err)
	require.Equal(t, expected, price.ID, "PostgreSQL and Go use the same key encoding")
	read, err := c.GetPrice(t.Context(), expected, billing.GetPriceParams{})
	require.NoError(t, err)
	require.Equal(t, expected, read.ID)
	updated, err := c.UpdatePrice(t.Context(), expected, billing.UpdatePriceParams{Archived: catalog.Value(true)})
	require.NoError(t, err)
	require.Equal(t, expected, updated.ID)
	require.True(t, updated.Archived)
	history, err := c.ListPriceHistory(t.Context(), expected, billing.PageRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, history.Items)
	for _, movement := range history.Items {
		require.Equal(t, expected, movement.Price.ID)
	}
}
