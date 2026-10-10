//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	helpersauth "github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// SEC: the standalone server asks AuthKit for a recent sign-in (Sensitive)
// before an owner moves money or grants access, and offers the same check
// (CheckRecentSignIn) to a hosted product's credential changes. A live owner
// session whose sign-in is stale, as a stolen token's is, gets 401
// step_up_required (RFC 9470) with AuthKit's step-up methods; a recent
// sign-in, and the owner's API key, pass. Reads need no step-up.
func TestSecurityOwnerOperationsNeedRecentSignIn(t *testing.T) {
	f := newFixture(t)
	cp := f.newServer(t, reserving())
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	owner := newAccount(t, cp)
	shop := uniqueName("stepup")
	m, err := cp.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: shop, OwnerUserID: owner.ID})
	require.NoError(t, err)
	fresh := authtest.SignIn(t, cp.AuthKit(), owner).AccessToken
	stale := authtest.StaleSession(t, cp.AuthKit(), authtest.SignIn(t, cp.AuthKit(), owner).AccessToken)
	customer := uuid.NewString()

	for _, op := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, "/v1/admin/customers/" + customer, map[string]any{}},
		{http.MethodPost, "/v1/admin/product-access", map[string]any{"items": []any{map[string]any{"customer_id": customer, "product_id": "prod_" + uuid.NewString(), "hours": 24}}}},
		{http.MethodPost, "/v1/admin/payments/" + uuid.NewString() + "/refunds", map[string]any{}},
		{http.MethodPost, "/v1/admin/credit-grants", map[string]any{}},
		{http.MethodPost, "/v1/admin/billing-import", map[string]any{}},
	} {
		w := call(t, handler, stale, op.method, op.path, shop, op.body)
		require.Equal(t, http.StatusUnauthorized, w.Code, "%s %s: %s", op.method, op.path, w.Body.String())
		require.Contains(t, w.Body.String(), `"code":"step_up_required"`, "%s %s", op.method, op.path)
		require.Equal(t, `Bearer error="insufficient_user_authentication", max_age="900"`, w.Header().Get("WWW-Authenticate"))
		require.Contains(t, w.Body.String(), `"step_up_methods":[`, "AuthKit's challenge reaches the client: %s", w.Body.String())

		w = call(t, handler, fresh, op.method, op.path, shop, op.body)
		require.NotContains(t, w.Body.String(), "step_up_required", "%s %s, fresh", op.method, op.path)
	}

	w := call(t, handler, stale, http.MethodGet, "/v1/admin/findings", shop, nil)
	require.Equal(t, http.StatusOK, w.Code, "a read needs no step-up: %s", w.Body.String())

	signedIn := func(token string) error {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return cp.CheckRecentSignIn(t.Context(), r)
	}
	require.ErrorIs(t, signedIn(stale), helpersauth.ErrStepUpRequired)
	require.NoError(t, signedIn(fresh))

	minted, err := cp.CreateMerchantAPIKey(t.Context(), userActor(t, cp, fresh), m.MerchantID, billing.CreateAPIKeyParams{Name: "owner key", Role: "owner"})
	require.NoError(t, err)
	w = call(t, handler, minted.Secret, http.MethodPatch, "/v1/admin/customers/"+uuid.NewString(), "", map[string]any{})
	require.Equal(t, http.StatusOK, w.Code, "an API key carries no sign-in to step up: %s", w.Body.String())
}
