//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
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

func TestPriceIdentityBatchesAreBoundedByTheirInputs(t *testing.T) {
	w := newWorld(t)
	c := w.client[remote]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "bounded-prices", DisplayName: "Batch bounds"})
	require.NoError(t, err)
	require.NoError(t, w.catalogExec(t.Context(), w.sql(`INSERT INTO billing.prices (id,merchant_id,product_id,key,amount,currency,archived)
SELECT gen_random_uuid(),$1,$2,'option-'||i,1000000,'USD',i=1 FROM generate_series(1,101) AS i`), c.MerchantID().UUID(), product.ID.UUID()))
	all := make([]string, billing.MaxBatchItems)
	public := make([]billing.PriceID, len(all))
	for i := range all {
		all[i] = fmt.Sprintf("bounded-prices:option-%d:v1", i+1)
		public[i] = billing.PriceID(all[i])
	}
	runtime := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(t.Context(), c.MerchantID())
	require.NoError(t, runtime.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		for _, tc := range []struct {
			name      string
			ids, want []string
		}{
			{"nil", nil, nil},
			{"empty", []string{}, nil},
			{"unknown", []string{"absent:price:v1"}, nil},
			{"duplicate and unknown", []string{all[0], "absent:price:v1", all[0]}, []string{all[0]}},
			{"full batch", all, all},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rows, err := runtime.DB.Gen(ctx).ListPriceStorageIDs(ctx, gen.ListPriceStorageIDsParams{MerchantID: c.MerchantID().UUID(), Ids: tc.ids})
				require.NoError(t, err)
				require.LessOrEqual(t, len(rows), len(tc.ids))
				got := make([]string, 0, len(rows))
				for _, row := range rows {
					got = append(got, row.PublicID)
				}
				require.ElementsMatch(t, tc.want, got)
			})
		}
		rows, err := runtime.DB.Gen(ctx).ListPriceStorageIDs(ctx, gen.ListPriceStorageIDsParams{MerchantID: uuid.New(), Ids: all})
		require.NoError(t, err)
		require.Empty(t, rows, "identity resolution remains merchant-scoped")
		return nil
	}))
	read, err := c.ListPrices(t.Context(), billing.PriceListParams{IDs: public})
	require.NoError(t, err)
	require.Len(t, read.Items, billing.MaxBatchItems, "an input-sized SQL bound must not truncate a valid full batch")
	_, err = c.ListPrices(t.Context(), billing.PriceListParams{IDs: append(public, "bounded-prices:option-101:v1")})
	require.Error(t, err, "the public batch boundary remains 100 IDs")
}
