package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
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
					TokenizationURL: "https://example.com/collect.js",
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
	require.Contains(t, w.Body.String(), "https://example.com/collect.js")
	require.Contains(t, w.Body.String(), "abc...456")
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
