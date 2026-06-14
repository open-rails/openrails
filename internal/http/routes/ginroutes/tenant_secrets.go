package ginroutes

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/middleware/ginmw"
	"github.com/open-rails/openrails/internal/http/response"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/merchant"
)

type upsertTenantSecretRequest struct {
	Value           string `json:"value"`
	Validate        bool   `json:"validate"`
	ValidateOnly    bool   `json:"validate_only"`
	SaveAndValidate bool   `json:"save_and_validate"`
}

func tenantSecretRegistryHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": tenantWritableSecretRegistry()})
	}
}

func tenantSecretListHandler(rt *app.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		svc, id, ok := tenantSecretContext(c, rt)
		if !ok {
			return
		}
		statuses, err := svc.ListSecretStatuses(c.Request.Context(), id)
		if err != nil {
			tenantSecretError(c, err)
			return
		}
		statuses = tenantWritableSecretStatuses(statuses)
		c.JSON(http.StatusOK, gin.H{"data": statuses})
	}
}

func tenantSecretPutHandler(rt *app.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		svc, id, ok := tenantSecretContext(c, rt)
		if !ok {
			return
		}
		name := cleanRouteSecretName(c.Param("name"))
		if name == "" {
			response.BadRequest(c, "secret_name_required")
			return
		}
		if !tenantSecretWritable(name) {
			response.ForbiddenWithMessage(c, "tenant_secret_not_writable")
			return
		}
		var req upsertTenantSecretRequest
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Value) == "" {
			response.BadRequest(c, "value_required")
			return
		}
		actor := delegatedActor(c)
		if req.ValidateOnly {
			if err := svc.ValidateCredential(c.Request.Context(), id, name, req.Value, actor, nil); err != nil {
				tenantSecretError(c, err)
				return
			}
			c.JSON(http.StatusOK, gin.H{"name": name, "validated": true, "configured": false})
			return
		}
		sec, err := svc.PutCredential(c.Request.Context(), id, name, req.Value, "rotate", actor)
		if err != nil {
			tenantSecretError(c, err)
			return
		}
		validated := false
		if req.Validate || req.SaveAndValidate {
			if err := svc.ValidateCredential(c.Request.Context(), id, name, "", actor, nil); err != nil {
				tenantSecretError(c, err)
				return
			}
			validated = true
		}
		c.JSON(http.StatusOK, gin.H{"name": sec.Name, "version": sec.Version, "configured": true, "validated": validated})
	}
}

func tenantSecretValidateHandler(rt *app.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		svc, id, ok := tenantSecretContext(c, rt)
		if !ok {
			return
		}
		name := cleanRouteSecretName(c.Param("name"))
		if name == "" {
			response.BadRequest(c, "secret_name_required")
			return
		}
		if !tenantSecretWritable(name) {
			response.ForbiddenWithMessage(c, "tenant_secret_not_writable")
			return
		}
		var req upsertTenantSecretRequest
		_ = c.ShouldBindJSON(&req)
		if err := svc.ValidateCredential(c.Request.Context(), id, name, req.Value, delegatedActor(c), nil); err != nil {
			tenantSecretError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"name": name, "validated": true})
	}
}

func tenantSecretDeleteHandler(rt *app.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		svc, id, ok := tenantSecretContext(c, rt)
		if !ok {
			return
		}
		name := cleanRouteSecretName(c.Param("name"))
		if name == "" {
			response.BadRequest(c, "secret_name_required")
			return
		}
		if !tenantSecretWritable(name) {
			response.ForbiddenWithMessage(c, "tenant_secret_not_writable")
			return
		}
		if err := svc.DeleteCredential(c.Request.Context(), id, name, delegatedActor(c)); err != nil {
			tenantSecretError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"name": name, "configured": false})
	}
}

func tenantSecretContext(c *gin.Context, rt *app.Runtime) (*tenancy.Service, merchant.ID, bool) {
	if rt == nil || rt.Tenancy == nil {
		response.ServiceUnavailable(c, "tenant secrets not configured")
		return nil, merchant.ID{}, false
	}
	id, ok := merchant.FromContext(c.Request.Context())
	if !ok {
		response.InternalError(c, "tenant context missing")
		return nil, merchant.ID{}, false
	}
	return rt.Tenancy, id, true
}

func delegatedActor(c *gin.Context) string {
	if resolved, ok := ginmw.DelegatedFromGin(c); ok && resolved != nil {
		return resolved.DelegatedSubject
	}
	return ""
}

func cleanRouteSecretName(name string) string {
	return strings.Trim(strings.TrimSpace(name), "/")
}

func tenantWritableSecretRegistry() []tenancy.SecretDefinition {
	defs := tenancy.TenantSecretRegistry()
	out := make([]tenancy.SecretDefinition, 0, len(defs))
	for _, def := range defs {
		if def.TenantWritable {
			out = append(out, def)
		}
	}
	return out
}

func tenantWritableSecretStatuses(statuses []tenancy.TenantSecretStatus) []tenancy.TenantSecretStatus {
	out := make([]tenancy.TenantSecretStatus, 0, len(statuses))
	for _, st := range statuses {
		if st.TenantWritable {
			out = append(out, st)
		}
	}
	return out
}

func tenantSecretWritable(name string) bool {
	def, ok := tenancy.SecretDefinitionFor(name)
	return ok && def.TenantWritable
}

func tenantSecretError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, tenancy.ErrSecretBackendUnavailable):
		response.ServiceUnavailable(c, "secret backend unavailable")
	case errors.Is(err, tenancy.ErrSecretNotFound):
		response.BadRequest(c, "secret_not_configured")
	default:
		response.BadRequest(c, "tenant_secret_error")
	}
}
