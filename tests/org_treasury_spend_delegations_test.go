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

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	ginroutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func TestOrgTreasurySpendDelegationsHTTPFullReplacement(t *testing.T) {
	suite := setupTestSuite(t)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	ginroutes.RegisterOrgTreasuryRoutes(router.Group("/v1/orgs"), suite.App.Runtime,
		ginmw.DelegatedPrincipalRequired(hostSeamAuthenticator{
			subject: uuid.NewString(),
			perms: []string{
				controlplane.PermCustomerSpendDelegationsRead,
				controlplane.PermCustomerSpendDelegationsUpdate,
			},
		}))

	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	invokerKey := uuid.NewString()
	roleKey := uuid.NewString()

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
	}}
	resp := requestOrgTreasuryJSON(t, srv, http.MethodPut, "/v1/orgs/"+dbtest.TestMerchantSlug+"/spend-delegations", firstDoc)
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	resp = requestOrgTreasuryJSON(t, srv, http.MethodGet, "/v1/orgs/"+dbtest.TestMerchantSlug+"/spend-delegations", nil)
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	require.Len(t, decodeDelegations(t, resp.body), 2)

	secondDoc := map[string]any{"delegations": []map[string]any{
		{
			"scope":     "role",
			"scope_key": roleKey,
			"windows": []map[string]any{
				{"key": "day", "window_seconds": 86400, "limit": 500, "currency": "USD"},
			},
		},
	}}
	resp = requestOrgTreasuryJSON(t, srv, http.MethodPut, "/v1/orgs/"+dbtest.TestMerchantSlug+"/spend-delegations", secondDoc)
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)
	payer := identity.CustomerID(dbtest.TestMerchantID.UUID())
	stored, err := svc.InvokerSpendLimits(dbtest.WithTestMerchant(context.Background()), payer)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, "role", stored[0].Scope)
	require.Equal(t, roleKey, stored[0].ScopeKey)
	require.Len(t, stored[0].Windows, 1)
	require.EqualValues(t, 500, stored[0].Windows[0].Limit)

	resp = requestOrgTreasuryJSON(t, srv, http.MethodPut, "/v1/orgs/"+dbtest.TestMerchantSlug+"/spend-delegations",
		map[string]any{"customer_id": uuid.NewString(), "delegations": []map[string]any{}})
	require.Equal(t, http.StatusBadRequest, resp.status, resp.body)
}

type orgTreasuryResponse struct {
	status int
	body   string
}

func requestOrgTreasuryJSON(t *testing.T, srv *httptest.Server, method, path string, body any) orgTreasuryResponse {
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
	return orgTreasuryResponse{status: resp.StatusCode, body: string(data)}
}

func decodeDelegations(t *testing.T, body string) []any {
	t.Helper()
	var doc struct {
		Delegations []any `json:"delegations"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &doc), body)
	return doc.Delegations
}
