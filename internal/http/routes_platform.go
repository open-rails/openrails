package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	policyginmw "github.com/open-rails/openrails/internal/auth/policy/ginmw"
)

// PlatformPrefix is the cross-tenant managed-hosting superadmin API (issue #226).
// It is gated by openrails:platform:superadmin in the SEPARATE platform tenant —
// DISTINCT from the per-tenant operator-admin gate on /v1/admin/*. A tenant
// operator admin cannot reach this surface.
const PlatformPrefix = "/platform"

// registerPlatformRoutes mounts the platform-superadmin cross-tenant API. No-op
// when no platform tenant is configured (the superadmin gate could never pass,
// so the surface stays closed).
func (s *Server) registerPlatformRoutes(e *gin.Engine) {
	if s.controlPlane.PlatformTenantSlug() == "" {
		// No platform tenant configured: do not mount a surface nobody can pass.
		log.Info("platform superadmin routes not mounted: no platform tenant configured")
		return
	}

	group := e.Group(StandaloneV1Prefix + PlatformPrefix)
	group.Use(s.authProvider.Required())
	group.Use(policyginmw.PlatformSuperadminRequired(s.controlPlane))

	// Cross-tenant directory + inspect.
	group.GET("/tenants", s.platformListTenantsHandler())
	group.GET("/tenants/:id", s.platformInspectTenantHandler())
	// Cross-tenant search (audited: every search is recorded).
	group.GET("/search", s.platformSearchHandler())
	// Platform-wide metrics aggregate.
	group.GET("/metrics", s.platformMetricsHandler())

	log.WithField("prefix", StandaloneV1Prefix+PlatformPrefix).
		Info("platform superadmin cross-tenant routes registered")
}

func (s *Server) platformListTenantsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		m, err := s.platformMetrics.Compute(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"tenants": m.Tenants, "count": m.TenantCount})
	}
}

func (s *Server) platformInspectTenantHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := tenantIDParam(c)
		if !ok {
			return
		}
		t, err := s.tenancy.Get(c.Request.Context(), id)
		if err != nil {
			s.tenantErr(c, err)
			return
		}
		c.JSON(http.StatusOK, tenantView(t))
	}
}

func (s *Server) platformSearchHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		q := strings.TrimSpace(c.Query("q"))
		if q == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "q is required"})
			return
		}
		results, err := s.tenancy.SearchTenants(c.Request.Context(), q, 50)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		views := make([]gin.H, 0, len(results))
		for i := range results {
			views = append(views, tenantView(&results[i]))
		}
		c.JSON(http.StatusOK, gin.H{"results": views, "count": len(views)})
	}
}

func (s *Server) platformMetricsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		m, err := s.platformMetrics.Compute(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, m)
	}
}
