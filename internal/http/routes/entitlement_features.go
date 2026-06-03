package routes

import (
	"net/http"

	"github.com/open-rails/openrails/internal/app"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/router"
)

// RegisterEntitlementFeatureRoutes mounts the Stripe-shaped entitlement-feature
// surface (issue #245) onto an ALREADY-AUTH-GATED route group. It deliberately
// does NOT apply auth/tenant middleware itself: the caller mounts it under a group
// that has already pinned auth + a tenant-scoped DB connection (so RLS constrains
// every tenant-owned query, #227), exactly like RegisterAdminRoutes /
// RegisterServiceRoutes do for their handlers.
//
// Intended mount: the operator-admin group built by RegisterAdminRoutes (which
// applies AuthProvider.Required + OperatorAdminRequired + TenantDBConn). Under it
// this exposes:
//
//	POST   /entitlements/features                          create a feature
//	GET    /entitlements/features                          list features
//	GET    /products/:id/features                          list a product's features
//	POST   /products/:id/features                          attach a feature to a product
//	DELETE /products/:id/features/:product_feature_id      detach a product feature
//	GET    /entitlements/active_entitlements?user_id=...    Stripe-shaped active read
//
// The active_entitlements read accepts an explicit user_id (admin/service callers
// resolve any user). For the browser self-service surface, mount
// httphandlers.SelfGetActiveEntitlements separately (it derives the user from the
// delegated token instead of a query param).
func RegisterEntitlementFeatureRoutes(group router.Router, rt *app.Runtime) {
	ent := group.Group("/entitlements")
	ent.Handle(http.MethodPost, "/features", h(httphandlers.CreateEntitlementFeature))
	ent.Handle(http.MethodGet, "/features", h(httphandlers.ListEntitlementFeatures))
	ent.Handle(http.MethodGet, "/active_entitlements", h(httphandlers.ServiceGetActiveEntitlements))

	products := group.Group("/products/:id/features")
	products.Handle(http.MethodGet, "", h(httphandlers.ListProductFeatures))
	products.Handle(http.MethodPost, "", h(httphandlers.AttachProductFeature))
	products.Handle(http.MethodDelete, "/:product_feature_id", h(httphandlers.DetachProductFeature))
}
