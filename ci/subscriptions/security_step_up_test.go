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

// Admin changes pass on the staff member's permission, regardless of the
// age of the sign-in. Customer and network policies are separate.
func TestSecurityAdminOperationsNeedNoRecentSignIn(t *testing.T) {
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

	comp, forever, host := w.giftProduct("content:comp"), w.giftProduct("content:forever"), w.giftProduct("content:host")
	grant := func(product billing.ProductID, hours any) map[string]any {
		item := map[string]any{"customer_id": member.id, "product_id": product}
		if hours != nil {
			item["hours"] = hours
		}
		return map[string]any{"items": []any{item}}
	}
	timed := grant(comp, 24)
	for _, op := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/admin/product-access", timed},
		{http.MethodPost, "/v1/admin/credit-grants", map[string]any{}},
		{http.MethodPost, "/v1/admin/payments", map[string]any{}},
		{http.MethodPost, "/v1/admin/payments/" + none + "/refunds", map[string]any{}},
		{http.MethodPost, "/v1/admin/subscriptions/" + none + "/cancel", map[string]any{}},
		{http.MethodPost, "/v1/merchant/billing-import", map[string]any{}},
		{http.MethodPost, "/v1/catalog/prices", map[string]any{}},
		{http.MethodPatch, "/v1/catalog/prices/" + price.ID.String(), map[string]any{}},
		{http.MethodPost, "/v1/catalog/products", map[string]any{}},
		{http.MethodPatch, "/v1/admin/customers/" + member.id, map[string]any{"items": []any{map[string]any{"customer_id": member.id, "credit_limits": []any{map[string]any{"currency": "USD", "amount": "1"}}}}}},
		{http.MethodPatch, "/v1/admin/customers/" + member.id, map[string]any{"items": []any{map[string]any{"customer_id": member.id, "trust_levels": []any{map[string]any{"currency": "USD", "trust_level": "gold"}}}}}},
	} {
		status, body := call(stale, op.method, op.path, op.body)
		require.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError}, status, "%s %s: %v", op.method, op.path, body)
		require.False(t, stepUp(body), "%s %s: %v", op.method, op.path, body)

		_, body = call(fresh, op.method, op.path, op.body)
		require.False(t, stepUp(body), "%s %s, fresh: %v", op.method, op.path, body)
	}
	require.True(t, member.entitled("content:comp"), "staff grants land without a recent sign-in")

	// A read needs no step-up, and neither does the host's in-process client.
	status, body := call(stale, http.MethodGet, "/v1/admin/customers?ids="+member.id, nil)
	require.Equal(t, http.StatusOK, status, "%v", body)
	member.grant(host, nil, nil)
	require.True(t, member.entitled("content:host"))

	// A grant with no end is a staff write like any other: the support member
	// holds it. Hours past time.Duration are refused.
	permanent := grant(forever, nil)
	support := w.auth.token(t, "support")
	status, body = call(support, http.MethodPost, "/v1/admin/product-access", timed)
	require.Equal(t, http.StatusCreated, status, "%v", body)
	status, body = call(fresh, http.MethodPost, "/v1/admin/product-access", grant(comp, 2562048))
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	status, body = call(support, http.MethodPost, "/v1/admin/product-access", permanent)
	require.Equal(t, http.StatusCreated, status, "%v", body)
	require.True(t, member.entitled("content:forever"))
}
