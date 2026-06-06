package ginmw

import (
	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/pkg/tenant"
)

// ResolveTenant resolves the tenant / billing namespace for the request and
// attaches it to the request context BEFORE authorization and before any
// tenant-owned DB access (issue #223).
//
// First increment: this resolves to the single default tenant, which is the
// correct behaviour for self-hosted / single-tenant deployments and keeps
// existing single-tenant routes working through the same code path. Multi-tenant
// resolution from host / path / JWT / service token / admin route is layered in by #222
// (tenant-scoped public routes & service tokens) at the marked extension point below; the
// rest of the stack already reads the tenant from context via
// tenant.FromContextOrDefault, so only this resolver needs to change.
func ResolveTenant() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := resolveTenantID(c)

		// Attach to the request context so repositories/services downstream
		// (which call tenant.FromContextOrDefault) are tenant-scoped.
		ctx := tenant.WithID(c.Request.Context(), id)
		c.Request = c.Request.WithContext(ctx)

		// Also expose on the gin context for handlers/middleware that read it
		// directly (e.g. future authorization that must bind to the tenant).
		c.Set("billing.tenant_id", id)

		c.Next()
	}
}

// resolveTenantID determines the tenant for a request.
//
// Extension point for #222: inspect host (subdomain), path prefix
// (/t/:tenant/...), the service token's owning tenant, or a delegated browser token's
// `tenant` claim, look the slug up in billing.tenants, and return that id.
// Until then every request maps to the single default tenant.
func resolveTenantID(_ *gin.Context) tenant.ID {
	return tenant.DefaultID
}
