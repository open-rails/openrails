//go:build integration

package embed_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/httptesthost"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func TestCatalogResourceAtomicOffers(t *testing.T) {
	ctx := t.Context()
	owner, pool, dsn := scopeWithoutRLSDatabase(t)
	_ = owner
	rt, err := embed.New(ctx, embed.Options{Config: &config.Config{
		Env: "development", TestMode: config.CredentialPostureSandbox,
		MerchantConfigSource: config.MerchantConfigSourceManifest, CatalogSource: config.CatalogSourceAPI,
		ProviderWriteMode: config.ProviderWriteModeReadOnly, DB: &config.DBConfig{URL: dsn},
	}, PGXPool: pool, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
	mid, err := rt.UpsertMerchantConfig(ctx, "inline-"+uuid.NewString(), embed.MerchantConfig{DisplayName: "Inline catalog"})
	require.NoError(t, err)
	local, err := rt.Client()
	require.NoError(t, err)
	handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{Catalog: true, Gate: creatorAdminTestGate{mid: mid}}})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	remote, err := openrails.NewRemote(server.URL, openrails.WithAPIKey("administrator"))
	require.NoError(t, err)
	for _, transport := range []struct {
		name   string
		client *openrails.Client
	}{{"embedded", local}, {"remote", remote}} {
		t.Run(transport.name, func(t *testing.T) {
			alice, err := transport.client.ForCatalogOwner("alice-" + transport.name)
			require.NoError(t, err)
			bob, err := transport.client.ForCatalogOwner("bob-" + transport.name)
			require.NoError(t, err)
			params := openrails.PriceCreateParams{ProductData: &openrails.PriceCreateProductDataParams{Key: "post-" + transport.name, DisplayName: "Original title"}, Key: "offer-" + transport.name, UnitAmount: 1_000_000, Currency: "usd"}
			var group errgroup.Group
			offers := make([]*openrails.Price, 12)
			for i := range offers {
				group.Go(func() error { var err error; offers[i], err = alice.Prices.Create(ctx, &params); return err })
			}
			require.NoError(t, group.Wait())
			for _, offer := range offers {
				require.Equal(t, offers[0].ID, offer.ID)
				require.Equal(t, offers[0].ProductID, offer.ProductID)
			}
			offer := offers[0]
			product, err := alice.Products.Retrieve(ctx, offer.ProductID)
			require.NoError(t, err)
			require.Equal(t, "Original title", product.DisplayName)
			_, err = bob.Products.Retrieve(ctx, offer.ProductID)
			require.ErrorIs(t, err, openrails.ErrNotFound, "resource clone must retain owner attenuation")
			_, err = bob.Prices.Create(ctx, &params)
			require.ErrorIs(t, err, openrails.ErrConflict, "natural product key cannot transfer catalogs")
			changed := params
			changed.UnitAmount = 2_000_000
			_, err = alice.Prices.Create(ctx, &changed)
			require.ErrorIs(t, err, openrails.ErrConflict, "same offer key cannot change its immutable terms")
			changed.Key += "-2"
			changed.ProductData = &openrails.PriceCreateProductDataParams{Key: params.ProductData.Key, DisplayName: "Uncommitted blog edit"}
			newer, err := alice.Prices.Create(ctx, &changed)
			require.NoError(t, err)
			require.NotEqual(t, offer.ID, newer.ID)
			require.Equal(t, offer.ProductID, newer.ProductID)
			original, err := alice.Prices.Retrieve(ctx, offer.ID)
			require.NoError(t, err)
			require.False(t, original.Archived)
			product, err = alice.Products.Retrieve(ctx, offer.ProductID)
			require.NoError(t, err)
			require.Equal(t, "Original title", product.DisplayName)
			invalid := params
			invalid.Key += "-invalid"
			invalid.ProductData = &openrails.PriceCreateProductDataParams{Key: params.ProductData.Key + "-invalid", DisplayName: "Must roll back"}
			invalid.UnitAmount = -1
			_, err = alice.Prices.Create(ctx, &invalid)
			require.ErrorIs(t, err, openrails.ErrInvalid)
			_, err = alice.Products.RetrieveByKey(ctx, invalid.ProductData.Key)
			require.ErrorIs(t, err, openrails.ErrNotFound, "invalid initial price must leave no orphan product")
			both := params
			both.ProductID = offer.ProductID
			_, err = alice.Prices.Create(ctx, &both)
			require.ErrorIs(t, err, openrails.ErrInvalid)
			badID := params
			badID.ProductData = nil
			badID.ProductID = offer.ID
			_, err = alice.Prices.Create(ctx, &badID)
			require.ErrorIs(t, err, openrails.ErrInvalid, "wrong prefixed string IDs are refused at the boundary")
			archived := true
			_, err = alice.Products.Update(ctx, product.ID, &openrails.ProductUpdateParams{Archived: &archived})
			require.NoError(t, err)
			archived = false
			updated, err := alice.Products.Update(ctx, product.ID, &openrails.ProductUpdateParams{Archived: &archived})
			require.NoError(t, err)
			require.False(t, updated.Archived, "explicit false differs from omitted")
			page, err := alice.Prices.List(ctx, nil)
			require.NoError(t, err)
			require.Len(t, page.Items, 2)
		})
	}
	t.Run("legacy provider inline refused without orphan", func(t *testing.T) {
		params := &openrails.PriceCreateParams{ProductData: &openrails.PriceCreateProductDataParams{Key: "legacy-inline", DisplayName: "Not a provider transaction"}, Key: "legacy-offer", UnitAmount: 1_000_000, Currency: "USD", PSPs: []string{"stripe"}}
		_, err := local.Prices.Create(ctx, params)
		require.ErrorIs(t, err, openrails.ErrInvalid, fmt.Sprint(err))
		_, err = local.Products.RetrieveByKey(ctx, "legacy-inline")
		require.ErrorIs(t, err, openrails.ErrNotFound)
	})
}
