package routes

import (
	"net/http"

	"github.com/open-rails/openrails/internal/app"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

type Options struct {
	// Authenticator is the framework-neutral auth boundary used to build the
	// neutral Required/Optional middleware for these routes (issue #282/#285).
	Authenticator billingauth.Authenticator

	// AdminPermissionChecker is the LIVE authority for admin routes (#312): admin
	// routes require the openrails:admin permission evaluated live against the
	// CALLER'S OWN tenant. nil in verifier-only mode, in which case admin routes
	// fail closed (there is no operator-tenant or role-claim fallback).
	AdminPermissionChecker authpolicy.AdminPermissionChecker

}

// requiredMW builds the neutral "authentication required" middleware for the
// embedded surface. It authenticates via opts.Authenticator, aborts 401 on
// failure, and pins the resulting UserContext on the request context (the single
// contract handlers + the operator gates read via FromContext).
func (opts Options) requiredMW() router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			a := opts.Authenticator
			if a == nil {
				r.AbortJSON(http.StatusInternalServerError, "authentication disabled")
				return
			}
			uc, err := a.Authenticate(r.Request.Context(), r.Request)
			if err != nil {
				r.AbortJSON(http.StatusUnauthorized, billingauth.UnauthenticatedMessage(err))
				return
			}
			r.SetUserContext(uc)
			next(r)
		}
	}
}

// optionalMW builds the neutral best-effort auth middleware: it attempts
// authentication and pins the UserContext when it succeeds, but never aborts.
func (opts Options) optionalMW() router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if a := opts.Authenticator; a != nil {
				if uc, err := a.Authenticate(r.Request.Context(), r.Request); err == nil {
					r.SetUserContext(uc)
				}
			}
			next(r)
		}
	}
}

// h adapts a handler func to the neutral router.Handler type.
func h(fn func(r *httprequest.Request)) router.Handler {
	return router.Handler(fn)
}

func RegisterUserRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	required := opts.requiredMW()
	optional := opts.optionalMW()

	// Pin a tenant-scoped DB connection for the request (tenant resolved by the
	// global ResolveTenant middleware) so RLS constrains tenant-owned queries
	// (issue #227). It applies to every route in this group.
	var group router.Router = rr
	if rt != nil && rt.DB != nil {
		group = rr.Group("", middleware.TenantDBConnMW(rt.DB))
	}

	group.Handle(http.MethodGet, "/products", h(httphandlers.GetProducts), optional)
	group.Handle(http.MethodGet, "/prices", h(httphandlers.GetPrices), optional)
	group.Handle(http.MethodGet, "/solana/config", h(httphandlers.GetSolanaConfig))
	group.Handle(http.MethodGet, "/solana/tokens", h(httphandlers.GetSupportedTokens))

	checkout := group.Group("/checkout", required)
	checkout.Handle(http.MethodPost, "", h(httphandlers.CreateCheckoutSession))
	checkout.Handle(http.MethodGet, "/:id", h(httphandlers.GetCheckoutSession))
	checkout.Handle(http.MethodPost, "/:id/confirm", h(httphandlers.ConfirmCheckoutSession))

	group.Handle(http.MethodGet, "/checkout/:id/solana-pay", h(httphandlers.GetSolanaPay))
	group.Handle(http.MethodPost, "/checkout/:id/solana-pay", h(httphandlers.PostSolanaPay))

	// Solana recurring enrollment (#255): the wallet signs subscribe client-side,
	// then confirms here to charge the first cycle + create the membership.
	group.Handle(http.MethodPost, "/solana/recurring/enroll", h(httphandlers.ConfirmSolanaEnrollment))

	me := group.Group("/me", required)
	me.Handle(http.MethodGet, "/status", h(httphandlers.GetMyBillingStatus))
	me.Handle(http.MethodGet, "/subscriptions", h(httphandlers.GetMySubscriptions))
	me.Handle(http.MethodGet, "/subscriptions/:id", h(httphandlers.GetSubscription))
	me.Handle(http.MethodPut, "/subscriptions/:id/payment-method", h(httphandlers.UpdateSubscriptionPaymentMethod))
	me.Handle(http.MethodPost, "/subscriptions/:id/cancel", h(httphandlers.CancelSubscription))
	me.Handle(http.MethodPost, "/subscriptions/:id/resume", h(httphandlers.ResumeSubscription))
	me.Handle(http.MethodPost, "/subscriptions/:id/change-tier", h(httphandlers.ChangeTier))
	me.Handle(http.MethodPost, "/subscriptions/:id/change-tier/preview", h(httphandlers.ChangeTierPreview))
	me.Handle(http.MethodGet, "/payments", h(httphandlers.GetUserPayments))
	me.Handle(http.MethodGet, "/payment-methods", h(httphandlers.ListPaymentMethods))
	me.Handle(http.MethodPost, "/payment-methods", h(httphandlers.CreatePaymentMethod))
	me.Handle(http.MethodPut, "/payment-methods/:id", h(httphandlers.UpdatePaymentMethod))
	me.Handle(http.MethodDelete, "/payment-methods/:id", h(httphandlers.DeletePaymentMethod))
	me.Handle(http.MethodGet, "/notifications", h(httphandlers.GetNotifications))
	me.Handle(http.MethodGet, "/notifications/unread-count", h(httphandlers.GetUnreadNotificationCount))
	me.Handle(http.MethodPost, "/notifications/:id/read", h(httphandlers.MarkNotificationRead))
	me.Handle(http.MethodGet, "/credits", h(httphandlers.GetMyCredits))
	me.Handle(http.MethodGet, "/credits/:type", h(httphandlers.GetMyCreditsType))
	me.Handle(http.MethodGet, "/credits/:type/transactions", h(httphandlers.GetMyCreditTransactions))
	me.Handle(http.MethodGet, "/products", h(httphandlers.GetMyProducts))
	me.Handle(http.MethodGet, "/products/:product_id/access", h(httphandlers.GetMyProductAccess))

	stripe := group.Group("/stripe", required)
	stripe.Handle(http.MethodPost, "/portal", h(httphandlers.CreatePortalSession))
}

func RegisterAdminRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	// HARDCUT (#312): admin authority is the LIVE openrails:admin permission held
	// in the caller's OWN tenant — evaluated at request time by the control plane,
	// not a claim-based operator-tenant gate. When no control plane is wired
	// (verifier-only mode) the checker is nil and the gate fails closed.
	mw := []router.Middleware{
		opts.requiredMW(),
		authpolicy.AdminPermissionRequiredMW(opts.AdminPermissionChecker, authpolicy.PermAdmin),
	}
	// Pin a tenant-scoped DB connection for the request so RLS constrains
	// tenant-owned admin queries (issue #227).
	if rt != nil && rt.DB != nil {
		mw = append(mw, middleware.TenantDBConnMW(rt.DB))
	}

	group := rr.Group("", mw...)

	group.Handle(http.MethodGet, "/subscriptions", h(httphandlers.GetAdminSubscriptions))
	group.Handle(http.MethodGet, "/subscriptions/:id", h(httphandlers.GetAdminSubscription))
	group.Handle(http.MethodPost, "/subscriptions/:id/cancel", h(httphandlers.AdminCancelSubscription))

	group.Handle(http.MethodGet, "/payments", h(httphandlers.GetAdminPayments))
	group.Handle(http.MethodGet, "/payments/:id", h(httphandlers.GetAdminPayment))
	group.Handle(http.MethodPost, "/payments/:id/refund", h(httphandlers.AdminRefundPayment))
	group.Handle(http.MethodGet, "/users/:user_id/payments", h(httphandlers.GetAdminUserPayments))
	group.Handle(http.MethodPost, "/users/:user_id/payments/off-channel", h(httphandlers.AdminCreateOffChannelPayment))
	group.Handle(http.MethodGet, "/repair-alerts", h(httphandlers.GetAdminRepairAlerts))
	group.Handle(http.MethodGet, "/manual-rebill-attempts", h(httphandlers.GetAdminManualRebillAttempts))

	group.Handle(http.MethodGet, "/users/:user_id", h(httphandlers.GetAdminUserBillingProfile))
	group.Handle(http.MethodGet, "/users/:user_id/entitlements", h(httphandlers.GetAdminUserEntitlements))
	group.Handle(http.MethodGet, "/users/:user_id/nmi", h(httphandlers.GetAdminUserNMI))
	group.Handle(http.MethodGet, "/users/:user_id/nmi/metrics", h(httphandlers.GetAdminUserNMIMetrics))
	group.Handle(http.MethodGet, "/users/:user_id/ccbill", h(httphandlers.GetAdminUserCCBill))
	group.Handle(http.MethodGet, "/users/:user_id/ccbill/metrics", h(httphandlers.GetAdminUserCCBillMetrics))
	group.Handle(http.MethodPost, "/users/:user_id/entitlements", h(httphandlers.GrantAdminEntitlement))
	group.Handle(http.MethodDelete, "/users/:user_id/entitlements/:id", h(httphandlers.RevokeAdminEntitlement))

	// Admin catalog API (issue #205). Mounted alongside subscriptions/payments/users.
	// Symmetric with pkg/service facade; embedded hosts may call the facade directly.
	adminCatalog := group.Group("/catalog")
	adminProducts := adminCatalog.Group("/products")
	adminProducts.Handle(http.MethodPost, "", h(httphandlers.AdminCreateProduct))
	adminProducts.Handle(http.MethodGet, "", h(httphandlers.AdminListProducts))
	adminProducts.Handle(http.MethodGet, "/:id", h(httphandlers.AdminGetProduct))
	adminProducts.Handle(http.MethodGet, "/by-slug/:slug", h(httphandlers.AdminGetProductBySlug))
	adminProducts.Handle(http.MethodPatch, "/:id", h(httphandlers.AdminUpdateProduct))
	adminProducts.Handle(http.MethodPost, "/:id/activate", h(httphandlers.AdminActivateProduct))
	adminProducts.Handle(http.MethodPost, "/:id/deactivate", h(httphandlers.AdminDeactivateProduct))
	adminProducts.Handle(http.MethodPost, "/:id/reconcile", h(httphandlers.AdminReconcileProduct))

	adminPrices := adminCatalog.Group("/prices")
	adminPrices.Handle(http.MethodPost, "", h(httphandlers.AdminCreatePrice))
	adminPrices.Handle(http.MethodGet, "", h(httphandlers.AdminListPrices))
	adminPrices.Handle(http.MethodGet, "/:id", h(httphandlers.AdminGetPrice))
	adminPrices.Handle(http.MethodPatch, "/:id", h(httphandlers.AdminUpdatePrice))
	adminPrices.Handle(http.MethodPost, "/:id/activate", h(httphandlers.AdminActivatePrice))
	adminPrices.Handle(http.MethodPost, "/:id/deactivate", h(httphandlers.AdminDeactivatePrice))
	adminPrices.Handle(http.MethodPost, "/:id/reconcile", h(httphandlers.AdminReconcilePrice))

	// Catalog reconciliation loop drift surface (issue #209). Alert-only:
	// these endpoints never mutate Stripe, NMI, or the catalog rows. Operators
	// resolve drift via the per-price reconcile action above. CCBill is never
	// reconciled (no catalog-list API), so it never appears in these surfaces.
	adminCatalog.Handle(http.MethodGet, "/drift", h(httphandlers.AdminListCatalogDrift))
	adminCatalog.Handle(http.MethodPost, "/drift/refresh", h(httphandlers.AdminRefreshCatalogDrift))
	// reconcile-all is the spec-named alias for an on-demand synchronous pull.
	adminCatalog.Handle(http.MethodPost, "/drift/reconcile-all", h(httphandlers.AdminRefreshCatalogDrift))
	// /orphans is provider-filterable (?provider=stripe|nmi); /stripe/orphans is
	// the operator-friendly convenience alias scoped to Stripe.
	adminCatalog.Handle(http.MethodGet, "/orphans", h(httphandlers.AdminListCatalogOrphans))
	adminCatalog.Handle(http.MethodGet, "/stripe/orphans", h(httphandlers.AdminListStripeOrphans))

	group.Handle(http.MethodGet, "/metrics/summary", h(httphandlers.GetAdminMetricsSummary))
	group.Handle(http.MethodGet, "/metrics/revenue", h(httphandlers.GetAdminMetricsRevenue))
	group.Handle(http.MethodGet, "/metrics/subscriptions", h(httphandlers.GetAdminMetricsSubscriptions))
	group.Handle(http.MethodGet, "/metrics/processors", h(httphandlers.GetAdminMetricsProcessors))
	group.Handle(http.MethodGet, "/metrics/churn", h(httphandlers.GetAdminMetricsChurn))

	// Stripe-shaped entitlement features (issue #245): features CRUD,
	// product-feature attachment, active-entitlements read. Operator-admin gated.
	RegisterEntitlementFeatureRoutes(group, rt)

	// Solana recurring plans — admin surface (#254). Create-only: on-chain plan
	// terms are immutable, so editing core terms = sunset + publish a new plan.
	group.Handle(http.MethodPost, "/solana/recurring/plans", h(httphandlers.AdminPublishSolanaPlan))

	// Product access grants — admin surface (issue #250).
	adminAccess := group.Group("/users/:user_id/product-access")
	adminAccess.Handle(http.MethodGet, "", h(httphandlers.GetAdminUserProductAccess))
	adminAccess.Handle(http.MethodPost, "", h(httphandlers.GrantAdminProductAccess))
	adminAccess.Handle(http.MethodDelete, "/:id", h(httphandlers.RevokeAdminProductAccess))
}

func RegisterWebhookRoutes(rr router.Router, rt *app.Runtime) {
	rr.Handle(http.MethodPost, "/:provider", h(httphandlers.Webhook))
}
