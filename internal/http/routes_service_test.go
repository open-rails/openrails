package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	httproutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
)

// Issue #222: the private/mTLS service trust surface is removed. The
// server-to-server surface is service token-authenticated and lives on the
// PUBLIC engine under /v1/service/*; the always-present control plane (#469)
// resolves service tokens. This test asserts the service-token gate without
// requiring a live control plane.

func TestRegisterServiceRoutes_ServiceTokenAuthGatesMountedSurface(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Mount the service surface with a service token middleware that always rejects
	// (modelling "no valid service token presented"). This proves the public service surface
	// is service token-gated rather than an open or certificate-trusted listener. We mount
	// the routes directly because building a Server with a real control plane
	// requires a live AuthKit/DB; the middleware contract is what matters here.
	e := gin.New()
	group := e.Group("/v1/service")
	rejectAll := gin.HandlerFunc(func(c *gin.Context) {
		c.AbortWithStatus(http.StatusUnauthorized)
	})
	// Use the same registration path as the server, with a rejecting service token gate.
	// Runtime is only dereferenced lazily inside handlers, which the rejecting
	// gate prevents from ever running.
	httproutes.RegisterServiceRoutes(group, nil, rejectAll)

	req := httptest.NewRequest(http.MethodPost, "/v1/service/admit", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}
