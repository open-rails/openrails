package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBodyLimitSkipsWebhookRoutes(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BodyLimit(8))
	r.POST("/v1/webhooks/:provider", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		c.String(http.StatusOK, string(body))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", strings.NewReader("larger-than-eight"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "larger-than-eight", w.Body.String())
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

func TestIsWebhookPathSupportsEmbeddedPrefix(t *testing.T) {
	t.Parallel()

	require.True(t, isWebhookPath("/v1/webhooks/stripe"))
	require.True(t, isWebhookPath("/billing/v1/webhooks/mobius"))
	require.False(t, isWebhookPath("/v1/checkout"))
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
