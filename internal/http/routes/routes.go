package routes

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/controlplane"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/request/ginreq"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// ServiceRoutePrefix is the path under the tenant-scoped public API where
// OAT-authenticated server-to-server billing operations live (issue #222). It
// REPLACES the retired private/mTLS service listener: machine callers present an
// OpenRails-issued tenant OAT as a Bearer token against this public surface. The
// acting tenant is bound by the OAT's owning org, not the URL.
const ServiceRoutePrefix = "/service"

// SelfRoutePrefix is the path under the tenant-scoped public API where
// browser-direct SELF-SERVICE billing operations live (issue #222 browser
// tier). A tenant's host frontend mints a short-lived DELEGATED ACCESS TOKEN for
// the logged-in end-user and the browser presents it as a Bearer token directly
// against this surface. The acting user is the token's `delegated_sub` and the
// acting tenant is bound by the token's `tenant` claim — never the URL.
const SelfRoutePrefix = "/self"

// TenantAdminRoutePrefix is the path under the tenant-scoped public API where
// browser-direct TENANT-ADMIN billing operations live (issue #259). A tenant's
// host frontend mints a FEDERATED, TENANT-SIGNED delegated access token carrying
// `openrails:tenant:*` permissions for one of its ADMIN users; the browser
// presents it as a Bearer token directly against this surface to act on ANY user
// WITHIN the token's tenant via the `:user_id` path param. The acting admin is
// the token's `delegated_sub` (recorded for audit) and the acting tenant is
// pinned from the token's validated issuer — never the URL.
const TenantAdminRoutePrefix = "/tenant-admin"

type Options struct {
	// Authenticator is the framework-neutral auth boundary used to build the
	// neutral Required/Optional middleware for these routes (issue #282/#285).
	Authenticator billingauth.Authenticator

	// OperatorPermissionChecker, when set, makes the operator org the LIVE
	// authority for admin routes (#224): after the operator-org gate, admin
	// routes additionally require the openrails:admin permission evaluated live
	// against the operator org. nil in verifier-only mode (legacy gate only).
	OperatorPermissionChecker authpolicy.OperatorPermissionChecker
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
				r.AbortJSON(http.StatusUnauthorized, unauthenticatedMessage(err))
				return
			}
			r.Request = r.Request.WithContext(billingauth.SetUserContext(r.Request.Context(), uc))
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
					r.Request = r.Request.WithContext(billingauth.SetUserContext(r.Request.Context(), uc))
				}
			}
			next(r)
		}
	}
}

func unauthenticatedMessage(err error) string {
	if err == nil || errors.Is(err, billingauth.ErrUnauthenticated) {
		return "authentication required"
	}
	return err.Error()
}

func wrapHandler(rt *app.Runtime, fn func(r *httprequest.Request)) gin.HandlerFunc {
	return func(c *gin.Context) {
		fn(ginreq.New(c, rt))
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
	mw := []router.Middleware{
		opts.requiredMW(),
		authpolicy.OperatorAdminRequiredMW(rt.Config, rt.DB.GetDB()),
	}
	// #224: when the OpenRails-owned AuthKit control plane is present, the
	// operator org is the LIVE authority — require the openrails:admin permission
	// evaluated against the operator org at request time. This is the forward
	// path away from the global-admin fallback.
	if opts.OperatorPermissionChecker != nil {
		mw = append(mw, authpolicy.OperatorPermissionRequiredMW(opts.OperatorPermissionChecker, controlplane.PermAdmin))
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

// RegisterServiceRoutes mounts the server-to-server billing surface on a
// tenant-scoped PUBLIC route group authenticated by OpenRails-issued tenant OATs
// (issue #222). This replaces the retired private/mTLS listener and its
// certificate-scope model: every operation is gated by an OAT permission from the
// canonical colon-form OpenRails permission catalog, and the acting tenant is the
// OAT's owning tenant (pinned by OATRequired before any tenant-owned DB access).
//
// oatMW must authenticate the OAT (typically ginmw.OATRequired); it pins the
// resolved tenant + permissions onto the context that the per-route
// RequireOATPermission gates and the handlers read.
func RegisterServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, oatMW gin.HandlerFunc, minter DelegatedMinter, issuerAdmin DelegatedIssuerAdmin) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(oatMW)
	// Pin a tenant-scoped DB connection AFTER the OAT resolves the tenant, so RLS
	// constrains every tenant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.TenantDBConn(rt.DB))
	}

	// Browser-tier delegated-token MINT (issue #222). A host backend authenticated
	// by an OAT holding PermSelfMint asks OpenRails to mint a short-lived,
	// user-scoped delegated access token (for its OWN tenant) to hand to a browser
	// for the /v1/self/* surface. Mounted only when a minter is wired (control
	// plane present); the OAT gate + per-route permission keep it server-to-server.
	//
	// DEPRECATED (issue #259): the federated/tenant-signed tier lets hosts mint
	// their OWN aud=openrails tokens (signed with their key, verified via JWKS),
	// removing this round-trip. Kept during the dual-trust migration window; it is
	// retired once all tenants self-sign (see the issuer-management routes below).
	if minter != nil {
		group.POST("/delegated-tokens",
			ginmw.RequireOATPermission(controlplane.PermSelfMint),
			mintDelegatedTokenHandler(minter),
		)
	}

	// FEDERATED issuer management (issue #259). The OAT remains the ROOT
	// tenant-management credential: a host backend authenticated by an OAT holding
	// PermAdmin for its tenant registers/rotates/disables the issuer + JWKS URL it
	// self-signs aud=openrails browser tokens with. The tenant is bound from the
	// OAT (never the body), so a caller can only manage its OWN tenant's issuers.
	if issuerAdmin != nil {
		issuers := group.Group("/tenant/issuers")
		issuers.POST("", ginmw.RequireOATPermission(controlplane.PermAdmin), registerDelegatedIssuerHandler(issuerAdmin))
		issuers.GET("", ginmw.RequireOATPermission(controlplane.PermAdmin), listDelegatedIssuersHandler(issuerAdmin))
		issuers.POST("/disable", ginmw.RequireOATPermission(controlplane.PermAdmin), disableDelegatedIssuerHandler(issuerAdmin))
	}

	users := group.Group("/users/:user_id")
	users.GET("/entitlements", ginmw.RequireOATPermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetUserEntitlements))
	users.GET("/credits", ginmw.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetUserCredits))
	users.GET("/product-access", ginmw.RequireOATPermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetUserProductAccess))

	credits := group.Group("/credits")
	// SPEND (hot-path billing) operations — authorize/hold/capture draw down a
	// payer's balance, so they require BOTH the coarse credits:write operator
	// capability AND the explicit billing:spend (credits:spend) "may you bill this
	// payer" gate (issue #246). The two are checked in sequence; PermAdmin
	// satisfies either. Release returns a remainder (un-bills), so it needs write
	// but not spend.
	creditsSpend := ginmw.RequireOATPermission(controlplane.PermCreditsSpend)
	creditsWrite := ginmw.RequireOATPermission(controlplane.PermCreditsWrite)

	// Unified authorize: policy decision + ATOMIC hold placement (issue #235/#247).
	credits.POST("/authorize", creditsWrite, creditsSpend, wrap(httphandlers.ServiceAuthorizeCredits))
	// Payer balance snapshot (issue #235/#247): available = balance - held.
	credits.GET("/balance", ginmw.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetCreditsBalance))

	credits.POST("/deposit", creditsWrite, wrap(httphandlers.ServiceDepositCredits))
	credits.POST("/withdraw", creditsWrite, wrap(httphandlers.ServiceWithdrawCredits))
	credits.POST("/hold", creditsWrite, creditsSpend, wrap(httphandlers.ServiceHoldCredits))
	credits.POST("/holds/:id/capture", creditsWrite, creditsSpend, wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/holds/:id/release", creditsWrite, wrap(httphandlers.ServiceReleaseHold))
	credits.POST("/hold/:id/capture", creditsWrite, creditsSpend, wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/hold/:id/release", creditsWrite, wrap(httphandlers.ServiceReleaseHold))
	credits.GET("/transactions/lookup", ginmw.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceLookupCreditTransaction))
	credits.GET("/users/:user_id", ginmw.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetUserCredits))

	// Org billing-account admin surface (issue #242): configure prepaid|arrears
	// mode + spend caps + auto-top-up, read settings, and list usage. Tensorhub's
	// billing-admin proxies to these with its service OAT; OpenRails owns the model.
	creditsRead := ginmw.RequireOATPermission(controlplane.PermCreditsRead)
	credits.PUT("/account-settings", creditsWrite, wrap(httphandlers.ServiceSetCreditAccountSettings))
	credits.GET("/account-settings", creditsRead, wrap(httphandlers.ServiceGetCreditAccountSettings))
	credits.GET("/transactions", creditsRead, wrap(httphandlers.ServiceListOwnerCreditTransactions))

	// Credit-type definition writes are catalog-definition operations: gate them
	// behind the explicit catalog-write permission (issue #222 — catalog/definition
	// writes available to OATs only under an explicit permission). Reads use the
	// coarse credits:read capability.
	creditTypes := group.Group("/credit-types")
	creditTypes.POST("", ginmw.RequireOATPermission(controlplane.PermCatalogWrite), wrap(httphandlers.ServiceCreateCreditType))
	creditTypes.GET("", ginmw.RequireOATPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceListCreditTypes))
	creditTypes.PATCH("/:name", ginmw.RequireOATPermission(controlplane.PermCatalogWrite), wrap(httphandlers.ServiceUpdateCreditType))
	creditTypes.POST("/:name/deactivate", ginmw.RequireOATPermission(controlplane.PermCatalogWrite), wrap(httphandlers.ServiceDeactivateCreditType))
	creditTypes.POST("/:name/activate", ginmw.RequireOATPermission(controlplane.PermCatalogWrite), wrap(httphandlers.ServiceActivateCreditType))
}

// RegisterSelfServiceRoutes mounts the browser-direct SELF-SERVICE billing
// surface on a tenant-scoped PUBLIC route group authenticated by a browser's
// DELEGATED ACCESS TOKEN (issue #222 browser-tier foundation). It reuses the
// existing user-facing handlers — they all read the acting user via
// r.GetUser(), which delegatedMW binds to the token's `delegated_sub` — so every
// operation is automatically scoped to the authenticated end-user and their
// tenant. There is no `:user_id` in any path: a browser token can only act on
// its own subject.
//
// delegatedMW must authenticate the delegated token (typically
// ginmw.DelegatedSelfRequired); it pins the resolved tenant + acting user +
// self-permissions onto the context that the per-route RequireDelegatedPermission
// gates and the handlers read.
func RegisterSelfServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(delegatedMW)
	// Pin a tenant-scoped DB connection AFTER the delegated token resolves the
	// tenant, so RLS constrains every tenant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.TenantDBConn(rt.DB))
	}

	read := ginmw.RequireDelegatedPermission(controlplane.PermSelfBillingRead)

	// Account + balance/credits read.
	group.GET("/status", read, wrap(httphandlers.GetMyBillingStatus))
	group.GET("/credits", read, wrap(httphandlers.GetMyCredits))
	group.GET("/credits/:type", read, wrap(httphandlers.GetMyCreditsType))
	group.GET("/credits/:type/transactions", read, wrap(httphandlers.GetMyCreditTransactions))

	// Payment / transaction history.
	group.GET("/payments", read, wrap(httphandlers.GetUserPayments))
	group.GET("/entitlements/active", read, wrap(httphandlers.SelfGetActiveEntitlements))

	// Subscriptions: read for the whole group; mutations gated by scope.
	//
	// cancel and resume are two halves of the same reversible-cancellation
	// lifecycle (#215/#216), so resume is gated by the SAME cancel scope — a
	// browser that may cancel its own subscription may also undo that cancel.
	// change-tier is a subscription mutation; it also rides the cancel scope as
	// the subscription-management capability for self tokens. The acting user is
	// the token's delegated_sub (bound by DelegatedSelfRequired), so every op is
	// scoped to that user's own subscription. payment-method update is a
	// payment-method mutation -> the payment-methods:manage scope.
	subscriptionManage := ginmw.RequireDelegatedPermission(controlplane.PermSelfSubscriptionCancel)
	manage := ginmw.RequireDelegatedPermission(controlplane.PermSelfPaymentMethods)
	subs := group.Group("/subscriptions")
	subs.GET("", read, wrap(httphandlers.GetMySubscriptions))
	subs.GET("/:id", read, wrap(httphandlers.GetSubscription))
	subs.POST("/:id/cancel", subscriptionManage, wrap(httphandlers.CancelSubscription))
	subs.POST("/:id/resume", subscriptionManage, wrap(httphandlers.ResumeSubscription))
	subs.POST("/:id/change-tier", subscriptionManage, wrap(httphandlers.ChangeTier))
	subs.PUT("/:id/payment-method", manage, wrap(httphandlers.UpdateSubscriptionPaymentMethod))
	// App-driven on-chain cancel/revoke (#266/#271): the full prepare -> sign ->
	// confirm -> mirror loop. solana-cancel-tx builds the unsigned cancel tx the
	// wallet signs+sends; solana-cancel confirms the signature landed on-chain and
	// then mirrors the cancel into the DB (stops the cranker). Solana is the source
	// of truth — there is no DB-only "soft cancel". Both gated by the cancel scope.
	subs.POST("/:id/solana-cancel-tx", subscriptionManage, wrap(httphandlers.PrepareSolanaCancelTx))
	subs.POST("/:id/solana-cancel", subscriptionManage, wrap(httphandlers.ConfirmSolanaCancel))
	// App-driven on-chain tier change (#272): the prepare -> sign -> confirm ->
	// mirror loop for changing tier on an existing Solana subscription. prepare
	// returns the SINGLE ATOMIC cancel-old+subscribe-new tx (co-signed for an
	// upgrade's prorated transfer); confirm verifies it landed on-chain and mirrors
	// the switch into the DB (old cancelled, new active, next_pull_at per kind).
	// Both ride the subscription-management (cancel) scope.
	subs.POST("/:id/solana-tier-change", subscriptionManage, wrap(httphandlers.PrepareSolanaTierChange))
	subs.POST("/:id/solana-tier-change/confirm", subscriptionManage, wrap(httphandlers.ConfirmSolanaTierChange))

	// Payment methods: list is a read; mutations require the manage scope.
	pm := group.Group("/payment-methods")
	pm.GET("", read, wrap(httphandlers.ListPaymentMethods))
	pm.POST("", manage, wrap(httphandlers.CreatePaymentMethod))
	pm.PUT("/:id", manage, wrap(httphandlers.UpdatePaymentMethod))
	pm.DELETE("/:id", manage, wrap(httphandlers.DeletePaymentMethod))

	// Checkout: create a session and read/confirm it (browser self-checkout).
	checkoutCreate := ginmw.RequireDelegatedPermission(controlplane.PermSelfCheckoutCreate)
	checkout := group.Group("/checkout")
	checkout.POST("", checkoutCreate, wrap(httphandlers.CreateCheckoutSession))
	checkout.GET("/:id", read, wrap(httphandlers.GetCheckoutSession))
	checkout.POST("/:id/confirm", checkoutCreate, wrap(httphandlers.ConfirmCheckoutSession))
}

// RegisterTenantAdminRoutes mounts the browser-direct TENANT-ADMIN billing
// surface on a tenant-scoped PUBLIC route group authenticated by a FEDERATED,
// TENANT-SIGNED delegated access token carrying `openrails:tenant:*` permissions
// (issue #259). Unlike the self-service surface (which has no `:user_id` and acts
// only on the token's own subject), these routes act on ANY user WITHIN the
// token's tenant via the `:user_id` path param.
//
// It REUSES the existing operator admin handlers unchanged. They are safe here
// because:
//   - the target user is the `:user_id` path param,
//   - the acting admin is r.GetUser() == the token's `delegated_sub` (so audit
//     fields like AdminGrant.GrantedBy record the acting admin),
//   - the tenant is pinned onto the request context by delegatedMW from the
//     token's validated issuer, so every tenant-owned query is scoped to that
//     tenant (RLS-enforced, #227). A `:user_id` belonging to another tenant is
//     therefore unreachable — fail closed, no cross-tenant access.
//
// delegatedMW must authenticate the delegated token (ginmw.DelegatedSelfRequired);
// it pins the resolved tenant + acting admin + tenant-permissions onto the
// context that the per-route RequireDelegatedPermission gates and handlers read.
func RegisterTenantAdminRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(delegatedMW)
	// Pin a tenant-scoped DB connection AFTER the delegated token resolves the
	// tenant, so RLS constrains every tenant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.TenantDBConn(rt.DB))
	}

	read := ginmw.RequireDelegatedPermission(controlplane.PermTenantBillingRead)
	subRead := ginmw.RequireDelegatedPermission(controlplane.PermTenantBillingRead)
	entWrite := ginmw.RequireDelegatedPermission(controlplane.PermTenantEntitlementsWrite)
	payWrite := ginmw.RequireDelegatedPermission(controlplane.PermTenantPaymentsWrite)
	subWrite := ginmw.RequireDelegatedPermission(controlplane.PermTenantSubscriptionsWrite)
	metricsRead := ginmw.RequireDelegatedPermission(controlplane.PermTenantMetricsRead)

	// Tenant metrics (issue #259 + #232): a tenant admin reads THEIR OWN tenant's
	// analytics. The metrics queries are tenant-scoped to the request's tenant
	// (resolved from the delegated token's issuer + pinned above), so these never
	// expose another tenant's numbers — cross-tenant aggregation is the separate
	// platform-superadmin path, not reachable here.
	metrics := group.Group("/metrics")
	metrics.GET("/summary", metricsRead, wrap(httphandlers.GetAdminMetricsSummary))
	metrics.GET("/revenue", metricsRead, wrap(httphandlers.GetAdminMetricsRevenue))
	metrics.GET("/subscriptions", metricsRead, wrap(httphandlers.GetAdminMetricsSubscriptions))
	metrics.GET("/processors", metricsRead, wrap(httphandlers.GetAdminMetricsProcessors))
	metrics.GET("/churn", metricsRead, wrap(httphandlers.GetAdminMetricsChurn))

	// Tenant-wide operational lists. They reuse admin handlers, but the delegated
	// middleware pins the tenant before RLS-aware queries run.
	group.GET("/repair-alerts", read, wrap(httphandlers.GetAdminRepairAlerts))
	group.GET("/manual-rebill-attempts", read, wrap(httphandlers.GetAdminManualRebillAttempts))

	users := group.Group("/users/:user_id")

	// Reads — the tenant-admin billing:read capability.
	users.GET("", read, wrap(httphandlers.GetAdminUserBillingProfile))
	users.GET("/payments", read, wrap(httphandlers.GetAdminUserPayments))
	users.GET("/entitlements", read, wrap(httphandlers.GetAdminUserEntitlements))
	users.GET("/nmi", read, wrap(httphandlers.GetAdminUserNMI))
	users.GET("/nmi/metrics", read, wrap(httphandlers.GetAdminUserNMIMetrics))
	users.GET("/ccbill", read, wrap(httphandlers.GetAdminUserCCBill))
	users.GET("/ccbill/metrics", read, wrap(httphandlers.GetAdminUserCCBillMetrics))

	// Entitlement grants/revokes — entitlements:write.
	users.POST("/entitlements", entWrite, wrap(httphandlers.GrantAdminEntitlement))
	users.DELETE("/entitlements/:id", entWrite, wrap(httphandlers.RevokeAdminEntitlement))

	// Off-channel payment recording — payments:write.
	users.POST("/payments/off-channel", payWrite, wrap(httphandlers.AdminCreateOffChannelPayment))

	// Subscriptions: a tenant admin may cancel a subscription owned by a user in
	// its tenant. Subscriptions are addressed by id; the handler operates within
	// the pinned tenant, so a sub outside the tenant is unreachable (fail closed).
	subs := group.Group("/subscriptions")
	subs.GET("", subRead, wrap(httphandlers.GetAdminSubscriptions))
	subs.GET("/:id", subRead, wrap(httphandlers.GetAdminSubscription))
	subs.POST("/:id/cancel", subWrite, wrap(httphandlers.AdminCancelSubscription))
}
