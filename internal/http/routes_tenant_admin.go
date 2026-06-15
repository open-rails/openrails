package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	policyginmw "github.com/open-rails/openrails/internal/auth/policy/ginmw"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TenantAdminPrefix is the admin-gated tenant provisioning/lifecycle API
// (issue #225). It is gated by the SAME admin authority as the rest of /v1/admin
// (#312): only a caller holding the LIVE openrails:admin permission in their own
// tenant may provision, suspend, resume, delete, or change a tenant's tier or
// processor credentials.
const TenantAdminPrefix = "/admin/tenants"

// registerTenantAdminRoutes mounts the admin-gated tenant provisioning API.
func (s *Server) registerTenantAdminRoutes(e *gin.Engine) {
	group := e.Group(StandaloneV1Prefix + TenantAdminPrefix)
	group.Use(s.authProvider.Required())
	// HARDCUT (#312): tenant provisioning is gated by the LIVE openrails:admin
	// permission held in the caller's own tenant — not a claim-based operator
	// tenant. The control plane (always present, #469) is the checker.
	group.Use(policyginmw.AdminPermissionRequired(s.controlPlane, controlplane.PermAdmin))

	group.POST("", s.tenantProvisionHandler())
	group.GET("/:id", s.tenantGetHandler())
	group.POST("/:id/suspend", s.tenantLifecycleHandler("suspend"))
	group.POST("/:id/resume", s.tenantLifecycleHandler("resume"))
	group.POST("/:id/tier", s.tenantTierHandler())
	group.POST("/:id/export", s.tenantExportHandler())
	group.POST("/:id/delete", s.tenantDeleteHandler())

	// Per-tenant processor credential management (issue #225): rotate + test.
	group.GET("/:id/credentials", s.tenantListCredentialsHandler())
	group.PUT("/:id/credentials/*name", s.tenantPutCredentialHandler())
	group.DELETE("/:id/credentials/*name", s.tenantDeleteCredentialHandler())
	group.POST("/:id/credentials/validate/*name", s.tenantValidateCredentialHandler())
	group.POST("/:id/credentials/test-stripe", s.tenantTestStripeHandler())

	log.WithField("prefix", StandaloneV1Prefix+TenantAdminPrefix).
		Info("admin-gated tenant provisioning routes registered")
}

func tenantIDParam(c *gin.Context) (merchant.ID, bool) {
	id, err := merchant.ParseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tenant id"})
		return merchant.ID{}, false
	}
	return id, true
}

func (s *Server) tenantProvisionHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req tenancy.ProvisionRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		t, err := s.tenancy.Provision(c.Request.Context(), req)
		if err != nil {
			log.WithError(err).Warn("tenant provision failed")
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, tenantView(t))
	}
}

func (s *Server) tenantGetHandler() gin.HandlerFunc {
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

func (s *Server) tenantLifecycleHandler(op string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := tenantIDParam(c)
		if !ok {
			return
		}
		var err error
		switch op {
		case "suspend":
			err = s.tenancy.Suspend(c.Request.Context(), id)
		case "resume":
			err = s.tenancy.Resume(c.Request.Context(), id)
		}
		if err != nil {
			s.tenantErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func (s *Server) tenantTierHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := tenantIDParam(c)
		if !ok {
			return
		}
		var body struct {
			Tier string `json:"tier"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		if err := s.tenancy.TierChange(c.Request.Context(), id, body.Tier); err != nil {
			s.tenantErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func (s *Server) tenantExportHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := tenantIDParam(c)
		if !ok {
			return
		}
		exportID, counts, err := s.tenancy.Export(c.Request.Context(), id)
		if err != nil {
			s.tenantErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"export_id": exportID, "row_counts": counts})
	}
}

func (s *Server) tenantDeleteHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := tenantIDParam(c)
		if !ok {
			return
		}
		var body struct {
			Confirm bool   `json:"confirm"`
			Reason  string `json:"reason"`
		}
		_ = c.ShouldBindJSON(&body)
		err := s.tenancy.Delete(c.Request.Context(), id, tenancy.DeleteOptions{Confirm: body.Confirm})
		if err != nil {
			if errors.Is(err, tenancy.ErrExportRequired) {
				c.JSON(http.StatusConflict, gin.H{"error": "export required before delete"})
				return
			}
			s.tenantErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "deleted"})
	}
}

func (s *Server) tenantPutCredentialHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := tenantIDParam(c)
		if !ok {
			return
		}
		var body struct {
			Value string `json:"value"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.Value == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "value is required"})
			return
		}
		name := strings.TrimPrefix(c.Param("name"), "/")
		if strings.TrimSpace(name) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "credential name is required"})
			return
		}
		sec, err := s.tenancy.RotateCredential(c.Request.Context(), id, name, body.Value)
		if err != nil {
			s.tenantErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"name": sec.Name, "version": sec.Version})
	}
}

func (s *Server) tenantListCredentialsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := tenantIDParam(c)
		if !ok {
			return
		}
		statuses, err := s.tenancy.ListSecretStatuses(c.Request.Context(), id)
		if err != nil {
			s.tenantSecretErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": statuses})
	}
}

func (s *Server) tenantDeleteCredentialHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := tenantIDParam(c)
		if !ok {
			return
		}
		name := strings.TrimPrefix(c.Param("name"), "/")
		if strings.TrimSpace(name) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "credential name is required"})
			return
		}
		if err := s.tenancy.DeleteCredential(c.Request.Context(), id, name); err != nil {
			s.tenantSecretErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"name": name, "configured": false})
	}
}

func (s *Server) tenantValidateCredentialHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := tenantIDParam(c)
		if !ok {
			return
		}
		name := strings.TrimPrefix(c.Param("name"), "/")
		if strings.TrimSpace(name) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "credential name is required"})
			return
		}
		var body struct {
			Value string `json:"value"`
		}
		_ = c.ShouldBindJSON(&body)
		if err := s.tenancy.ValidateCredential(c.Request.Context(), id, name, body.Value, nil); err != nil {
			s.tenantSecretErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"name": name, "validated": true})
	}
}

func (s *Server) tenantTestStripeHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := tenantIDParam(c)
		if !ok {
			return
		}
		err := s.tenancy.TestStripeCredential(c.Request.Context(), id, nil)
		if err != nil {
			if errors.Is(err, tenancy.ErrSecretNotFound) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "no stripe secret key configured"})
				return
			}
			c.JSON(http.StatusBadGateway, gin.H{"ok": false, "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func (s *Server) tenantSecretErr(c *gin.Context, err error) {
	if errors.Is(err, tenancy.ErrSecretBackendUnavailable) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "secret backend unavailable"})
		return
	}
	if errors.Is(err, tenancy.ErrSecretNotFound) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "secret not configured"})
		return
	}
	s.tenantErr(c, err)
}

func (s *Server) tenantErr(c *gin.Context, err error) {
	if errors.Is(err, tenancy.ErrTenantNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	log.WithError(err).Warn("tenant admin operation failed")
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

func tenantView(t *tenancy.Tenant) gin.H {
	return gin.H{
		"id":              t.ID.String(),
		"slug":            t.Slug,
		"name":            t.Name,
		"status":          string(t.Status),
		"owner_tenant_id": t.OwnerTenantID,
		"billing_tier":    t.BillingTier,
		"region":          t.Region,
		"webhook_host":    t.WebhookHost,
		"webhook_path":    t.WebhookPath,
	}
}
