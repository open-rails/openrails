package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/controlplane"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
)

// PlatformPrefix is the cross-merchant managed-hosting superadmin API (issue #226).
// It is gated by openrails:platform:superadmin in the SEPARATE platform org —
// DISTINCT from the per-merchant operator-admin gate on /v1/admin/*. A merchant
// operator admin cannot reach this surface.
const PlatformPrefix = "/platform"

// registerPlatformRoutes mounts the platform-superadmin cross-merchant API. No-op
// when no platform org is configured (the superadmin gate could never pass,
// so the surface stays closed).
func (s *Server) registerPlatformRoutes(e *gin.Engine) {
	if s.controlPlane.PlatformOrgSlug() == "" {
		// No platform org configured: do not mount a surface nobody can pass.
		log.Info("platform superadmin routes not mounted: no platform org configured")
		return
	}

	group := e.Group(StandaloneV1Prefix + PlatformPrefix)
	group.Use(s.authProvider.Required())
	group.Use(ginmw.UserSessionPlatformPrincipalRequired(s.controlPlane))
	group.Use(ginmw.RequirePermission(controlplane.PermPlatformSuperadmin))

	// Cross-merchant directory + inspect.
	group.GET("/merchants", s.platformListMerchantsHandler())
	group.GET("/merchants/:id", s.platformInspectMerchantHandler())
	// Cross-merchant search (audited: every search is recorded).
	group.GET("/search", s.platformSearchHandler())
	// Platform-wide metrics aggregate.
	group.GET("/metrics", s.platformMetricsHandler())

	log.WithField("prefix", StandaloneV1Prefix+PlatformPrefix).
		Info("platform superadmin cross-merchant routes registered")
}

func (s *Server) platformListMerchantsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		m, err := s.platformMetrics.Compute(c.Request.Context())
		if err != nil {
			jsonError(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"merchants": m.Merchants, "count": m.MerchantCount})
	}
}

func (s *Server) platformInspectMerchantHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := merchantIDParam(c)
		if !ok {
			return
		}
		t, err := s.merchants.Get(c.Request.Context(), id)
		if err != nil {
			s.merchantErr(c, err)
			return
		}
		c.JSON(http.StatusOK, merchantView(t))
	}
}

func (s *Server) platformSearchHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		q := strings.TrimSpace(c.Query("q"))
		if q == "" {
			jsonError(c, http.StatusBadRequest, "q is required")
			return
		}
		results, err := s.merchants.SearchMerchants(c.Request.Context(), q, 50)
		if err != nil {
			jsonError(c, http.StatusInternalServerError, err.Error())
			return
		}
		views := make([]gin.H, 0, len(results))
		for i := range results {
			views = append(views, merchantView(&results[i]))
		}
		c.JSON(http.StatusOK, gin.H{"results": views, "count": len(views)})
	}
}

func (s *Server) platformMetricsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		m, err := s.platformMetrics.Compute(c.Request.Context())
		if err != nil {
			jsonError(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.JSON(http.StatusOK, m)
	}
}
