//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// SEC (secaudit round 2, D1): an operation that moves money or grants access
// needs a recent sign-in on every route that serves it. A staff token whose
// sign-in is stale, as a stolen one's is, reaches none of them (merchant
// chosen by header): the import door, the catalog or the merchant API. The same staff signed in recently passes the gate. Reads and the
// host's in-process client need no step-up, and a grant with no end needs its
// own authority.
func TestSecurityStaleSignInReachesNoOwnerOperation(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	price := w.membership("content:members", 9_990_000)
	member := w.newCustomer()
	stale, fresh := w.auth.staleToken(t, "staff"), w.auth.token(t, "staff")
	none := uuid.NewString()
	call := func(token, method, path string, body any) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), method, w.server.URL+mountPrefix+path, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("OpenRails-Merchant", w.slug)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		raw, err = io.ReadAll(res.Body)
		require.NoError(t, err)
		out := map[string]any{}
		_ = json.Unmarshal(raw, &out)
		return res.StatusCode, out
	}
	stepUp := func(body map[string]any) bool {
		e, _ := body["error"].(map[string]any)
		return e["code"] == "step_up_required"
	}

	timed := map[string]any{"entitlement": "content:comp", "hours": 24}
	for _, op := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/merchant/customers/" + member.id + "/entitlements", timed},
		{http.MethodPost, "/v1/merchant/customers/" + member.id + "/product-access", map[string]any{"product_id": price.ProductID}},
		{http.MethodPost, "/v1/merchant/customers/" + member.id + "/credits", map[string]any{}},
		{http.MethodPost, "/v1/merchant/customers/" + member.id + "/payments/off-channel", map[string]any{}},
		{http.MethodPost, "/v1/merchant/payments/" + none + "/refunds", map[string]any{}},
		{http.MethodPost, "/v1/merchant/subscriptions/" + none + "/cancel", map[string]any{}},
		{http.MethodPost, "/v1/import/billing", map[string]any{}},
		{http.MethodPost, "/v1/merchant/catalog/prices", map[string]any{}},
		{http.MethodPatch, "/v1/merchant/catalog/prices/" + price.ID, map[string]any{}},
		{http.MethodPost, "/v1/merchant/catalog/products", map[string]any{}},
		{http.MethodPost, "/v1/merchant/credits/deposit", map[string]any{}},
		{http.MethodPut, "/v1/merchant/credit-limit", map[string]any{}},
	} {
		status, body := call(stale, op.method, op.path, op.body)
		require.Equal(t, http.StatusForbidden, status, "%s %s: %v", op.method, op.path, body)
		require.True(t, stepUp(body), "%s %s: %v", op.method, op.path, body)
		require.Equal(t, []any{"password"}, body["error"].(map[string]any)["metadata"].(map[string]any)["step_up_methods"],
			"the provider's challenge reaches the client: %v", body)

		_, body = call(fresh, op.method, op.path, op.body)
		require.False(t, stepUp(body), "%s %s, fresh: %v", op.method, op.path, body)
	}
	require.True(t, member.entitled("content:comp"), "the fresh grants landed")

	// A read needs no step-up, and neither does the host's in-process client.
	status, body := call(stale, http.MethodGet, "/v1/merchant/customers/"+member.id, nil)
	require.Equal(t, http.StatusOK, status, "%v", body)
	_, err := w.client[embedded].GrantEntitlement(t.Context(), member.id, billing.GrantEntitlementRequest{Entitlement: "content:host"})
	require.NoError(t, err)
	require.True(t, member.entitled("content:host"))

	// A grant with no end needs merchant:access:grant-permanent, which the
	// support member lacks; staff hold it. Hours past time.Duration are refused.
	permanent := map[string]any{"entitlement": "content:forever"}
	support := w.auth.token(t, "support")
	status, body = call(support, http.MethodPost, "/v1/merchant/customers/"+member.id+"/entitlements", timed)
	require.Equal(t, http.StatusCreated, status, "%v", body)
	status, body = call(support, http.MethodPost, "/v1/merchant/customers/"+member.id+"/entitlements", permanent)
	require.Equal(t, http.StatusForbidden, status, "%v", body)
	require.Equal(t, "permanent_grant_forbidden", body["error"].(map[string]any)["code"])
	status, body = call(support, http.MethodPost, "/v1/merchant/customers/"+member.id+"/product-access", map[string]any{"product_id": price.ProductID})
	require.Equal(t, http.StatusForbidden, status, "%v", body)
	require.Equal(t, "permanent_grant_forbidden", body["error"].(map[string]any)["code"])
	require.False(t, member.entitled("content:forever"))
	status, body = call(fresh, http.MethodPost, "/v1/merchant/customers/"+member.id+"/entitlements", map[string]any{"entitlement": "content:comp", "hours": 2562048})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	status, body = call(fresh, http.MethodPost, "/v1/merchant/customers/"+member.id+"/entitlements", permanent)
	require.Equal(t, http.StatusCreated, status, "%v", body)
	require.True(t, member.entitled("content:forever"))
}
