//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// SEC (secaudit round 2, D1): the standalone control plane asks AuthKit for
// a recent sign-in (Sensitive) before an owner moves money, grants access or
// mints credentials. A live owner session whose sign-in is stale, as a stolen
// token's is, gets 403 step_up_required with AuthKit's step-up methods; the
// same owner signed in recently, and the owner's API key, pass. Reads need no
// step-up.
func TestSecurityOwnerOperationsNeedRecentSignIn(t *testing.T) {
	f := newFixture(t)
	cp := f.attachControlPlane(t, reserving())
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	owner := newAccount(t, cp)
	shop := uniqueName("stepup")
	_, err = cp.ProvisionMerchant(t.Context(), billing.ProvisionMerchantRequest{Slug: shop, OwnerUserID: owner.ID})
	require.NoError(t, err)
	fresh := authtest.SignIn(t, cp.AuthKit(), owner).AccessToken
	stale := authtest.StaleSession(t, cp.AuthKit(), authtest.SignIn(t, cp.AuthKit(), owner).AccessToken)
	customer := uuid.NewString()

	for _, op := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/v1/merchant/customers/" + customer, map[string]any{}},
		{http.MethodPost, "/v1/merchant/customers/" + customer + "/entitlements", map[string]any{"entitlement": "content:comp", "hours": 24}},
		{http.MethodPost, "/v1/merchant/payments/" + uuid.NewString() + "/refunds", map[string]any{}},
		{http.MethodPost, "/v1/merchant/customers/" + customer + "/credit-grants", map[string]any{}},
		{http.MethodPost, "/v1/merchant/api-keys", map[string]string{"name": "ci", "role": "viewer"}},
		{http.MethodPost, "/v1/import/billing", map[string]any{}},
	} {
		w := call(t, handler, stale, op.method, op.path, shop, op.body)
		require.Equal(t, http.StatusForbidden, w.Code, "%s %s: %s", op.method, op.path, w.Body.String())
		require.Contains(t, w.Body.String(), `"code":"step_up_required"`, "%s %s", op.method, op.path)
		require.Contains(t, w.Body.String(), `"step_up_methods":[`, "AuthKit's challenge reaches the client: %s", w.Body.String())

		w = call(t, handler, fresh, op.method, op.path, shop, op.body)
		require.NotContains(t, w.Body.String(), "step_up_required", "%s %s, fresh", op.method, op.path)
	}

	w := call(t, handler, stale, http.MethodGet, "/v1/merchant/team", shop, nil)
	require.Equal(t, http.StatusOK, w.Code, "a read needs no step-up: %s", w.Body.String())

	w = call(t, handler, fresh, http.MethodPost, "/v1/merchant/api-keys", shop, map[string]string{"name": "owner key", "role": "owner"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var minted struct{ Secret string }
	require.NoError(t, json.NewDecoder(w.Body).Decode(&minted))
	key := minted.Secret
	w = call(t, handler, key, http.MethodPut, "/v1/merchant/customers/"+uuid.NewString(), "", map[string]any{})
	require.Equal(t, http.StatusOK, w.Code, "an API key carries no sign-in to step up: %s", w.Body.String())
}
