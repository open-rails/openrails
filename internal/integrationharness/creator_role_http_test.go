//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/permissions"
	"github.com/stretchr/testify/require"
)

// This deliberately boots the real AuthKit-backed control plane. A string-set
// permission test cannot detect a role that AuthKit rejects at construction.
func TestCreatorRoleThroughRealAuthKitAndHTTP(t *testing.T) {
	ctx := t.Context()
	h := New(t, ctx)
	surface := h.StartStandalone("USD")
	cp := embcp.Get(surface.App())
	require.NotNil(t, cp)
	core := cp.Core()
	require.NotNil(t, core)
	owned := surface.ProvisionOwnedMerchant("creator-role-" + uuid.NewString()[:8])
	userID, email := makeUser(t, core, "creator"+uuid.NewString()[:8])
	token, _, err := core.MintAccessToken(ctx, userID, nil)
	require.NoError(t, err)
	credential := openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil })
	creator := surface.Client(credential, openrails.WithMerchantID(owned.MerchantID), openrails.WithOwnCatalog())
	_, err = creator.EnsureOwnCatalog(ctx)
	require.ErrorIs(t, err, openrails.ErrDenied, "authentication alone must not assign the creator role")

	// Assign the declared role through the actual owner-authorized team API,
	// not a custom Gate or an in-memory permission substitute.
	status, body := inviteTeamHTTP(t, surface.BaseURL, owned.APIKey, email, controlplane.MerchantRoleCreator)
	require.Equal(t, http.StatusCreated, status, string(body))
	for _, permission := range []string{permissions.MerchantCatalogOwnRead, permissions.MerchantCatalogOwnUpdate} {
		allowed, err := core.Can(ctx, authkit.UserSubject(userID), controlplane.MerchantGroup(owned.MerchantSlug), authkit.Perm(permission))
		require.NoError(t, err)
		require.True(t, allowed, permission)
	}
	for _, permission := range []string{permissions.MerchantCatalogRead, permissions.MerchantCatalogUpdate, permissions.MerchantPaymentProvidersUpdate, permissions.MerchantPaymentsRefund, permissions.MerchantSettingsUpdate} {
		allowed, err := core.Can(ctx, authkit.UserSubject(userID), controlplane.MerchantGroup(owned.MerchantSlug), authkit.Perm(permission))
		require.NoError(t, err)
		require.False(t, allowed, permission)
	}

	catalog, err := creator.EnsureOwnCatalog(ctx)
	require.NoError(t, err, "the unchanged real user token must observe its new live creator grant")
	require.NotNil(t, catalog.OwnerSubject)
	require.Equal(t, userID, *catalog.OwnerSubject, "Gate identity must come from the verified AuthKit user")
	require.Equal(t, owned.MerchantID, catalog.MerchantID)
	product, err := creator.Products.Create(ctx, &openrails.ProductCreateParams{Key: "creator-product", DisplayName: "Creator product"})
	require.NoError(t, err)
	require.Equal(t, catalog.ID.String(), product.CatalogID)
	_, err = creator.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, Key: "creator-usd", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	title := "Creator update"
	updated, err := creator.Products.Update(ctx, product.ID, &openrails.ProductUpdateParams{DisplayName: &title})
	require.NoError(t, err)
	require.Equal(t, title, updated.DisplayName)
	products, err := creator.Products.List(ctx, &openrails.ProductListParams{})
	require.NoError(t, err)
	require.EqualValues(t, 1, products.Total)
	require.Equal(t, product.ID, products.Items[0].ID)

	admin := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
	merchantProduct, err := admin.Products.Create(ctx, &openrails.ProductCreateParams{Key: "merchant-product", DisplayName: "Merchant product"})
	require.NoError(t, err)
	_, err = creator.Products.Retrieve(ctx, merchantProduct.ID)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	// Choosing the administrator path in the SDK never upgrades this token.
	wide := surface.Client(credential, openrails.WithMerchantID(owned.MerchantID))
	_, err = wide.Products.Create(ctx, &openrails.ProductCreateParams{Key: "forbidden-wide-create", DisplayName: "Forbidden"})
	require.ErrorIs(t, err, openrails.ErrDenied)
	_, err = wide.Products.List(ctx, &openrails.ProductListParams{})
	require.ErrorIs(t, err, openrails.ErrDenied)
	_, err = wide.ListCatalogs(ctx, openrails.PageOptions{})
	require.ErrorIs(t, err, openrails.ErrDenied)
	_, err = wide.GetMerchantSettings(ctx)
	require.ErrorIs(t, err, openrails.ErrDenied)
	status, body = requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/api-keys", token, map[string]string{"name": "escalation", "role": "owner"})
	require.Equal(t, http.StatusForbidden, status, string(body))
	status, body = requestJSON(t, http.MethodPut, surface.BaseURL+"/v1/merchant/payment-providers/stripe", token, map[string]any{})
	require.Equal(t, http.StatusForbidden, status, string(body))

	status, body = requestJSON(t, http.MethodDelete, surface.BaseURL+"/v1/merchant/team/"+userID, owned.APIKey, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	_, err = creator.Products.Retrieve(ctx, product.ID)
	require.ErrorIs(t, err, openrails.ErrDenied, "revoking the role must remove own-catalog authority from the same token")
}
