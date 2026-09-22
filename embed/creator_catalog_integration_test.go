//go:build integration

package embed_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/httptesthost"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestCreatorCatalogAuthority(t *testing.T) {
	ctx := t.Context()
	owner, _, runtimeDSN := scopeWithoutRLSDatabase(t)
	ownerURL, err := url.Parse(runtimeDSN)
	require.NoError(t, err)
	credentials := owner.Config().ConnConfig
	ownerURL.User = url.UserPassword(credentials.User, credentials.Password)
	newRuntime := func() (*embed.Runtime, merchant.ID, *openrails.Client) {
		rt, err := embed.New(ctx, embed.Options{
			Config: &config.Config{
				Env: "development", TestMode: config.CredentialPostureSandbox,
				MerchantConfigSource: config.MerchantConfigSourceManifest, CatalogSource: config.CatalogSourceAPI,
				ProviderWriteMode: config.ProviderWriteModeReadOnly,
				DB:                &config.DBConfig{URL: ownerURL.String()},
			},
			PGXPool: owner, River: embed.RiverManagedByOpenRails(),
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
		mid, err := rt.UpsertMerchantConfig(ctx, "creator-"+uuid.NewString(), embed.MerchantConfig{DisplayName: "Creator platform"})
		require.NoError(t, err)
		admin, err := rt.Client()
		require.NoError(t, err)
		return rt, mid, admin
	}
	rt, mid, admin := newRuntime()
	const subjectA = "creator|Alice/雪:%2f"
	const subjectB = "creator:bob"
	alice, err := rt.CatalogClient(subjectA)
	require.NoError(t, err)
	bob, err := rt.CatalogClient(subjectB)
	require.NoError(t, err)
	catA, err := alice.EnsureOwnCatalog(ctx)
	require.NoError(t, err)
	catB, err := bob.EnsureOwnCatalog(ctx)
	require.NoError(t, err)
	require.NotEqual(t, catA.ID, catB.ID)
	require.Equal(t, subjectA, *catA.OwnerSubject)
	require.Equal(t, openrails.MerchantID(mid), catA.MerchantID)
	var group errgroup.Group
	for range 8 {
		group.Go(func() error {
			catalog, err := alice.EnsureOwnCatalog(ctx)
			if err != nil {
				return err
			}
			if catalog.ID != catA.ID {
				return fmt.Errorf("concurrent ensure created another catalog")
			}
			return nil
		})
	}
	require.NoError(t, group.Wait())
	productA, err := alice.CreateProduct(ctx, openrails.CreateProductRequest{Key: "alice-post", DisplayName: "Alice post", CatalogID: catA.ID})
	require.NoError(t, err)
	productB, err := bob.CreateProduct(ctx, openrails.CreateProductRequest{Key: "bob-post", DisplayName: "Bob post"})
	require.NoError(t, err)
	require.Equal(t, catB.ID, productB.CatalogID)
	priceA, err := alice.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: productA.ID, Key: "alice-usd", UnitAmount: 5_000_000, Currency: "USD"})
	require.NoError(t, err)
	priceB, err := bob.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: productB.ID, Key: "bob-usd", UnitAmount: 7_000_000, Currency: "USD"})
	require.NoError(t, err)

	t.Run("owner reads and writes stay inside the catalog", func(t *testing.T) {
		_, err := alice.GetProduct(ctx, productB.ID)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = alice.GetProductByKey(ctx, productB.Key)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		title := "Stolen"
		_, err = alice.UpdateProduct(ctx, productB.ID, openrails.UpdateProductRequest{DisplayName: &title})
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = alice.GetPrice(ctx, priceB.ID)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = alice.GetPriceByKey(ctx, priceB.Key)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = alice.SetPriceKey(ctx, priceB.ID, "hijacked")
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = alice.SetPriceKey(ctx, priceA.ID, priceB.Key)
		require.ErrorIs(t, err, openrails.ErrConflict, "an owned price cannot take another catalog's key")
		unchangedPrice, err := bob.GetPrice(ctx, priceB.ID)
		require.NoError(t, err)
		require.False(t, unchangedPrice.Archived)
		_, err = alice.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: productB.ID, Key: "forged-price", UnitAmount: 1_000_000, Currency: "USD"})
		require.ErrorIs(t, err, openrails.ErrNotFound)
		products, err := alice.ListProducts(ctx, openrails.ProductFilter{})
		require.NoError(t, err)
		require.EqualValues(t, 1, products.Total)
		require.Len(t, products.Items, 1)
		require.Equal(t, productA.ID, products.Items[0].ID)
		prices, err := alice.ListPrices(ctx, openrails.PriceFilter{})
		require.NoError(t, err)
		require.EqualValues(t, 1, prices.Total)
		require.Len(t, prices.Items, 1)
		require.Equal(t, priceA.ID, prices.Items[0].ID)
		unchanged, err := bob.GetProduct(ctx, productB.ID)
		require.NoError(t, err)
		require.Equal(t, "Bob post", unchanged.DisplayName)
	})

	t.Run("selectors and fields cannot expand creator authority", func(t *testing.T) {
		_, err := alice.CreateProduct(ctx, openrails.CreateProductRequest{Key: "foreign-catalog", DisplayName: "Forged", CatalogID: catB.ID})
		require.Error(t, err)
		_, err = alice.CreateProduct(ctx, openrails.CreateProductRequest{Key: productB.Key, DisplayName: "Collision"})
		require.ErrorIs(t, err, openrails.ErrConflict)
		_, err = alice.UpdateProduct(ctx, productA.ID, openrails.UpdateProductRequest{SetEntitlements: true, EntitlementsSpec: map[string]*int{"platform-wide": nil}})
		require.Error(t, err)
		_, err = alice.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: productA.ID, UnitAmount: 2_000_000, Currency: "USD", PSPs: []string{"stripe"}})
		require.Error(t, err)
		_, err = alice.UpdatePrice(ctx, priceA.ID, openrails.UpdatePriceRequest{PSPLinks: map[string]map[string]string{"stripe": {"price_id": "price_forged"}}})
		require.Error(t, err)
		_, err = alice.EnsureCatalogForOwner(ctx, subjectB)
		require.ErrorIs(t, err, openrails.ErrDenied)
		_, err = alice.ListCatalogs(ctx, openrails.PageOptions{})
		require.ErrorIs(t, err, openrails.ErrDenied)
	})

	t.Run("administrator and merchant default remain explicit", func(t *testing.T) {
		catalog, err := admin.EnsureCatalogForOwner(ctx, subjectA)
		require.NoError(t, err)
		require.Equal(t, catA.ID, catalog.ID)
		title := "Moderated"
		updated, err := admin.UpdateProduct(ctx, productB.ID, openrails.UpdateProductRequest{DisplayName: &title})
		require.NoError(t, err)
		require.Equal(t, catB.ID, updated.CatalogID)
		plain, err := admin.CreateProduct(ctx, openrails.CreateProductRequest{Key: "merchant-default", DisplayName: "Default"})
		require.NoError(t, err)
		defaultCatalog, err := admin.GetCatalog(ctx, plain.CatalogID)
		require.NoError(t, err)
		require.Nil(t, defaultCatalog.OwnerSubject)
		_, err = alice.GetProduct(ctx, plain.ID)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = owner.Exec(ctx, `UPDATE billing.catalogs SET owner_subject='new-owner' WHERE id=$1`, catA.ID.UUID())
		require.Error(t, err)
		_, err = owner.Exec(ctx, `UPDATE billing.products SET catalog_id=$1 WHERE id=$2`, catB.ID.UUID(), productA.ID.UUID())
		require.Error(t, err)
	})

	t.Run("same host subject in another merchant remains separate", func(t *testing.T) {
		otherRuntime, _, otherAdmin := newRuntime()
		otherOwner, err := otherRuntime.CatalogClient(subjectA)
		require.NoError(t, err)
		otherCatalog, err := otherOwner.EnsureOwnCatalog(ctx)
		require.NoError(t, err)
		require.NotEqual(t, catA.ID, otherCatalog.ID)
		_, err = otherOwner.GetProduct(ctx, productA.ID)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = otherAdmin.CreateProduct(ctx, openrails.CreateProductRequest{CatalogID: catA.ID, Key: "cross-merchant", DisplayName: "Forbidden"})
		require.Error(t, err)
	})

	t.Run("HTTP identity comes only from the gate", func(t *testing.T) {
		handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{Catalog: true, Gate: creatorTestGate{mid: mid, subject: subjectA}}})
		require.NoError(t, err)
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		remote, err := openrails.NewRemote(server.URL, openrails.WithOwnCatalog(), openrails.WithAPIKey("owner"))
		require.NoError(t, err)
		catalog, err := remote.EnsureOwnCatalog(ctx)
		require.NoError(t, err)
		require.Equal(t, catA.ID, catalog.ID)
		_, err = remote.GetProduct(ctx, productB.ID)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		status, body := creatorRawRequest(t, server.URL, "owner", http.MethodPut, "/v1/catalog", map[string]string{"owner_subject": subjectB, "merchant_id": uuid.NewString()})
		require.Equal(t, http.StatusBadRequest, status, string(body))
		own, err := remote.EnsureOwnCatalog(ctx)
		require.NoError(t, err)
		require.Equal(t, subjectA, *own.OwnerSubject)
		status, _ = creatorRawRequest(t, server.URL, "missing", http.MethodPut, "/v1/catalog", map[string]string{"owner_subject": subjectA})
		require.Equal(t, http.StatusForbidden, status)
		status, _ = creatorRawRequest(t, server.URL, "owner", http.MethodPost, "/v1/merchant/catalogs", map[string]string{"owner_subject": subjectB})
		require.Equal(t, http.StatusForbidden, status)
		status, _ = creatorRawRequest(t, server.URL, "owner", http.MethodPost, "/v1/catalog/publish", map[string]any{})
		require.Equal(t, http.StatusNotFound, status)
		status, _ = creatorRawRequest(t, server.URL, "owner", http.MethodPatch, "/v1/catalog/products/"+productA.ID.String(), map[string]string{"display_name": "HTTP edit", "owner_subject": subjectB, "catalog_id": catB.ID.String()})
		require.Equal(t, http.StatusBadRequest, status)
		read, err := alice.GetProduct(ctx, productA.ID)
		require.NoError(t, err)
		require.Equal(t, catA.ID, read.CatalogID, "forged fields must never reassign ownership")
		status, body = creatorRawRequest(t, server.URL, "owner", http.MethodGet, "/v1/catalog/prices/by-key/"+priceA.Key+"/history", nil)
		require.Equal(t, http.StatusOK, status, string(body))
		status, _ = creatorRawRequest(t, server.URL, "owner", http.MethodGet, "/v1/catalog/prices/by-key/"+priceB.Key+"/history", nil)
		require.Equal(t, http.StatusNotFound, status)
	})
}

type creatorTestGate struct {
	mid     merchant.ID
	subject string
}

func (g creatorTestGate) Authorize(_ context.Context, request *http.Request, permission string) (billingauth.Principal, error) {
	if permission != permissions.MerchantCatalogOwnRead && permission != permissions.MerchantCatalogOwnUpdate {
		return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "creator scope only"}
	}
	subject := g.subject
	if request.Header.Get("Authorization") == "Bearer missing" {
		subject = ""
	}
	return billingauth.Principal{MerchantID: g.mid, Subject: subject, Permissions: []string{permission}}, nil
}

func creatorRawRequest(t *testing.T, base, token, method, path string, input any) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(input)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Catalog-Owner", "forged")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	require.NoError(t, err)
	require.False(t, strings.Contains(string(raw), "sk_test_"))
	return response.StatusCode, raw
}
