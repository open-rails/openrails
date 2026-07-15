//go:build integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprouter "github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func TestCustomerTreasurySpendDelegationsHTTPFullReplacement(t *testing.T) {
	suite := setupTestSuite(t)

	router := http.NewServeMux()
	httproutes.RegisterCustomerTreasuryRoutes(httprouter.NewMux(router, "/v1/customers", suite.App.Runtime), suite.App.Runtime,
		middleware.DelegatedPrincipalRequired(hostSeamAuthenticator{
			subject: uuid.NewString(),
			perms: []string{
				controlplane.PermCustomerSpendDelegationsRead,
				controlplane.PermCustomerSpendDelegationsUpdate,
			},
		}), routesurface.AllProviderRoutes())

	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	invokerKey := uuid.NewString()
	roleKey := uuid.NewString()
	trustLevelKey := "trust_1"

	firstDoc := map[string]any{"delegations": []map[string]any{
		{
			"scope":     "invoker",
			"scope_key": invokerKey,
			"windows": []map[string]any{
				{"key": "day", "window_seconds": 86400, "limit": 1200, "currency": "USD"},
			},
		},
		{
			"scope":     "role",
			"scope_key": roleKey,
			"windows": []map[string]any{
				{"key": "week", "window_seconds": 604800, "limit": 9000, "currency": "USD"},
			},
		},
		{
			"scope":     "invoker_tier",
			"scope_key": trustLevelKey,
			"windows": []map[string]any{
				{"key": "month", "window_seconds": 2592000, "limit": 15000, "currency": "USD"},
			},
		},
	}}
	resp := requestCustomerTreasuryJSON(t, srv, http.MethodPut, "/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations", firstDoc)
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	resp = requestCustomerTreasuryJSON(t, srv, http.MethodGet, "/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations", nil)
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	require.Len(t, decodeDelegations(t, resp.body), 3)

	resp = requestCustomerTreasuryJSON(t, srv, http.MethodPut, "/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations:upsert", map[string]any{
		"scope":     "invoker",
		"scope_key": invokerKey,
		"windows": []map[string]any{
			{"key": "day", "window_seconds": 86400, "limit": 321, "currency": "USD"},
		},
	})
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)
	payer := identity.CustomerID(dbtest.TestMerchantID.UUID())
	stored, err := svc.InvokerSpendLimits(dbtest.WithTestMerchant(context.Background()), payer)
	require.NoError(t, err)
	require.Len(t, stored, 3, "single upsert must preserve unrelated role and invoker-tier rows")
	limits := map[string]int64{}
	for _, row := range stored {
		limits[row.Scope+"\x00"+row.ScopeKey] = row.Windows[0].Limit
	}
	require.EqualValues(t, 321, limits["invoker\x00"+invokerKey])
	require.EqualValues(t, 9000, limits["role\x00"+roleKey])
	require.EqualValues(t, 15000, limits["invoker_tier\x00"+trustLevelKey])

	secondDoc := map[string]any{"delegations": []map[string]any{
		{
			"scope":     "role",
			"scope_key": roleKey,
			"windows": []map[string]any{
				{"key": "day", "window_seconds": 86400, "limit": 500, "currency": "USD"},
			},
		},
	}}
	resp = requestCustomerTreasuryJSON(t, srv, http.MethodPut, "/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations", secondDoc)
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	stored, err = svc.InvokerSpendLimits(dbtest.WithTestMerchant(context.Background()), payer)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, "role", stored[0].Scope)
	require.Equal(t, roleKey, stored[0].ScopeKey)
	require.Len(t, stored[0].Windows, 1)
	require.EqualValues(t, 500, stored[0].Windows[0].Limit)

	resp = requestCustomerTreasuryJSON(t, srv, http.MethodPut, "/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations",
		map[string]any{"customer_id": uuid.NewString(), "delegations": []map[string]any{}})
	require.Equal(t, http.StatusBadRequest, resp.status, resp.body)
}

type customerTreasuryResponse struct {
	status int
	body   string
}

func requestCustomerTreasuryJSON(t *testing.T, srv *httptest.Server, method, path string, body any) customerTreasuryResponse {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer host-credential")
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return customerTreasuryResponse{status: resp.StatusCode, body: string(data)}
}

func decodeDelegations(t *testing.T, body string) []any {
	t.Helper()
	var doc struct {
		Delegations []any `json:"delegations"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &doc), body)
	return doc.Delegations
}
