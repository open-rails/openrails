package ginmw

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBodyLimitAppliesToWebhookRoutes(t *testing.T) {
	t.Parallel()

	// Webhook routes are no longer exempt from the global body limit; it acts as
	// a backstop in addition to the tighter per-processor caps in the handlers.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BodyLimit(8))
	r.POST("/v1/webhooks/:provider", func(c *gin.Context) {
		_, err := io.ReadAll(c.Request.Body)
		require.Error(t, err)
		c.Status(http.StatusRequestEntityTooLarge)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", strings.NewReader("larger-than-eight"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestBodyLimitAppliesToNonWebhookRoutes(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BodyLimit(8))
	r.POST("/v1/checkout", func(c *gin.Context) {
		_, err := io.ReadAll(c.Request.Body)
		require.Error(t, err)
		c.Status(http.StatusRequestEntityTooLarge)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader("larger-than-eight"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestSecurityHeadersSkipsCSPForNMITokenizationDebugPage(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(SecurityHeaders())
	r.GET("/debug/nmi/tokenization", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/v1/products", func(c *gin.Context) { c.Status(http.StatusOK) })

	debugReq := httptest.NewRequest(http.MethodGet, "/debug/nmi/tokenization", nil)
	debugW := httptest.NewRecorder()
	r.ServeHTTP(debugW, debugReq)
	require.Equal(t, http.StatusOK, debugW.Code)
	require.Empty(t, debugW.Header().Get("Content-Security-Policy"))
	require.Equal(t, "nosniff", debugW.Header().Get("X-Content-Type-Options"))

	apiReq := httptest.NewRequest(http.MethodGet, "/v1/products", nil)
	apiW := httptest.NewRecorder()
	r.ServeHTTP(apiW, apiReq)
	require.Equal(t, http.StatusOK, apiW.Code)
	require.NotEmpty(t, apiW.Header().Get("Content-Security-Policy"))
}

// CORS merchant-origin tests (issue #222 browser tier): a browser on a configured
// (per-merchant) allowed origin can preflight + call OpenRails directly; an
// unlisted origin is denied. The allow-list never weakens to "*" when origins
// are configured.
func TestCORS_AllowsConfiguredTenantOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS([]string{"https://app.example.com", "https://app.example.com"}))
	r.POST("/v1/self/status", func(c *gin.Context) { c.Status(http.StatusOK) })

	// Preflight (OPTIONS) from an allowed merchant origin.
	req := httptest.NewRequest(http.MethodOptions, "/v1/self/status", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, "https://app.example.com", w.Header().Get("Access-Control-Allow-Origin"))
	require.True(t, w.Code == http.StatusNoContent || w.Code == http.StatusOK, "preflight should succeed, got %d", w.Code)
}

func TestCORS_DeniesUnlistedOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS([]string{"https://app.example.com", "https://app.example.com"}))
	r.POST("/v1/self/status", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodOptions, "/v1/self/status", nil)
	req.Header.Set("Origin", "https://evil.example.net")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// A disallowed origin must NOT be echoed back and must not get a wildcard.
	require.NotEqual(t, "https://evil.example.net", w.Header().Get("Access-Control-Allow-Origin"))
	require.NotEqual(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}
