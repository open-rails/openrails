//go:build integration

package embed_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
)

// TestCatalogListFiltersAreTriStateAcrossDeployments proves the archived
// filter on product and price listings: unset lists everything, false lists
// live rows only, true lists archived rows only — identically through the
// embedded in-process and standalone HTTP Clients. (The price handler used to
// read active_only=false as archived-only.)
func TestCatalogListFiltersAreTriStateAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone("USD")
	runtime, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	local, err := runtime.Client(openrails.WithMerchantID(dbtest.TestMerchantID))
	require.NoError(t, err)

	live, archived := false, true
	for name, client := range map[string]*openrails.Client{"embedded": local, "standalone": standalone.Client()} {
		t.Run(name, func(t *testing.T) {
			group := "tri-" + uuid.NewString()[:8]
			product, err := client.CreateProduct(ctx, openrails.CreateProductRequest{Key: group + "-live", DisplayName: "Live", TierGroup: &group})
			require.NoError(t, err)
			retired, err := client.CreateProduct(ctx, openrails.CreateProductRequest{Key: group + "-old", DisplayName: "Old", TierGroup: &group})
			require.NoError(t, err)
			_, err = client.UpdateProduct(ctx, retired.ID, openrails.UpdateProductRequest{Archived: &archived})
			require.NoError(t, err)

			duration := 720
			price, err := client.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: product.ID, Key: group + "-price-live", UnitAmount: 1_000_000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
			require.NoError(t, err)
			oldPrice, err := client.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: product.ID, Key: group + "-price-old", UnitAmount: 2_000_000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
			require.NoError(t, err)
			_, err = client.UpdatePrice(ctx, oldPrice.ID, openrails.UpdatePriceRequest{Archived: &archived})
			require.NoError(t, err)

			productIDs := func(filter openrails.ProductFilter) []uuid.UUID {
				filter.TierGroup = group
				page, err := client.ListProducts(ctx, filter)
				require.NoError(t, err)
				require.EqualValues(t, len(page.Items), page.Total)
				out := make([]uuid.UUID, 0, len(page.Items))
				for _, p := range page.Items {
					out = append(out, p.ID)
				}
				return out
			}
			require.ElementsMatch(t, []uuid.UUID{product.ID, retired.ID}, productIDs(openrails.ProductFilter{}), "unset lists live and archived products")
			require.Equal(t, []uuid.UUID{product.ID}, productIDs(openrails.ProductFilter{Archived: &live}))
			require.Equal(t, []uuid.UUID{retired.ID}, productIDs(openrails.ProductFilter{Archived: &archived}))

			priceIDs := func(filter openrails.PriceFilter) []uuid.UUID {
				filter.ProductID = &product.ID
				page, err := client.ListPrices(ctx, filter)
				require.NoError(t, err)
				require.EqualValues(t, len(page.Items), page.Total)
				out := make([]uuid.UUID, 0, len(page.Items))
				for _, p := range page.Items {
					out = append(out, p.ID)
				}
				return out
			}
			require.ElementsMatch(t, []uuid.UUID{price.ID, oldPrice.ID}, priceIDs(openrails.PriceFilter{}), "unset lists live and archived prices")
			require.Equal(t, []uuid.UUID{price.ID}, priceIDs(openrails.PriceFilter{Archived: &live}))
			require.Equal(t, []uuid.UUID{oldPrice.ID}, priceIDs(openrails.PriceFilter{Archived: &archived}))
		})
	}
}
