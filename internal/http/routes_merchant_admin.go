package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/controlplane"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// MerchantAdminPrefix is the platform-gated merchant provisioning/lifecycle API
// (issue #225). It is a cross-merchant directory surface, so it requires
// AuthKit `platform:` authority rather than per-merchant `org:` authority.
const MerchantAdminPrefix = "/admin/merchants"

// registerMerchantAdminRoutes mounts the platform-gated merchant provisioning API.
func (s *Server) registerMerchantAdminRoutes(e *gin.Engine) {
	group := e.Group(StandaloneV1Prefix + MerchantAdminPrefix)
	group.Use(s.authProvider.Required())
	group.Use(ginmw.UserSessionPlatformPrincipalRequired(s.controlPlane))
	group.Use(ginmw.RequirePermission(controlplane.PermPlatformSuperadmin))

	group.POST("", s.merchantProvisionHandler())
	group.GET("/:id", s.merchantGetHandler())
	group.POST("/:id/export", s.merchantExportHandler())
	group.POST("/:id/delete", s.merchantDeleteHandler())

	// Per-merchant processor credential management (issue #225): rotate + test.
	group.GET("/:id/credentials", s.merchantListCredentialsHandler())
	group.PUT("/:id/credentials/*name", s.merchantPutCredentialHandler())
	group.DELETE("/:id/credentials/*name", s.merchantDeleteCredentialHandler())
	group.POST("/:id/credentials/validate/*name", s.merchantValidateCredentialHandler())
	group.POST("/:id/credentials/test-stripe", s.merchantTestStripeHandler())

	log.WithField("prefix", StandaloneV1Prefix+MerchantAdminPrefix).
		Info("platform-gated merchant provisioning routes registered")
}

func merchantIDParam(c *gin.Context) (merchant.ID, bool) {
	id, err := merchant.ParseID(c.Param("id"))
	if err != nil {
		jsonError(c, http.StatusBadRequest, "invalid merchant id")
		return merchant.ID{}, false
	}
	return id, true
}

func (s *Server) merchantProvisionHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req merchants.ProvisionRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			jsonError(c, http.StatusBadRequest, "invalid request body")
			return
		}
		t, err := s.merchants.Provision(c.Request.Context(), req)
		if err != nil {
			log.WithError(err).Warn("merchant provision failed")
			jsonError(c, http.StatusBadRequest, err.Error())
			return
		}
		c.JSON(http.StatusOK, merchantView(t))
	}
}

func (s *Server) merchantGetHandler() gin.HandlerFunc {
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

func (s *Server) merchantExportHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := merchantIDParam(c)
		if !ok {
			return
		}
		exportID, counts, err := s.merchants.Export(c.Request.Context(), id)
		if err != nil {
			s.merchantErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"export_id": exportID, "row_counts": counts})
	}
}

func (s *Server) merchantDeleteHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := merchantIDParam(c)
		if !ok {
			return
		}
		var body struct {
			Confirm bool   `json:"confirm"`
			Reason  string `json:"reason"`
		}
		_ = c.ShouldBindJSON(&body)
		err := s.merchants.Delete(c.Request.Context(), id, merchants.DeleteOptions{Confirm: body.Confirm})
		if err != nil {
			if errors.Is(err, merchants.ErrExportRequired) {
				jsonError(c, http.StatusConflict, "export required before delete")
				return
			}
			s.merchantErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "deleted"})
	}
}

func (s *Server) merchantPutCredentialHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := merchantIDParam(c)
		if !ok {
			return
		}
		var body struct {
			Value string `json:"value"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.Value == "" {
			jsonError(c, http.StatusBadRequest, "value is required")
			return
		}
		name := strings.TrimPrefix(c.Param("name"), "/")
		if strings.TrimSpace(name) == "" {
			jsonError(c, http.StatusBadRequest, "credential name is required")
			return
		}
		sec, err := s.merchants.RotateCredential(c.Request.Context(), id, name, body.Value)
		if err != nil {
			s.merchantErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"name": sec.Name, "version": sec.Version})
	}
}

func (s *Server) merchantListCredentialsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := merchantIDParam(c)
		if !ok {
			return
		}
		statuses, err := s.merchants.ListSecretStatuses(c.Request.Context(), id)
		if err != nil {
			s.merchantSecretErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": statuses})
	}
}

func (s *Server) merchantDeleteCredentialHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := merchantIDParam(c)
		if !ok {
			return
		}
		name := strings.TrimPrefix(c.Param("name"), "/")
		if strings.TrimSpace(name) == "" {
			jsonError(c, http.StatusBadRequest, "credential name is required")
			return
		}
		if err := s.merchants.DeleteCredential(c.Request.Context(), id, name); err != nil {
			s.merchantSecretErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"name": name, "configured": false})
	}
}

func (s *Server) merchantValidateCredentialHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := merchantIDParam(c)
		if !ok {
			return
		}
		name := strings.TrimPrefix(c.Param("name"), "/")
		if strings.TrimSpace(name) == "" {
			jsonError(c, http.StatusBadRequest, "credential name is required")
			return
		}
		var body struct {
			Value string `json:"value"`
		}
		_ = c.ShouldBindJSON(&body)
		if err := s.merchants.ValidateCredential(c.Request.Context(), id, name, body.Value, nil); err != nil {
			s.merchantSecretErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"name": name, "validated": true})
	}
}

func (s *Server) merchantTestStripeHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := merchantIDParam(c)
		if !ok {
			return
		}
		err := s.merchants.TestStripeCredential(c.Request.Context(), id, nil)
		if err != nil {
			if errors.Is(err, merchants.ErrSecretNotFound) {
				jsonError(c, http.StatusBadRequest, "no stripe secret key configured")
				return
			}
			jsonError(c, http.StatusBadGateway, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func (s *Server) merchantSecretErr(c *gin.Context, err error) {
	if errors.Is(err, merchants.ErrSecretBackendUnavailable) {
		jsonError(c, http.StatusServiceUnavailable, "secret backend unavailable")
		return
	}
	if errors.Is(err, merchants.ErrSecretNotFound) {
		jsonError(c, http.StatusBadRequest, "secret not configured")
		return
	}
	s.merchantErr(c, err)
}

func (s *Server) merchantErr(c *gin.Context, err error) {
	if errors.Is(err, merchants.ErrMerchantNotFound) {
		jsonError(c, http.StatusNotFound, "merchant not found")
		return
	}
	log.WithError(err).Warn("merchant admin operation failed")
	jsonError(c, http.StatusInternalServerError, err.Error())
}

func merchantView(t *merchants.Merchant) gin.H {
	return gin.H{
		"id":           t.ID.String(),
		"slug":         t.Slug,
		"status":       string(t.Status),
		"owner_org_id": t.OwnerOrgID,
	}
}
