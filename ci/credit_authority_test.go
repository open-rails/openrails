//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// SEC: minting money is owner authority. A credit grant and the credit-limit
// write need merchant:credits:grant. A support member or support API key edits customers but mints no
// balance and opens no credit line.
func TestSecuritySupportCannotMintCredit(t *testing.T) {
	f := newFixture(t)
	cp := f.newServer(t, reserving())
	ctx := t.Context()
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)

	owner := newAccount(t, cp)
	shop := uniqueName("credit")
	_, err = cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: shop, OwnerUserID: owner.ID})
	require.NoError(t, err)
	mid, _, err := cp.ResolveMerchantForGroup(ctx, shop)
	require.NoError(t, err)
	ownerSession := authtest.SignIn(t, cp.AuthKit(), owner).AccessToken

	support := newAccount(t, cp)
	role, err := cp.AuthKit().Role("merchant:support")
	require.NoError(t, err)
	_, err = cp.AuthKit().SetGroupRole(ctx, iam.SystemIdentity(), iam.GroupByID(mid.String()), iam.UserSubject(support.ID), role)
	require.NoError(t, err)
	supportSession := authtest.SignIn(t, cp.AuthKit(), support).AccessToken

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
		w := call(t, handler, c.token, http.MethodPost, "/v1/merchant/customers/ensure", c.selector, map[string]any{"items": []any{map[string]any{"id": customer}}})
		require.Equal(t, http.StatusOK, w.Code, "%s edits customers: %s", name, w.Body.String())

		w = call(t, handler, c.token, http.MethodPost, "/v1/merchant/customers/"+customer+"/credit-grants", c.selector, map[string]any{
			"invoker": "staff", "currency": "USD", "amount": "1000000000", "source": "manual", "source_id": uuid.NewString(),
		})
		if !owner {
			require.Equal(t, http.StatusForbidden, w.Code, "%s grants credit: %s", name, w.Body.String())
			require.Contains(t, w.Body.String(), "permission_required")
		} else {
			require.Equal(t, http.StatusCreated, w.Code, "%s grants credit: %s", name, w.Body.String())
			w = call(t, handler, c.token, http.MethodGet, "/v1/merchant/customers/"+customer+"/balance?currency=USD", c.selector, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), `"1000000000"`)
		}

		w = call(t, handler, c.token, http.MethodPut, "/v1/merchant/customers/"+customer+"/credit-limit", c.selector, map[string]any{
			"currency": "USD", "amount": "1000000000000",
		})
		if !owner {
			require.Equal(t, http.StatusForbidden, w.Code, "%s opens a credit line: %s", name, w.Body.String())
			require.Contains(t, w.Body.String(), "permission_required")
		} else {
			require.Equal(t, http.StatusOK, w.Code, "%s opens a credit line: %s", name, w.Body.String())
		}
		w = call(t, handler, c.token, http.MethodGet, "/v1/merchant/customers/"+customer+"/credit-limit?currency=USD", c.selector, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		limit := map[string]any{}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&limit))
		require.Equal(t, owner, limit["amount"] == "1000000000000", "%s: %v", name, limit)
	}
}
