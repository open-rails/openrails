package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
)

func TestRegisterDebugRoutes_NMITokenizationPage(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	srv := &Server{
		cfg: &config.Config{
			Env: "dev",
			Processors: map[string]*config.ProcessorConfig{
				"mobius": {
					Type:            config.ProcessorTypeNMI,
					TokenizationKey: "abcdef123456",
				},
			},
		},
	}

	r := gin.New()
	srv.registerDebugRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/debug/nmi/tokenization?provider=mobius&mode=real", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Header().Get("Content-Type"), "text/html")
	require.Contains(t, w.Body.String(), "https://secure.networkmerchants.com/token/Collect.js")
	require.Contains(t, w.Body.String(), "abc...456")
}

func TestRegisterDebugRoutes_NMITokenizationPageUsesMerchantSecrets(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	store := merchants.NewMemorySecretStore()
	merchantSvc, err := merchants.NewSecretManagementService(store)
	require.NoError(t, err)
	_, err = merchantSvc.PutCredential(ctx, dbtest.TestMerchantID, merchants.SecretNMIMobiusTokenizationKey, "secret123456")
	require.NoError(t, err)
	_, err = merchantSvc.PutCredential(ctx, dbtest.TestMerchantID, merchants.SecretNMIMobiusTokenizationURL, "https://example.test/Collect.js")
	require.NoError(t, err)

	srv := &Server{
		cfg: &config.Config{
			Env: "dev",
			Processors: map[string]*config.ProcessorConfig{
				"mobius": {
					Type:            config.ProcessorTypeNMI,
					TokenizationKey: "legacy123456",
				},
			},
		},
		merchants:          merchantSvc,
		configuredMerchant: dbtest.TestMerchantID,
	}

	r := gin.New()
	srv.registerDebugRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/debug/nmi/tokenization?provider=mobius&mode=real", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "https://example.test/Collect.js")
	require.Contains(t, w.Body.String(), "sec...456")
	require.NotContains(t, w.Body.String(), "legacy")
}

func TestRegisterDebugRoutes_NMICollectStubJS(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	srv := &Server{cfg: &config.Config{Env: "dev"}}
	r := gin.New()
	srv.registerDebugRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/debug/nmi/collect-stub.js", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Header().Get("Content-Type"), "application/javascript")
	require.Contains(t, w.Body.String(), "window.CollectJS")
	require.Contains(t, w.Body.String(), "tok_stub_")
}
