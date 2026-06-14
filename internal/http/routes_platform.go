package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	policyginmw "github.com/open-rails/openrails/internal/auth/policy/ginmw"
	"github.com/open-rails/openrails/internal/platform"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
	"github.com/open-rails/openrails/pkg/merchant"
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
	// Cross-tenant audit log viewer.
	group.GET("/audit", s.platformAuditHandler())
	// Break-glass elevation: grant (justified, time-boxed), list active, revoke.
	group.POST("/break-glass", s.platformBreakGlassGrantHandler())
	group.GET("/break-glass", s.platformBreakGlassListHandler())
	group.POST("/break-glass/:id/revoke", s.platformBreakGlassRevokeHandler())

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
		// list is a low-sensitivity read; record it as a platform-wide action.
		s.auditTenantMutation(c, platform.ActionTenantList, nil, "", nil, nil)
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
		s.auditTenantMutation(c, platform.ActionTenantInspect, &id, "", nil, nil)
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
		// Cross-tenant search is sensitive: ALWAYS audited with the query.
		s.auditTenantMutation(c, platform.ActionTenantSearch, nil, q, nil,
			gin.H{"query": q, "result_count": len(results)})
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
		s.auditTenantMutation(c, platform.ActionMetricsRead, nil, "", nil, nil)
		c.JSON(http.StatusOK, m)
	}
}

func (s *Server) platformAuditHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var target *merchant.ID
		if raw := strings.TrimSpace(c.Query("tenant")); raw != "" {
			id, err := merchant.ParseID(raw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tenant id"})
				return
			}
			target = &id
		}
		limit, _ := strconv.Atoi(c.Query("limit"))
		rows, err := s.platformAudit.List(c.Request.Context(), target, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"entries": rows, "count": len(rows)})
	}
}

func (s *Server) platformBreakGlassGrantHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.platformBreakGlass == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "break-glass unavailable"})
			return
		}
		var body struct {
			TargetTenant  string `json:"target_tenant"`
			Justification string `json:"justification"`
			TTLSeconds    int    `json:"ttl_seconds"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		var target *merchant.ID
		if strings.TrimSpace(body.TargetTenant) != "" {
			id, perr := merchant.ParseID(strings.TrimSpace(body.TargetTenant))
			if perr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid target_tenant"})
				return
			}
			target = &id
		}
		uc, _ := ginauth.UserContextFromGin(c)
		grant, err := s.platformBreakGlass.Grant(c.Request.Context(), platform.GrantRequest{
			ActorUserID:   uc.UserID,
			ActorTenant:   uc.Tenant,
			TargetTenant:  target,
			Justification: body.Justification,
			TTL:           time.Duration(body.TTLSeconds) * time.Second,
		})
		if err != nil {
			if errors.Is(err, platform.ErrBreakGlassJustificationRequired) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "justification is required"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, grant)
	}
}

func (s *Server) platformBreakGlassListHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.platformBreakGlass == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "break-glass unavailable"})
			return
		}
		grants, err := s.platformBreakGlass.ListActive(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"grants": grants, "count": len(grants)})
	}
}

func (s *Server) platformBreakGlassRevokeHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.platformBreakGlass == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "break-glass unavailable"})
			return
		}
		uc, _ := ginauth.UserContextFromGin(c)
		err := s.platformBreakGlass.Revoke(c.Request.Context(), c.Param("id"), uc.UserID, uc.Tenant)
		if err != nil {
			if errors.Is(err, platform.ErrBreakGlassNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "grant not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "revoked"})
	}
}
