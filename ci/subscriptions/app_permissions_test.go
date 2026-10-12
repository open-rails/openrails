//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
)

// Through a real mount, each programmatic route admits an application
// holding its own permission, the root: name in the root scope or the
// merchant: name at the merchant, and refuses one holding every other, or
// the right name in the wrong scope; a mount without
// RouteGroups.Programmatic serves none of them.
func TestProgrammaticRoutesNeedTheirPermission(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	_, err := w.client[embedded].CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{
		{CustomerID: c.cid(), Currency: "USD", Amount: 10_000_000, Source: "support", SourceID: "seed"},
	})
	require.NoError(t, err)

	application := func(role string) string {
		return w.auth.issue(t, grant{subject: "backend-" + uuid.NewString()[:8], role: role,
			kind: string(openrails.SubjectApplication), credential: string(openrails.CredentialAPIKey)})
	}
	calls := []struct {
		perm         billing.Permission
		method, path string
		body         func() any
		status       int
	}{
		{appEntitlements, http.MethodPost, "/v1/app/entitlements/check", func() any {
			return map[string]any{"customer_id": c.id, "entitlements": []string{"content:any"}}
		}, http.StatusOK},
		{appUsage, http.MethodPost, "/v1/app/admissions/release", func() any { return map[string]any{"request_ids": []string{"req-1"}} }, http.StatusOK},
		{appEvents, http.MethodGet, "/v1/app/host-events", func() any { return nil }, http.StatusOK},
		{appOrders, http.MethodPost, "/v1/app/orders/preview", func() any {
			return map[string]any{"customer_id": c.id, "lines": []any{map[string]any{"price_id": billing.PriceID("test:" + uuid.NewString() + ":v1").String()}}}
		}, http.StatusOK},
	}
	shop := w.client[embedded].MerchantID().String()
	for _, call := range calls {
		status, body := w.merchantJSON(application("only:"+string(call.perm)), call.method, call.path, call.body())
		require.Equal(t, call.status, status, "%s holding %s: %v", call.path, call.perm, body)

		merchant := billing.Permission(billing.MerchantGroupPersona + ":" + call.perm.Resource() + ":" + call.perm.Action())
		require.Contains(t, billing.Permissions(), merchant)
		status, body = w.merchantJSON(application(heldIn(shop, merchant)), call.method, call.path, call.body())
		require.Equal(t, call.status, status, "%s holding %s at the merchant: %v", call.path, merchant, body)

		for name, role := range map[string]string{
			"every permission but its own":                    "but:" + string(call.perm),
			"its merchant: name in the root scope":            "only:" + string(merchant),
			"its merchant: name at another merchant":          heldIn(uuid.NewString(), merchant),
			"its root: name at the merchant":                  heldIn(shop, call.perm),
			"another route's merchant: name, at the merchant": heldIn(shop, billing.MerchantCustomerRead),
		} {
			status, body = w.merchantJSON(application(role), call.method, call.path, call.body())
			require.Equal(t, http.StatusForbidden, status, "%s with %s: %v", call.path, name, body)
			require.Equal(t, billing.CodePermissionRequired, body["error"].(map[string]any)["code"], name)
		}

		status, body = w.merchantJSON(w.auth.token(t, "staff"), call.method, call.path, call.body())
		require.Equal(t, http.StatusForbidden, status, "a person holding %s: %v", call.perm, body)
		require.Equal(t, billing.CodeApplicationRequired, body["error"].(map[string]any)["code"])
	}

	// Provider operations are deleted: neither their application nor their
	// admin routes are served, whatever the caller holds.
	status, raw := w.merchantCall(application(hostApp), http.MethodPost, "/v1/app/provider-operations")
	require.Equal(t, http.StatusNotFound, status, raw)
	status, raw = w.staff(http.MethodGet, "/v1/admin/provider-operations")
	require.Equal(t, http.StatusNotFound, status, raw)

	// The programmatic routes mount with their group, all of them, and none
	// without it.
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, w.rt, openrails.Routes{Auth: w.auth, Prefix: mountPrefix, RouteGroups: openrails.RouteGroups{Admin: true}}))
	staffOnly := httptest.NewServer(mux)
	t.Cleanup(staffOnly.Close)
	for _, call := range calls {
		raw, err := json.Marshal(call.body())
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), call.method, staffOnly.URL+mountPrefix+call.path, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+w.auth.hostToken(t))
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		require.Equal(t, http.StatusNotFound, res.StatusCode, "%s is not mounted without RouteGroups.Programmatic", call.path)
	}
}
