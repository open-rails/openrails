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

// SEC: minting money is a staff write on the standalone server: a support
// member or API key, holding merchant:billing:write, grants credit and opens a
// credit line as an owner does; a viewer key, holding only the reads, does
// neither and edits no customer.
func TestSecurityOnlyStaffWritesMintCredit(t *testing.T) {
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
		key, err := cp.CreateMerchantAPIKey(ctx, userActor(t, cp, ownerSession), mid, billing.CreateAPIKeyParams{Name: role + " key", Role: role})
		require.NoError(t, err)
		return key.Secret
	}
	type caller struct{ token, selector string }
	callers := map[string]caller{
		"support member":  {supportSession, shop},
		"support API key": {apiKey("support"), ""},
		"owner member":    {ownerSession, shop},
		"owner API key":   {apiKey("owner"), ""},
		"viewer API key":  {apiKey("viewer"), ""},
	}

	for name, c := range callers {
		writer := name != "viewer API key"
		customer := uuid.NewString()
		w := call(t, handler, ownerSession, http.MethodPatch, "/v1/admin/customers/"+customer, shop, map[string]any{})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		w = call(t, handler, c.token, http.MethodPatch, "/v1/admin/customers/"+customer, c.selector, map[string]any{})
		require.Equal(t, writer, w.Code == http.StatusOK, "%s edits customers: %s", name, w.Body.String())

		w = call(t, handler, c.token, http.MethodPost, "/v1/admin/credit-grants", c.selector, map[string]any{"items": []any{map[string]any{
			"customer_id": customer, "invoker": "staff", "currency": "USD", "amount": "1000000000", "source": "manual", "source_id": uuid.NewString(),
		}}})
		if !writer {
			require.Equal(t, http.StatusForbidden, w.Code, "%s grants credit: %s", name, w.Body.String())
			require.Contains(t, w.Body.String(), "permission_required")
		} else {
			require.Equal(t, http.StatusCreated, w.Code, "%s grants credit: %s", name, w.Body.String())
			w = call(t, handler, c.token, http.MethodGet, "/v1/admin/customers/"+customer+"/balance?currency=USD", c.selector, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), `"1000000000"`)
		}

		w = call(t, handler, c.token, http.MethodPatch, "/v1/admin/customers/"+customer, c.selector, map[string]any{
			"credit_limits": []any{map[string]any{"currency": "USD", "amount": "1000000000000"}},
		})
		if !writer {
			require.Equal(t, http.StatusForbidden, w.Code, "%s opens a credit line: %s", name, w.Body.String())
			require.Contains(t, w.Body.String(), "permission_required")
		} else {
			require.Equal(t, http.StatusOK, w.Code, "%s opens a credit line: %s", name, w.Body.String())
		}
		w = call(t, handler, c.token, http.MethodGet, "/v1/admin/customers/"+customer, c.selector, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var read billing.Customer
		require.NoError(t, json.NewDecoder(w.Body).Decode(&read))
		limits := read.Settings.CreditLimits
		require.Equal(t, writer, len(limits) == 1 && limits[0].Amount == 1_000_000_000_000, "%s: %v", name, limits)
	}
}
