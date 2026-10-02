//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/embed/controlplane"
)

// SEC: minting money is owner authority. The machine credit deposit and the
// credit-limit write need merchant:credits:grant, as the human credit grant
// does. A support member or support API key edits customers but mints no
// balance and opens no credit line.
func TestSecuritySupportCannotMintCredit(t *testing.T) {
	f := newFixture(t)
	cp := f.attachControlPlane(t, reserving())
	ctx := t.Context()
	handler, err := cp.Handler()
	require.NoError(t, err)

	owner := newAccount(t, cp)
	shop := uniqueName("credit")
	_, err = cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: shop, OwnerUserID: owner.ID})
	require.NoError(t, err)
	mid, _, err := cp.ResolveMerchantForGroup(ctx, shop)
	require.NoError(t, err)
	ownerSession := authtest.SignIn(t, cp.Core(), owner).AccessToken

	support := newAccount(t, cp)
	role, err := cp.Core().Role("merchant:support")
	require.NoError(t, err)
	_, err = cp.Core().SetGroupRole(ctx, iam.SystemActor(), iam.GroupByID(mid.String()), iam.UserSubject(support.ID), role)
	require.NoError(t, err)
	supportSession := authtest.SignIn(t, cp.Core(), support).AccessToken

	apiKey := func(role string) string {
		w := call(t, handler, ownerSession, http.MethodPost, "/v1/merchant/api-keys", shop, map[string]string{"name": role + " key", "role": role})
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		out := map[string]any{}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&out))
		return out["secret"].(string)
	}
	type caller struct{ token, selector string }
	callers := map[string]caller{
		"support member":  {supportSession, shop},
		"support API key": {apiKey("support"), ""},
		"owner member":    {ownerSession, shop},
		"owner API key":   {apiKey("owner"), ""},
	}

	for name, c := range callers {
		owner := name == "owner member" || name == "owner API key"
		customer := uuid.NewString()
		w := call(t, handler, c.token, http.MethodPut, "/v1/merchant/customers/"+customer, c.selector, nil)
		require.Equal(t, http.StatusOK, w.Code, "%s edits customers: %s", name, w.Body.String())

		w = call(t, handler, c.token, http.MethodPost, "/v1/merchant/credits/deposit", c.selector, map[string]any{
			"customer_id": customer, "invoker": "staff", "currency": "USD",
			"amount": "1000000000", "source": "manual", "source_id": uuid.NewString(),
		})
		if !owner {
			require.Equal(t, http.StatusForbidden, w.Code, "%s deposits credit: %s", name, w.Body.String())
			require.Contains(t, w.Body.String(), "permission_required")
		} else {
			require.Equal(t, http.StatusOK, w.Code, "%s deposits credit: %s", name, w.Body.String())
			w = call(t, handler, c.token, http.MethodGet, "/v1/merchant/credits/balance?currency=USD&customer_id="+customer, c.selector, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), `"1000000000"`)
		}

		w = call(t, handler, c.token, http.MethodPut, "/v1/merchant/credit-limit", c.selector, map[string]any{
			"customer_id": customer, "currency": "USD", "credit_limit_amount": "1000000000000",
		})
		if !owner {
			require.Equal(t, http.StatusForbidden, w.Code, "%s opens a credit line: %s", name, w.Body.String())
			require.Contains(t, w.Body.String(), "permission_required")
		} else {
			require.Equal(t, http.StatusOK, w.Code, "%s opens a credit line: %s", name, w.Body.String())
		}
		w = call(t, handler, c.token, http.MethodGet, "/v1/merchant/credit-limit?currency=USD&customer_id="+customer, c.selector, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		limit := map[string]any{}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&limit))
		require.Equal(t, owner, limit["credit_limit_amount"] == "1000000000000", "%s: %v", name, limit)
	}
}
