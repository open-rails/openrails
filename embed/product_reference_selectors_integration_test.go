//go:build integration

package embed_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/httptesthost"
	"github.com/stretchr/testify/require"
)

func TestExplicitProductSelectorsEmbeddedAndRemote(t *testing.T) {
	ctx := t.Context()
	_, pool, dsn := scopeWithoutRLSDatabase(t)
	rt, mid, err := newDeclaredMerchant(ctx, embed.Options{Config: &config.Config{TestMode: config.CredentialPostureSandbox, AllowCatalogUpdates: true, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: dsn}}, PGXPool: pool, River: embed.RiverManagedByOpenRails(), StripeTransport: catalogAuthorityTransport{t: t}}, "selectors-"+uuid.NewString(), embed.MerchantConfig{DisplayName: "Product selectors", PSPs: map[string]embed.PSPConfig{"stripe": {"stripe": {AccountID: "acct_selectors", Secrets: map[string]string{"secret_key": "sk_test_selectors", "webhook_signing_secret": "whsec_selectors"}}}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
	local, err := rt.Client()
	require.NoError(t, err)
	handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{Catalog: true, MerchantAdmin: true, MerchantAPI: true}, Gate: creatorAdminTestGate{mid: mid}})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	remote, err := openrails.NewRemote(server.URL, openrails.WithAPIKey("administrator"), openrails.WithMerchantID(mid))
	require.NoError(t, err)
	for _, item := range []struct {
		name   string
		client *openrails.Client
	}{{"embedded", local}, {"remote", remote}} {
		t.Run(item.name, func(t *testing.T) {
			client := item.client
			idProduct, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: "id-target-" + uuid.NewString(), DisplayName: "ID product"})
			require.NoError(t, err)
			idTarget, err := openrails.ParseProductID(idProduct.ID)
			require.NoError(t, err)
			key := idTarget.UUID().String()
			product, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: key, DisplayName: "Key product"})
			require.NoError(t, err)
			price, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductKey: key, Key: "offer-" + key, UnitAmount: 1_000_000, Currency: "USD"})
			require.NoError(t, err)
			require.Equal(t, product.ID, price.ProductID)
			idPrice, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: idProduct.ID, Key: "id-offer-" + key, UnitAmount: 1_000_000, Currency: "USD"})
			require.NoError(t, err)
			require.Equal(t, idProduct.ID, idPrice.ProductID)
			require.NotEqual(t, idPrice.ProductID, price.ProductID, "UUID-looking keys cannot be interpreted as IDs")
			_, err = client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, ProductKey: key, UnitAmount: 1_000_000, Currency: "USD"})
			require.ErrorIs(t, err, openrails.ErrInvalid)
			_, err = client.Prices.Create(ctx, &openrails.PriceCreateParams{UnitAmount: 1_000_000, Currency: "USD"})
			require.ErrorIs(t, err, openrails.ErrInvalid)
			inline, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductData: &openrails.PriceCreateProductDataParams{Key: "inline-" + key, DisplayName: "Inline"}, Key: "inline-price-" + key, UnitAmount: 1_000_000, Currency: "USD"})
			require.NoError(t, err)
			require.NotEmpty(t, inline.ProductID)
			alice, err := client.ForCatalogOwner("alice-" + item.name)
			require.NoError(t, err)
			bob, err := client.ForCatalogOwner("bob-" + item.name)
			require.NoError(t, err)
			owned, err := alice.Products.Create(ctx, &openrails.ProductCreateParams{Key: "owned-" + key, DisplayName: "Owned"})
			require.NoError(t, err)
			_, err = bob.Prices.Create(ctx, &openrails.PriceCreateParams{ProductKey: owned.Key, Key: "foreign-offer-" + key, UnitAmount: 1_000_000, Currency: "USD"})
			require.ErrorIs(t, err, openrails.ErrNotFound)
			customer := uuid.NewString()
			dbtest.EnsureCustomerIDPgxFor(ctx, t, pool, mid.UUID(), customer)
			pid, err := openrails.ParseProductID(product.ID)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO billing.grants(merchant_id,customer_id,product_id,kind,source_type,source_id,event,starts_at) VALUES($1,$2,$3,'ownership','admin',$4,'grant',NOW())`, mid.UUID(), customer, pid.UUID(), uuid.NewString())
			require.NoError(t, err)
			archived := true
			_, err = client.Products.Update(ctx, product.ID, &openrails.ProductUpdateParams{Archived: &archived})
			require.NoError(t, err)
			access, err := client.ProductAccess.Check(ctx, &openrails.ProductAccessCheckParams{CustomerID: customer, ProductKey: key})
			require.NoError(t, err)
			require.True(t, access.HasAccess)
			require.Equal(t, product.ID, access.ProductID)
			require.Equal(t, key, access.ProductKey)
			byID, err := client.ProductAccess.Check(ctx, &openrails.ProductAccessCheckParams{CustomerID: customer, ProductID: idProduct.ID})
			require.NoError(t, err)
			require.False(t, byID.HasAccess)
			batch, err := client.ProductAccess.CheckMany(ctx, &openrails.ProductAccessCheckManyParams{CustomerID: customer, ProductKeys: []string{key, key, "missing"}})
			require.NoError(t, err)
			require.Equal(t, map[string]bool{key: true, "missing": false}, batch)
			_, err = client.ProductAccess.Check(ctx, &openrails.ProductAccessCheckParams{CustomerID: customer, ProductID: product.ID, ProductKey: key})
			require.ErrorIs(t, err, openrails.ErrInvalid)
			_, err = client.ProductAccess.CheckMany(ctx, &openrails.ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: []string{product.ID}, ProductKeys: []string{key}})
			require.ErrorIs(t, err, openrails.ErrInvalid)
			_, err = client.ProductAccess.CheckMany(ctx, &openrails.ProductAccessCheckManyParams{CustomerID: customer})
			require.ErrorIs(t, err, openrails.ErrInvalid)
		})
	}
}
