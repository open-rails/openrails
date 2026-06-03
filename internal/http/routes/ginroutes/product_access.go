package ginroutes

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// RegisterProductAccessRoutes mounts the durable product-access surfaces (issue
// #250) onto the tenant-scoped /v1 api group. It registers three surfaces, each
// expecting the auth middleware its host group already applies:
//
//   - USER (browser/session): GET /me/products, GET /me/products/:product_id/access.
//     The caller's `group` must already require an authenticated end-user
//     (AuthProvider.Required at the central mount, mirroring the existing /me/*
//     routes). Handlers read r.GetUser().
//   - OAT SERVICE: GET /service/users/:user_id/product-access (optional
//     ?product_id=... narrows to a has-access check). The OAT auth middleware is
//     applied at the central /service group mount (ginmw.OATRequired); this
//     function adds the per-route OAT permission gate.
//   - ADMIN: GET/POST/DELETE under /admin/users/:user_id/product-access. The
//     caller's /admin group must already apply the operator-admin gate (mirroring
//     RegisterAdminRoutes).
//
// It deliberately does NOT apply user/admin top-level auth itself (it has no
// AuthProvider) — that stays with the central mount, exactly like the existing
// Register*Routes functions. See the package doc / mount notes for the exact
// central wiring.
func RegisterProductAccessRoutes(group *gin.RouterGroup, rt *app.Runtime) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	// USER surface (auth applied by the host /v1 group, like other /me/* routes).
	me := group.Group("/me")
	me.GET("/products", wrap(httphandlers.GetMyProducts))
	me.GET("/products/:product_id/access", wrap(httphandlers.GetMyProductAccess))

	// OAT SERVICE surface (OATRequired applied at the central /service mount).
	service := group.Group("/service")
	service.GET("/users/:user_id/product-access",
		ginmw.RequireOATPermission(controlplane.PermEntitlementsRead),
		wrap(httphandlers.ServiceGetUserProductAccess),
	)

	// ADMIN surface (operator-admin gate applied at the central /admin mount).
	admin := group.Group("/admin")
	adminAccess := admin.Group("/users/:user_id/product-access")
	adminAccess.GET("", wrap(httphandlers.GetAdminUserProductAccess))
	adminAccess.POST("", wrap(httphandlers.GrantAdminProductAccess))
	adminAccess.DELETE("/:id", wrap(httphandlers.RevokeAdminProductAccess))
}
