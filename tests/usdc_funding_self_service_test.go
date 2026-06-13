//go:build integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	ginroutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
	"github.com/open-rails/openrails/pkg/tenant"
)

const testUSDCFundingWallet = "11111111111111111111111111111111"

type usdcFundingDelegatedResolver struct {
	subject     string
	permissions []string
	tenantID    tenant.ID
}

func (r usdcFundingDelegatedResolver) ResolveDelegated(context.Context, string) (*controlplane.ResolvedDelegated, error) {
	tenantID := r.tenantID
	if tenantID.IsZero() {
		tenantID = dbtest.TestTenantID
	}
	return &controlplane.ResolvedDelegated{
		Tenant:           dbtest.TestTenantSlug,
		TenantID:         tenantID,
		TenantSlug:       dbtest.TestTenantSlug,
		DelegatedSubject: r.subject,
		Permissions:      r.permissions,
	}, nil
}

func newUSDCFundingSelfRouter(t *testing.T, suite *TestContainerSuite, subject string, permissions []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	group := e.Group("/v1/self")
	ginroutes.RegisterSelfServiceRoutes(group, suite.App.Runtime, ginmw.DelegatedSelfRequired(usdcFundingDelegatedResolver{
		subject:     subject,
		permissions: permissions,
		tenantID:    dbtest.TestTenantID,
	}))
	return e
}

func doUSDCFundingSelf(e *gin.Engine, method, path, body, idempotencyKey string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer delegated.jwt.token")
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("X-Idempotency-Key", idempotencyKey)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func decodeUSDCFundingBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&body), w.Body.String())
	return body
}

func configureUSDCFundingProviders(t *testing.T, suite *TestContainerSuite) {
	t.Helper()
	require.NotNil(t, suite.App)
	require.NotNil(t, suite.App.Runtime)
	require.NotNil(t, suite.App.Runtime.Config)
	suite.App.Runtime.Config.USDCFunding = &config.USDCFundingConfig{
		Providers: map[string]*config.USDCFundingProviderConfig{
			"robinhood": {
				Enabled:           true,
				SupportedNetworks: []string{"solana"},
				LaunchURLTemplate: "https://robinhood.example/connect?address={wallet}&network={network}&asset={asset}&amount={amount}&session={session_id}&redirect={return_url}",
			},
			"coinbase": {
				Enabled:           true,
				SupportedNetworks: []string{"solana"},
				LaunchURLTemplate: "https://pay.coinbase.example/buy?address={wallet}&network={network}&asset={asset}&amount={amount}&session={session_id}&redirect={return_url}",
			},
		},
	}
}

func TestUSDCFundingSelfServiceCreateGetIdempotencyAndIsolation(t *testing.T) {
	suite := setupTestSuite(t)
	configureUSDCFundingProviders(t, suite)

	userA := uuid.NewString()
	userB := uuid.NewString()
	perms := []string{controlplane.PermSelfBillingRead, controlplane.PermSelfCheckoutCreate}
	routerA := newUSDCFundingSelfRouter(t, suite, userA, perms)
	routerB := newUSDCFundingSelfRouter(t, suite, userB, []string{controlplane.PermSelfBillingRead})

	body := `{
		"provider": "robinhood",
		"wallet": "` + testUSDCFundingWallet + `",
		"network": "solana",
		"asset": "USDC",
		"amount": "12.50",
		"return_url": "https://doujins.example/checkout/return"
	}`

	first := doUSDCFundingSelf(routerA, http.MethodPost, "/v1/self/usdc-funding-sessions", body, "idem-usdc-funding")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	firstBody := decodeUSDCFundingBody(t, first)
	require.Equal(t, "robinhood", firstBody["provider"])
	require.Equal(t, testUSDCFundingWallet, firstBody["wallet"])
	require.Equal(t, "USDC", firstBody["asset"])
	require.Equal(t, "solana", firstBody["network"])
	require.Equal(t, "12.50", firstBody["amount"])
	require.Equal(t, "created", firstBody["status"])
	id, ok := firstBody["id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, id)
	providerURL, ok := firstBody["provider_url"].(string)
	require.True(t, ok)
	require.Contains(t, providerURL, "https://robinhood.example/connect")
	require.Contains(t, providerURL, "address="+testUSDCFundingWallet)
	require.Contains(t, providerURL, "network=solana")

	replay := doUSDCFundingSelf(routerA, http.MethodPost, "/v1/self/usdc-funding-sessions", body, "idem-usdc-funding")
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	replayBody := decodeUSDCFundingBody(t, replay)
	require.Equal(t, id, replayBody["id"])

	getA := doUSDCFundingSelf(routerA, http.MethodGet, "/v1/self/usdc-funding-sessions/"+id, `{}`, "")
	require.Equal(t, http.StatusOK, getA.Code, getA.Body.String())
	getABody := decodeUSDCFundingBody(t, getA)
	require.Equal(t, id, getABody["id"])

	getB := doUSDCFundingSelf(routerB, http.MethodGet, "/v1/self/usdc-funding-sessions/"+id, `{}`, "")
	require.Equal(t, http.StatusNotFound, getB.Code, getB.Body.String())
}

func TestUSDCFundingSelfServiceRejectsUnsupportedProviderAndNetwork(t *testing.T) {
	suite := setupTestSuite(t)
	configureUSDCFundingProviders(t, suite)

	router := newUSDCFundingSelfRouter(t, suite, uuid.NewString(), []string{
		controlplane.PermSelfBillingRead,
		controlplane.PermSelfCheckoutCreate,
	})

	baseBody := `{
		"provider": "coinbase",
		"wallet": "` + testUSDCFundingWallet + `",
		"network": "base",
		"asset": "USDC",
		"amount": "5.00"
	}`
	baseResp := doUSDCFundingSelf(router, http.MethodPost, "/v1/self/usdc-funding-sessions", baseBody, "")
	require.Equal(t, http.StatusBadRequest, baseResp.Code, baseResp.Body.String())
	require.Contains(t, baseResp.Body.String(), "provider is unavailable")

	moonPayBody := `{
		"provider": "moonpay",
		"wallet": "` + testUSDCFundingWallet + `",
		"network": "solana",
		"asset": "USDC",
		"amount": "5.00"
	}`
	moonPayResp := doUSDCFundingSelf(router, http.MethodPost, "/v1/self/usdc-funding-sessions", moonPayBody, "")
	require.Equal(t, http.StatusBadRequest, moonPayResp.Code, moonPayResp.Body.String())
	require.Contains(t, moonPayResp.Body.String(), "provider required")
}
