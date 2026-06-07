package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	httproutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
)

// Issue #222: the private/mTLS service trust surface is removed. The
// server-to-server surface is now service token-authenticated and lives on the PUBLIC
// engine under /v1/service/*, and is mounted ONLY when the OpenRails-owned
// AuthKit control plane is configured (it resolves service tokens). These tests assert that
// removal + gating without requiring a live control plane.

func TestRegisterServiceRoutes_NotMountedWithoutControlPlane(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := &Server{cfg: &config.Config{}} // controlPlane is nil (verifier-only mode)
	e := gin.New()
	srv.registerServiceRoutes(e)

	// No control plane => no service token issuer => the service surface is not mounted.
	req := httptest.NewRequest(http.MethodGet, "/v1/service/tenant-subjects/00000000-0000-0000-0000-000000000001/entitlements", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

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
	httproutes.RegisterServiceRoutes(group, nil, rejectAll, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/service/credits/hold", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}
