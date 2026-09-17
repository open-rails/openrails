//go:build integration

package embed_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/stretchr/testify/require"
)

func TestCatalogClientSharedWorkflow(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	remote := h.StartStandalone("USD")
	runtime, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	local, err := runtime.Client(openrails.WithMerchantID(dbtest.TestMerchantID))
	require.NoError(t, err)
	for name, client := range map[string]*openrails.Client{"embedded": local, "standalone": remote.Client()} {
		t.Run(name, func(t *testing.T) {
			key := "client-catalog-" + uuid.NewString()
			product, err := client.CreateProduct(ctx, openrails.CreateProductRequest{Key: key, DisplayName: "Initial", EntitlementsSpec: map[string]*int{"access": nil}})
			require.NoError(t, err)
			id, err := client.EnsureUsageProduct(ctx, key, "Must not overwrite existing")
			require.NoError(t, err)
			require.Equal(t, product.ID, id)
			read, err := client.GetProductByKey(ctx, key)
			require.NoError(t, err)
			require.Equal(t, "Initial", read.DisplayName)
			title := "Renamed"
			updated, err := client.UpdateProduct(ctx, product.ID, openrails.UpdateProductRequest{DisplayName: &title})
			require.NoError(t, err)
			require.Equal(t, title, updated.DisplayName)
			require.Contains(t, updated.EntitlementsSpec, "access")
			_, err = client.GetProduct(ctx, openrails.ProductID(uuid.New()))
			require.ErrorIs(t, err, openrails.ErrNotFound)
			duration := 720
			price, err := client.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: product.ID, Key: key + "-monthly", UnitAmount: 1234567, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
			require.NoError(t, err)
			require.EqualValues(t, 1234567, price.UnitAmount)
			priced, err := client.GetPriceByKey(ctx, price.Key)
			require.NoError(t, err)
			require.Equal(t, price.ID, priced.ID)
			priced, err = client.GetPrice(ctx, price.ID)
			require.NoError(t, err)
			require.Equal(t, product.ID, priced.ProductID)
			live, retired := false, true
			prices, err := client.ListPrices(ctx, openrails.PriceFilter{ProductID: product.ID, Archived: &live})
			require.NoError(t, err)
			require.Len(t, prices.Items, 1)
			priced, err = client.UpdatePrice(ctx, price.ID, openrails.UpdatePriceRequest{Archived: &retired})
			require.NoError(t, err)
			require.True(t, priced.Archived)
			prices, err = client.ListPrices(ctx, openrails.PriceFilter{ProductID: product.ID, Archived: &live})
			require.NoError(t, err)
			require.Empty(t, prices.Items)
			// No archived filter lists every price; the archived-only filter is explicit.
			prices, err = client.ListPrices(ctx, openrails.PriceFilter{ProductID: product.ID})
			require.NoError(t, err)
			require.Len(t, prices.Items, 1, "an unset filter lists archived and live prices")
			prices, err = client.ListPrices(ctx, openrails.PriceFilter{ProductID: product.ID, Archived: &retired})
			require.NoError(t, err)
			require.Len(t, prices.Items, 1)
			require.True(t, prices.Items[0].Archived)
			// Archived identity remains addressable for existing obligations.
			priced, err = client.GetPrice(ctx, price.ID)
			require.NoError(t, err)
			require.True(t, priced.Archived)
			products, err := client.ListProducts(ctx, openrails.ProductFilter{PageOptions: openrails.PageOptions{Limit: 100}})
			require.NoError(t, err)
			require.Positive(t, products.Total)
		})
	}
}
