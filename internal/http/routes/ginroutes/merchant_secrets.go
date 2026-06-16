package ginroutes

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/response"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

type upsertMerchantSecretRequest struct {
	Value           string `json:"value"`
	Validate        bool   `json:"validate"`
	ValidateOnly    bool   `json:"validate_only"`
	SaveAndValidate bool   `json:"save_and_validate"`
}

func merchantSecretRegistryHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": merchantWritableSecretRegistry()})
	}
}

func merchantSecretListHandler(rt *app.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		svc, id, ok := merchantSecretContext(c, rt)
		if !ok {
			return
		}
		statuses, err := svc.ListSecretStatuses(c.Request.Context(), id)
		if err != nil {
			merchantSecretError(c, err)
			return
		}
		statuses = merchantWritableSecretStatuses(statuses)
		c.JSON(http.StatusOK, gin.H{"data": statuses})
	}
}

func merchantSecretPutHandler(rt *app.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		svc, id, ok := merchantSecretContext(c, rt)
		if !ok {
			return
		}
		name := cleanRouteSecretName(c.Param("name"))
		if name == "" {
			response.BadRequest(c, "secret_name_required")
			return
		}
		if !merchantSecretWritable(name) {
			response.ForbiddenWithMessage(c, "merchant_secret_not_writable")
			return
		}
		var req upsertMerchantSecretRequest
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Value) == "" {
			response.BadRequest(c, "value_required")
			return
		}
		if req.ValidateOnly {
			if err := svc.ValidateCredential(c.Request.Context(), id, name, req.Value, nil); err != nil {
				merchantSecretError(c, err)
				return
			}
			c.JSON(http.StatusOK, gin.H{"name": name, "validated": true, "configured": false})
			return
		}
		sec, err := svc.PutCredential(c.Request.Context(), id, name, req.Value)
		if err != nil {
			merchantSecretError(c, err)
			return
		}
		validated := false
		if req.Validate || req.SaveAndValidate {
			if err := svc.ValidateCredential(c.Request.Context(), id, name, "", nil); err != nil {
				merchantSecretError(c, err)
				return
			}
			validated = true
		}
		c.JSON(http.StatusOK, gin.H{"name": sec.Name, "version": sec.Version, "configured": true, "validated": validated})
	}
}

func merchantSecretValidateHandler(rt *app.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		svc, id, ok := merchantSecretContext(c, rt)
		if !ok {
			return
		}
		name := cleanRouteSecretName(c.Param("name"))
		if name == "" {
			response.BadRequest(c, "secret_name_required")
			return
		}
		if !merchantSecretWritable(name) {
			response.ForbiddenWithMessage(c, "merchant_secret_not_writable")
			return
		}
		var req upsertMerchantSecretRequest
		_ = c.ShouldBindJSON(&req)
		if err := svc.ValidateCredential(c.Request.Context(), id, name, req.Value, nil); err != nil {
			merchantSecretError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"name": name, "validated": true})
	}
}

func merchantSecretDeleteHandler(rt *app.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		svc, id, ok := merchantSecretContext(c, rt)
		if !ok {
			return
		}
		name := cleanRouteSecretName(c.Param("name"))
		if name == "" {
			response.BadRequest(c, "secret_name_required")
			return
		}
		if !merchantSecretWritable(name) {
			response.ForbiddenWithMessage(c, "merchant_secret_not_writable")
			return
		}
		if err := svc.DeleteCredential(c.Request.Context(), id, name); err != nil {
			merchantSecretError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"name": name, "configured": false})
	}
}

func merchantSecretContext(c *gin.Context, rt *app.Runtime) (*merchants.Service, merchant.ID, bool) {
	if rt == nil || rt.Merchants == nil {
		response.ServiceUnavailable(c, "merchant secrets not configured")
		return nil, merchant.ID{}, false
	}
	id, ok := merchant.FromContext(c.Request.Context())
	if !ok {
		response.InternalError(c, "merchant context missing")
		return nil, merchant.ID{}, false
	}
	return rt.Merchants, id, true
}

func cleanRouteSecretName(name string) string {
	return strings.Trim(strings.TrimSpace(name), "/")
}

func merchantWritableSecretRegistry() []merchants.SecretDefinition {
	defs := merchants.MerchantSecretRegistry()
	out := make([]merchants.SecretDefinition, 0, len(defs))
	for _, def := range defs {
		if def.MerchantWritable {
			out = append(out, def)
		}
	}
	return out
}

func merchantWritableSecretStatuses(statuses []merchants.MerchantSecretStatus) []merchants.MerchantSecretStatus {
	out := make([]merchants.MerchantSecretStatus, 0, len(statuses))
	for _, st := range statuses {
		if st.MerchantWritable {
			out = append(out, st)
		}
	}
	return out
}

func merchantSecretWritable(name string) bool {
	def, ok := merchants.SecretDefinitionFor(name)
	return ok && def.MerchantWritable
}

func merchantSecretError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, merchants.ErrSecretBackendUnavailable):
		response.ServiceUnavailable(c, "secret backend unavailable")
	case errors.Is(err, merchants.ErrSecretNotFound):
		response.BadRequest(c, "secret_not_configured")
	default:
		response.BadRequest(c, "merchant_secret_error")
	}
}
