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

		rt, mid, err := newDeclaredMerchant(ctx, embed.Options{
			Config: &config.Config{
				TestMode:             config.CredentialPostureSandbox,
				MerchantConfigSource: config.MerchantConfigSourceManifest, AllowCatalogUpdates: true,
				ProviderWriteMode: config.ProviderWriteModeReadOnly,
				DB:                &config.DBConfig{URL: ownerURL.String()},
			},
			PGXPool: owner, River: embed.RiverManagedByOpenRails(),
		}, "creator-"+uuid.NewString(), embed.MerchantConfig{DisplayName: "Creator platform"})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
		admin, err := rt.Client()
		require.NoError(t, err)
		return rt, mid, admin
	}
	rt, mid, admin := newRuntime()
	t.Run("missing owner reads do not create catalogs", func(t *testing.T) {
		const subject = "never-created-reader"
		reader, err := admin.ForCatalogOwner(subject)
		require.NoError(t, err)
		_, err = reader.Products.List(ctx, nil)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = admin.GetCatalogForOwner(ctx, subject)
		require.ErrorIs(t, err, openrails.ErrNotFound)
	})
	const subjectA = "creator|Alice/雪:%2f"
	const subjectB = "creator:bob"
	alice, err := admin.ForCatalogOwner(subjectA)
	require.NoError(t, err)
	bob, err := admin.ForCatalogOwner(subjectB)
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
	productA, err := alice.Products.Create(ctx, &openrails.ProductCreateParams{Key: "alice-post", DisplayName: "Alice post", CatalogID: (catA.ID).String()})
	require.NoError(t, err)
	productB, err := bob.Products.Create(ctx, &openrails.ProductCreateParams{Key: "bob-post", DisplayName: "Bob post"})
	require.NoError(t, err)
	require.Equal(t, catB.ID.String(), productB.CatalogID)
	priceA, err := alice.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: productA.ID, Key: "alice-usd", UnitAmount: 5_000_000, Currency: "USD"})
	require.NoError(t, err)
	priceB, err := bob.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: productB.ID, Key: "bob-usd", UnitAmount: 7_000_000, Currency: "USD"})
	require.NoError(t, err)

	t.Run("owner reads and writes stay inside the catalog", func(t *testing.T) {
		_, err := alice.Products.Retrieve(ctx, productB.ID)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = alice.Products.RetrieveByKey(ctx, productB.Key)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		title := "Stolen"
		_, err = alice.Products.Update(ctx, productB.ID, &openrails.ProductUpdateParams{DisplayName: &title})
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = alice.Prices.Retrieve(ctx, priceB.ID)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = alice.Prices.RetrieveByKey(ctx, priceB.Key)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = alice.Prices.SetKey(ctx, priceB.ID, "hijacked")
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = alice.Prices.SetKey(ctx, priceA.ID, priceB.Key)
		require.ErrorIs(t, err, openrails.ErrConflict, "an owned price cannot take another catalog's key")
		unchangedPrice, err := bob.Prices.Retrieve(ctx, priceB.ID)
		require.NoError(t, err)
		require.False(t, unchangedPrice.Archived)
		_, err = alice.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: productB.ID, Key: "forged-price", UnitAmount: 1_000_000, Currency: "USD"})
		require.ErrorIs(t, err, openrails.ErrNotFound)
		products, err := alice.Products.List(ctx, &openrails.ProductListParams{})
		require.NoError(t, err)
		require.EqualValues(t, 1, products.Total)
		require.Len(t, products.Items, 1)
		require.Equal(t, productA.ID, products.Items[0].ID)
		prices, err := alice.Prices.List(ctx, &openrails.PriceListParams{})
		require.NoError(t, err)
		require.EqualValues(t, 1, prices.Total)
		require.Len(t, prices.Items, 1)
		require.Equal(t, priceA.ID, prices.Items[0].ID)
		unchanged, err := bob.Products.Retrieve(ctx, productB.ID)
		require.NoError(t, err)
		require.Equal(t, "Bob post", unchanged.DisplayName)
	})

	t.Run("selectors and fields cannot expand creator authority", func(t *testing.T) {
		_, err := alice.Products.Create(ctx, &openrails.ProductCreateParams{Key: "foreign-catalog", DisplayName: "Forged", CatalogID: (catB.ID).String()})
		require.Error(t, err)
		_, err = alice.Products.Create(ctx, &openrails.ProductCreateParams{Key: productB.Key, DisplayName: "Collision"})
		require.ErrorIs(t, err, openrails.ErrConflict)
		_, err = alice.Products.Update(ctx, productA.ID, &openrails.ProductUpdateParams{SetEntitlements: true, EntitlementsSpec: map[string]*int{"platform-wide": nil}})
		require.Error(t, err)
		_, err = alice.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: productA.ID, UnitAmount: 2_000_000, Currency: "USD", PSPs: []string{"stripe"}})
		require.Error(t, err)
		_, err = alice.Prices.Update(ctx, priceA.ID, &openrails.PriceUpdateParams{PSPLinks: map[string]map[string]string{"stripe": {"price_id": "price_forged"}}})
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
		updated, err := admin.Products.Update(ctx, productB.ID, &openrails.ProductUpdateParams{DisplayName: &title})
		require.NoError(t, err)
		require.Equal(t, catB.ID.String(), updated.CatalogID)
		plain, err := admin.Products.Create(ctx, &openrails.ProductCreateParams{Key: "merchant-default", DisplayName: "Default"})
		require.NoError(t, err)
		defaultCatalog, err := admin.GetCatalog(ctx, sdkCatalogID(t, plain.CatalogID))
		require.NoError(t, err)
		require.Nil(t, defaultCatalog.OwnerSubject)
		_, err = alice.Products.Retrieve(ctx, plain.ID)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = owner.Exec(ctx, `UPDATE billing.catalogs SET owner_subject='new-owner' WHERE id=$1`, catA.ID.UUID())
		require.Error(t, err)
		_, err = owner.Exec(ctx, `UPDATE billing.products SET catalog_id=$1 WHERE id=$2`, catB.ID.UUID(), sdkProductID(t, productA.ID).UUID())
		require.Error(t, err)
	})

	t.Run("same host subject in another merchant remains separate", func(t *testing.T) {
		_, _, otherAdmin := newRuntime()
		otherOwner, err := otherAdmin.ForCatalogOwner(subjectA)
		require.NoError(t, err)
		otherCatalog, err := otherOwner.EnsureOwnCatalog(ctx)
		require.NoError(t, err)
		require.NotEqual(t, catA.ID, otherCatalog.ID)
		_, err = otherOwner.Products.Retrieve(ctx, productA.ID)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = otherAdmin.Products.Create(ctx, &openrails.ProductCreateParams{CatalogID: (catA.ID).String(), Key: "cross-merchant", DisplayName: "Forbidden"})
		require.Error(t, err)
	})

	t.Run("remote administrator scope matches embedded scope", func(t *testing.T) {
		handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{Catalog: true}, Gate: creatorAdminTestGate{mid: mid}})
		require.NoError(t, err)
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		remote, err := openrails.NewRemote(server.URL, openrails.WithAPIKey("administrator"), openrails.WithMerchantID(mid))
		require.NoError(t, err)
		scoped, err := remote.ForCatalogOwner(subjectA)
		require.NoError(t, err)
		own, err := scoped.EnsureOwnCatalog(ctx)
		require.NoError(t, err)
		require.Equal(t, catA.ID, own.ID)
		_, err = scoped.Products.Retrieve(ctx, productB.ID)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = scoped.EnsureCatalogForOwner(ctx, subjectB)
		require.ErrorIs(t, err, openrails.ErrDenied)
		_, err = scoped.ForCatalogOwner(subjectB)
		require.ErrorIs(t, err, openrails.ErrDenied)
	})

	t.Run("HTTP identity comes only from the gate", func(t *testing.T) {
		handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{Catalog: true}, Gate: creatorTestGate{mid: mid, subject: subjectA}})
		require.NoError(t, err)
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		remote, err := openrails.NewRemote(server.URL, openrails.WithOwnCatalog(), openrails.WithAPIKey("owner"), openrails.WithMerchantID(mid))
		require.NoError(t, err)
		ownView, err := remote.ForCatalogOwner(subjectA)
		require.NoError(t, err)
		same, err := ownView.EnsureOwnCatalog(ctx)
		require.NoError(t, err)
		require.Equal(t, catA.ID, same.ID)
		forged, err := remote.ForCatalogOwner(subjectB)
		require.NoError(t, err, "selection itself grants no authority")
		_, err = forged.EnsureOwnCatalog(ctx)
		require.ErrorIs(t, err, openrails.ErrDenied, "verified creators cannot select a different owner")
		catalog, err := remote.EnsureOwnCatalog(ctx)
		require.NoError(t, err)
		require.Equal(t, catA.ID, catalog.ID)
		_, err = remote.Products.Retrieve(ctx, productB.ID)
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
		status, _ = creatorRawRequest(t, server.URL, "owner", http.MethodPost, "/v1/catalog/applications", map[string]any{})
		require.Equal(t, http.StatusNotFound, status)
		status, _ = creatorRawRequest(t, server.URL, "owner", http.MethodPatch, "/v1/catalog/products/"+productA.ID, map[string]string{"display_name": "HTTP edit", "owner_subject": subjectB, "catalog_id": catB.ID.String()})
		require.Equal(t, http.StatusBadRequest, status)
		read, err := alice.Products.Retrieve(ctx, productA.ID)
		require.NoError(t, err)
		require.Equal(t, catA.ID.String(), read.CatalogID, "forged fields must never reassign ownership")
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

type creatorAdminTestGate struct{ mid merchant.ID }

func (g creatorAdminTestGate) Authorize(_ context.Context, req *http.Request, permission string) (billingauth.Principal, error) {
	if req.Header.Get("Authorization") != "Bearer administrator" {
		return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "invalid administrator"}
	}
	return billingauth.Principal{MerchantID: g.mid, Subject: "actual-administrator", Permissions: []string{permissions.MerchantAll}}, nil
}
