//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
)

// A merchant composes least-privilege roles for its programs: the owner
// defines one in the merchant's AuthKit group through the routes the console
// calls, gives it to an API key and a teammate, and every check reads it
// live. An edit applies on the next request; deleting it revokes the key.
func TestMerchantCustomRoles(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, resourced)
	ctx := t.Context()
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	ownerID, owner := newOwner(t, srv)
	p, err := srv.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName("roles"), OwnerUserID: ownerID})
	require.NoError(t, err)
	mid := p.MerchantID
	bound := f.exchanged(t, handler, owner, mid)
	groups := "/" + f.schema + "/v1/groups/" + mid.String()
	decode := func(w *httptest.ResponseRecorder, status int, out any) {
		t.Helper()
		require.Equal(t, status, w.Code, w.Body.String())
		if out != nil {
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), out))
		}
	}

	// The console finds the merchant's group in the access read.
	var access billing.AdminAccess
	decode(bound.call(t, handler, http.MethodGet, "/v1/admin/access", nil), http.StatusOK, &access)
	require.Equal(t, mid.String(), access.ScopeID)

	var created iam.GroupRole
	decode(call(t, handler, owner, http.MethodPost, groups+"/roles", map[string]any{
		"name": "storefront", "permissions": []string{string(billing.MerchantCatalogRead), string(billing.MerchantEntitlementsRead)},
	}), http.StatusCreated, &created)
	storefront := created.Name.String()
	require.Equal(t, "merchant:custom-storefront", storefront)
	require.True(t, created.Custom)
	rolePath := groups + "/roles/" + url.PathEscape(storefront)

	// Declared roles, then the merchant's; the owner's lists every
	// permission a role may hold, which the console offers.
	var roles struct{ Data []iam.GroupRole }
	decode(call(t, handler, owner, http.MethodGet, groups+"/roles", nil), http.StatusOK, &roles)
	byName := map[string]iam.GroupRole{}
	for _, r := range roles.Data {
		byName[r.Name.String()] = r
	}
	require.Contains(t, byName, storefront)
	var catalog []string
	for _, perm := range byName["merchant:owner"].Permissions {
		catalog = append(catalog, perm.String())
	}
	require.Subset(t, catalog, []string{
		string(billing.MerchantBillingRead), string(billing.MerchantBillingManage), string(billing.MerchantCatalogManage), string(billing.MerchantConfigManage), string(billing.MerchantMetricsRead),
		string(billing.MerchantEntitlementsRead), string(billing.MerchantCatalogRead), string(billing.MerchantUsageManage), string(billing.MerchantCostsManage), string(billing.MerchantEventsRead),
		string(billing.MerchantOrdersManage), server.MerchantRolesRead, server.MerchantRolesManage,
	})

	var minted iam.APIKeyCreated
	decode(call(t, handler, owner, http.MethodPost, groups+"/api-keys", map[string]any{"name": "storefront", "role": storefront}), http.StatusCreated, &minted)
	key := minted.Secret
	customer := billing.CustomerID(uuid.New()).String()
	products := func(token string) *httptest.ResponseRecorder {
		return call(t, handler, token, http.MethodGet, "/v1/app/catalog/products?entitlement=content:any", nil)
	}
	entitlements := func(token string) *httptest.ResponseRecorder {
		return call(t, handler, token, http.MethodPost, "/v1/app/entitlements/check", map[string]any{"customer_id": customer, "entitlements": []string{"content:any"}})
	}
	usage := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/app/usage-events", strings.NewReader(`{"items":[]}`))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", uuid.NewString())
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	refused := func(w *httptest.ResponseRecorder, what string) {
		t.Helper()
		require.Equal(t, http.StatusForbidden, w.Code, "%s: %s", what, w.Body.String())
		require.Equal(t, billing.CodePermissionRequired, errorCode(t, w), what)
	}
	decode(products(key), http.StatusOK, nil)
	decode(entitlements(key), http.StatusOK, nil)
	refused(usage(key), "the storefront role holds no usage permission")

	// A hosted product hands out the same role through the server's Go API.
	r, ok := server.MerchantRole("custom-storefront")
	require.True(t, ok)
	require.Equal(t, storefront, r.String())
	goKey, err := srv.AuthKit().CreateAPIKey(ctx, iam.SystemIdentity(), iam.GroupByID(mid.String()), iam.NewAPIKey{Name: "storefront-go", Role: r})
	require.NoError(t, err)
	decode(products(goKey.Secret), http.StatusOK, nil)

	// A teammate holds it too; their merchant list says what it grants.
	teammate := newAccount(t, srv)
	decode(call(t, handler, owner, http.MethodPut, groups+"/members/users/"+teammate.ID, map[string]any{"role": storefront}), http.StatusOK, nil)
	listed, err := srv.ListUserMerchants(ctx, teammate.ID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "custom-storefront", listed[0].Role)
	require.Equal(t, []string{string(billing.MerchantCatalogRead), string(billing.MerchantEntitlementsRead)}, listed[0].Permissions)

	// Only the owner manages roles: a viewer neither reads nor changes them.
	viewer := newAccount(t, srv)
	decode(call(t, handler, owner, http.MethodPut, groups+"/members/users/"+viewer.ID, map[string]any{"role": "merchant:viewer"}), http.StatusOK, nil)
	viewerToken := authtest.SignIn(t, srv.AuthKit(), viewer).AccessToken
	decode(call(t, handler, viewerToken, http.MethodGet, groups+"/roles", nil), http.StatusForbidden, nil)
	decode(call(t, handler, viewerToken, http.MethodPatch, rolePath, map[string]any{"permissions": []string{string(billing.MerchantUsageManage)}}), http.StatusForbidden, nil)

	// An edit applies on the next request, to every holder.
	decode(call(t, handler, owner, http.MethodPatch, rolePath, map[string]any{
		"permissions": []string{string(billing.MerchantCatalogRead), string(billing.MerchantUsageManage)},
	}), http.StatusOK, nil)
	decode(products(key), http.StatusOK, nil)
	refused(entitlements(key), "the edit took entitlements away")
	w := usage(key)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Equal(t, "invalid_param", errorCode(t, w), "past the gate: the handler's own answer to an empty batch")
	listed, err = srv.ListUserMerchants(ctx, teammate.ID)
	require.NoError(t, err)
	require.Equal(t, []string{string(billing.MerchantCatalogRead), string(billing.MerchantUsageManage)}, listed[0].Permissions)

	// Deleting it revokes the keys carrying it and takes it from the team.
	decode(call(t, handler, owner, http.MethodDelete, rolePath, nil), http.StatusNoContent, nil)
	for _, secret := range []string{key, goKey.Secret} {
		require.Equal(t, http.StatusUnauthorized, products(secret).Code, "a deleted role's key authenticates nothing")
	}
	listed, err = srv.ListUserMerchants(ctx, teammate.ID)
	require.NoError(t, err)
	require.Empty(t, listed)
	decode(call(t, handler, owner, http.MethodGet, rolePath, nil), http.StatusNotFound, nil)
}
