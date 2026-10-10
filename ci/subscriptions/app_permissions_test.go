//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
)

// Through a real mount, each programmatic route admits an application
// holding its own permission and refuses one holding every other; a mount
// given only some of the programmatic permissions serves only their routes;
// and Mount refuses a programmatic permission with RouteGroups.Programmatic
// off.
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
	openOperation := func() map[string]any {
		id := "op-" + uuid.NewString()[:8]
		body := []byte(`{"rental":"` + id + `"}`)
		digest := sha256.Sum256(body)
		return map[string]any{
			"operation_id": id, "customer_id": c.id, "record_owner": "user:1", "currency": "USD", "amount": "1000000",
			"claim_reference": "claim:" + id, "authorization_body": base64.StdEncoding.EncodeToString(body),
			"authorization_body_sha256": hex.EncodeToString(digest[:]),
		}
	}
	calls := []struct {
		perm         perm
		method, path string
		body         func() any
		status       int
	}{
		{appEntitlements, http.MethodPost, "/v1/app/entitlements/check", func() any {
			return map[string]any{"customer_id": c.id, "entitlements": []string{"content:any"}}
		}, http.StatusOK},
		{appUsage, http.MethodPost, "/v1/app/admissions/release", func() any { return map[string]any{"request_ids": []string{"req-1"}} }, http.StatusOK},
		{appCosts, http.MethodPost, "/v1/app/provider-operations", func() any { return openOperation() }, http.StatusCreated},
		{appEvents, http.MethodGet, "/v1/app/host-events", func() any { return nil }, http.StatusOK},
	}
	for _, call := range calls {
		status, body := w.merchantJSON(application("only:"+string(call.perm)), call.method, call.path, call.body())
		require.Equal(t, call.status, status, "%s holding %s: %v", call.path, call.perm, body)

		status, body = w.merchantJSON(application("but:"+string(call.perm)), call.method, call.path, call.body())
		require.Equal(t, http.StatusForbidden, status, "%s with every permission but %s: %v", call.path, call.perm, body)
		require.Equal(t, billing.CodePermissionRequired, body["error"].(map[string]any)["code"])

		status, body = w.merchantJSON(w.auth.token(t, "staff"), call.method, call.path, call.body())
		require.Equal(t, http.StatusForbidden, status, "a person holding %s: %v", call.perm, body)
		require.Equal(t, billing.CodeApplicationRequired, body["error"].(map[string]any)["code"])
	}

	// A mount given only Entitlements serves the content gate and no other
	// programmatic route.
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, w.rt, openrails.Routes{
		Auth: w.auth, Scope: staffScope, Prefix: mountPrefix,
		RouteGroups: openrails.RouteGroups{Programmatic: true}, Permissions: openrails.Permissions{Entitlements: appEntitlements},
	}))
	gate := httptest.NewServer(mux)
	t.Cleanup(gate.Close)
	for _, call := range calls {
		if call.perm == appEntitlements {
			status, body := w.merchantJSONAt(gate.URL, w.auth.hostToken(t), call.method, call.path, call.body())
			require.Equal(t, call.status, status, "%s on a mount given Entitlements: %v", call.path, body)
			continue
		}
		raw, err := json.Marshal(call.body())
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), call.method, gate.URL+mountPrefix+call.path, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+w.auth.hostToken(t))
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		require.Equal(t, http.StatusNotFound, res.StatusCode, "%s is not mounted without %s", call.path, call.perm)
	}

	for name, routes := range map[string]openrails.Routes{
		"Usage, programmatic off": {Auth: w.auth, Scope: staffScope, Permissions: openrails.Permissions{Usage: appUsage}},
		"Events, staff on":        {Auth: w.auth, Scope: staffScope, RouteGroups: openrails.RouteGroups{Admin: true}, Permissions: openrails.Permissions{AdminRead: staffReads, Events: appEvents}},
		"Costs without Scope":     {Auth: w.auth, RouteGroups: openrails.RouteGroups{Programmatic: true}, Permissions: openrails.Permissions{Costs: appCosts}},
	} {
		err := openrailshttp.Mount(http.NewServeMux(), w.rt, routes)
		require.Error(t, err, name)
		require.True(t, slices.ContainsFunc([]string{"RouteGroups.Programmatic is off", "without Routes.Scope"}, func(s string) bool { return strings.Contains(err.Error(), s) }), "%s: %v", name, err)
	}
}
