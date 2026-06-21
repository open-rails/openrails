package ginroutes

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/request/ginreq"
)

// ServiceRoutePrefix is the path under the merchant-scoped public API where
// API-key-authenticated server-to-server billing operations live (issue #222).
// It REPLACES the retired private/mTLS service listener: machine callers
// present an OpenRails-issued merchant API key as a Bearer token against this
// public surface. The acting merchant is bound by the API key's owner, not the
// URL.
const ServiceRoutePrefix = "/service"

// SelfRoutePrefix is the canonical browser self-service billing surface. The
// credential profile may be a delegated JWT in standalone mode or a host/user
// bearer in embedded mode; the URL is intentionally one stable `/me` surface
// and the credential profile lives on the resolved Principal.
const SelfRoutePrefix = "/me"

// AdminRoutePrefix is the path under the merchant-scoped public API where the
// (delegated) ADMIN billing surface lives (#259, #528). A merchant's host
// frontend mints a FEDERATED, MERCHANT-SIGNED delegated access token carrying
// browser-safe `org:` permissions for one of its ADMIN users; the browser
// presents it as a Bearer token directly against this surface to act on ANY user
// WITHIN the token's merchant via the `:user_id` path param. The acting admin is
// the token's `delegated_sub` (recorded for audit) and the acting merchant is
// pinned from the token's validated issuer — never the URL. (#528 retired the
// per-user admin surface; this delegated surface IS the admin API.)
const AdminRoutePrefix = "/admin"

func wrapHandler(rt *app.Runtime, fn func(r *httprequest.Request)) gin.HandlerFunc {
	return func(c *gin.Context) {
		fn(ginreq.New(c, rt))
	}
}

// RegisterServiceRoutes mounts the server-to-server billing surface on a
// merchant-scoped PUBLIC route group authenticated by OpenRails-issued merchant API keys
// (issue #222). This replaces the retired private/mTLS listener and its
// certificate-scope model: every operation is gated by an API-key permission from the
// canonical colon-form OpenRails permission catalog, and the acting merchant is the
// API key's owner (pinned by ServiceCredentialRequired before any merchant-owned DB access).
//
// oatMW must authenticate the service credential (typically ginmw.ServiceCredentialRequired);
// it pins the resolved merchant + Principal onto the context that the per-route
// RequirePermission gates and the handlers read.
func RegisterServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, oatMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(oatMW)
	// Pin a merchant-scoped DB connection AFTER the API key resolves the merchant, so RLS
	// constrains every merchant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.MerchantDBConn(rt.DB))
	}

	// #480/#481: federated delegated-token issuers are no longer an OpenRails-owned
	// registry (the old delegated-issuer table was dropped). Standalone JWKS/
	// issuer trust is AuthKit's remote_application registry (#74); register issuers
	// via AuthKit, not an OpenRails route.

	// Always batch (#354): one issuer, many subjects, one query; a single
	// lookup is an array of one.
	group.POST("/customers/by-external-subject/entitlements",
		ginmw.RequirePermission(controlplane.PermEntitlementsRead),
		wrap(httphandlers.ServiceGetExternalSubjectEntitlements),
	)

	customers := group.Group("/customers/:customer_id")
	customers.GET("/entitlements", ginmw.RequirePermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetCustomerEntitlements))

	// Reverse lookup (#535): customers holding an active window of an entitlement,
	// keyset-paginated. Backs a host directory's filter-by-entitlement (AuthKit #91).
	entGroup := group.Group("/entitlements")
	entGroup.GET("/:entitlement/customers", ginmw.RequirePermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetCustomersWithEntitlement))

	users := group.Group("/users/:user_id")
	users.GET("/product-access", ginmw.RequirePermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetUserProductAccess))

	// Canonical invoker credits path. The legacy alias `/credits/invokers/:invoker`
	// was removed (finishes the #534 service-route dedup): both hit the same
	// handler, and a duplicate surface only has to be kept permission-symmetric.
	invokers := group.Group("/invokers/:invoker")
	invokers.GET("/credits", ginmw.RequirePermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetInvokerCredits))

	credits := group.Group("/credits")
	// SPEND (hot-path billing) operations — authorize/hold/capture draw down a
	// payer's balance, so they require BOTH the coarse credits:write operator
	// capability AND the explicit billing:spend (credits:spend) "may you bill this
	// payer" gate (issue #246). The two are checked in sequence. Release returns
	// a remainder (un-bills), so it needs write but not spend.
	creditsSpend := ginmw.RequirePermission(controlplane.PermCreditsSpend)
	creditsWrite := ginmw.RequirePermission(controlplane.PermCreditsWrite)

	// Unified ADMISSION (issue #298): throughput (rate-limit) + money (hold) +
	// suspension + blocklist + endpoint gating in one call; emits x-ratelimit-*
	// + 429/Retry-After. Hot-path gate hosts call before doing work.
	group.POST("/admit", creditsWrite, creditsSpend, wrap(httphandlers.ServiceAdmit))
	// Cross-payer BATCH admission (#335): N admit items (mixed payers) in one
	// hop, per-item verdicts with single-admit semantics. Same spend gates.
	group.POST("/admit/batch", creditsWrite, creditsSpend, wrap(httphandlers.ServiceAdmitBatch))
	// Budget introspection (#304): spend-cap windows for a host /status dashboard.
	group.GET("/budget", ginmw.RequirePermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetBudget))
	// Trust-tier policy admin (#298): configure spend/trust-tier money budgets.
	group.PUT("/payer-spend-limits", creditsWrite, wrap(httphandlers.ServiceSetPayerSpendLimits))
	// Trust-tier SCHEDULE admin (#476): declare the cumulative-spend ladder ONCE;
	// OpenRails then auto-maintains each payer's trust tier.
	group.PUT("/tier-schedules", creditsWrite, wrap(httphandlers.ServiceSetTierSchedule))
	// Merchant-scoped configuration. Service API keys and delegated merchant
	// admins use the same merchant-configuration permission names; only the
	// credential profile differs.
	group.GET("/merchant-configuration", ginmw.RequirePermission(controlplane.PermMerchantConfigurationRead), wrap(httphandlers.ServiceGetMerchantConfiguration))
	group.PUT("/merchant-configuration", ginmw.RequirePermission(controlplane.PermMerchantConfigurationWrite), wrap(httphandlers.ServiceSetMerchantConfiguration))
	// Trust-tier READ (#477): the payer's current auto-maintained trust tier, for a
	// host that drives its OWN per-tier capacity (e.g. tensorhub's scheduler cap).
	group.GET("/trust-tier", ginmw.RequirePermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetTier))
	// Deprecated alias kept while current clients migrate.
	group.GET("/tier", ginmw.RequirePermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetTier))
	// Wasted-spend report (#488): the host reports a FAILED attempt that cost it $
	// (a refunded hold, a content-filter reject). Accrues into the payer's
	// trust-tier bad_spend windows + the invoker's flat windows; admit denies when
	// over.
	group.POST("/wasted-spend", creditsWrite, wrap(httphandlers.ServiceReportWastedSpend))
	// Wasted-spend usage READ (#488): the payer's + invoker's running wasted-$ totals.
	group.GET("/abuse-usage", ginmw.RequirePermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceAbuseUsage))
	// Arrears credit-line admin (#489): operator sets the per-payer
	// negative-balance ceiling. Read is credits:read.
	group.PUT("/credit-limit", ginmw.RequirePermission(controlplane.PermCreditsWrite), wrap(httphandlers.ServiceSetCreditLimit))
	group.GET("/credit-limit", ginmw.RequirePermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetCreditLimit))
	// Per-invoker spend limits (#473/#517): the payer caps how much its delegated
	// invokers/roles may spend. Written/read with the operator credits gates.
	group.PUT("/invoker-spend-limits", creditsWrite, wrap(httphandlers.ServiceSetInvokerSpendLimits))
	group.GET("/invoker-spend-limits", ginmw.RequirePermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetInvokerSpendLimits))

	// Payer balance snapshot (issue #235/#247): available = balance - held.
	credits.GET("/balance", ginmw.RequirePermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetCreditsBalance))

	credits.POST("/deposit", creditsWrite, wrap(httphandlers.ServiceDepositCredits))
	credits.POST("/withdraw", creditsWrite, wrap(httphandlers.ServiceWithdrawCredits))
	// Canonical hold capture/release (plural). The legacy singular `hold/:id`
	// aliases were removed (#534): the SDK (remote.go) and all consumers use the
	// plural form, and a duplicate surface only has to be kept permission-symmetric.
	credits.POST("/holds/:id/capture", creditsWrite, creditsSpend, wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/holds/:id/release", creditsWrite, wrap(httphandlers.ServiceReleaseHold))
	// #311: per-dimension spend rollup for the platform usage/revenue surfaces.
	credits.POST("/usage/rollup", ginmw.RequirePermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceUsageRollup))
	// #410: per-resource daily revenue (by the usage_event resource column, cross-payer).
	credits.POST("/usage/resource-revenue", ginmw.RequirePermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceResourceRevenue))
	credits.GET("/transactions/lookup", ginmw.RequirePermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceLookupCreditTransaction))

	// Merchant billing-account admin surface (issue #242): configure prepaid|arrears
	// mode + spend caps + auto-top-up, read settings, and list usage. Tensorhub's
	// billing-admin proxies to these with its service API key; OpenRails owns the model.
	creditsRead := ginmw.RequirePermission(controlplane.PermCreditsRead)
	credits.PUT("/account-settings", creditsWrite, wrap(httphandlers.ServiceSetCreditAccountSettings))
	credits.GET("/account-settings", creditsRead, wrap(httphandlers.ServiceGetCreditAccountSettings))
	credits.GET("/transactions", creditsRead, wrap(httphandlers.ServiceListCustomerCreditTransactions))
	// #472: credit-type definition CRUD removed — money has no credit_type dimension.
}

// RegisterSelfServiceRoutes mounts the browser self-service billing surface on
// a merchant-scoped PUBLIC route group authenticated by a bearer principal. It
// reuses the existing user-facing handlers — they all read the acting user via
// r.GetUser(), which delegatedMW binds from the resolved Principal — so every
// operation is automatically scoped to the authenticated end-user and their
// merchant. There is no `:user_id` in any path: a browser token can only act on
// its own subject.
//
// delegatedMW must authenticate the delegated token (typically
// ginmw.DelegatedSelfRequired); it pins the resolved merchant + acting user +
// self-permissions onto the context that the per-route RequirePermission gates
// and the handlers read.
func RegisterSelfServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(delegatedMW)
	// Pin a merchant-scoped DB connection AFTER the delegated token resolves the
	// merchant, so RLS constrains every merchant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.MerchantDBConn(rt.DB))
	}

	// Customer money self-service. Customer routes operate only on the
	// authenticated subject; no self permissions or customer_id path params.
	group.GET("/balance", wrap(httphandlers.GetMyBalance))
	group.GET("/transactions", wrap(httphandlers.GetMyAccountTransactions))
	group.PUT("/settings", wrap(httphandlers.SetMyCreditAccountSettings))
	group.GET("/status", wrap(httphandlers.GetMyBillingStatus))

	// Usage breakdown (issue #289): the acting user's metered usage rolled up by
	// event_type (endpoint/model) over a [from, to) window, with summed dimensions.
	// Scoped to the token's subject + merchant like every other self route.
	group.GET("/usage", wrap(httphandlers.GetMyUsage))

	// Invoices (issue #303): the acting user's finalized monthly itemized
	// statements (newest first, paginated) and a single invoice with its line
	// items. Scoped to the token's subject + merchant like every other self route.
	group.GET("/invoices", wrap(httphandlers.GetMyInvoices))
	group.GET("/invoices/:id", wrap(httphandlers.GetMyInvoice))

	// Payment / transaction history.
	group.GET("/payments", wrap(httphandlers.GetUserPayments))
	group.GET("/entitlements/active", wrap(httphandlers.SelfGetActiveEntitlements))

	group.GET("/notifications", wrap(httphandlers.GetNotifications))
	group.GET("/notifications/unread-count", wrap(httphandlers.GetUnreadNotificationCount))
	group.POST("/notifications/:id/read", wrap(httphandlers.MarkNotificationRead))
	group.GET("/products", wrap(httphandlers.GetMyProducts))
	group.GET("/products/:product_id/access", wrap(httphandlers.GetMyProductAccess))

	// Subscriptions: every operation is scoped to the authenticated subject.
	subs := group.Group("/subscriptions")
	subs.GET("", wrap(httphandlers.GetMySubscriptions))
	subs.GET("/:id", wrap(httphandlers.GetSubscription))
	subs.POST("/:id/cancel", wrap(httphandlers.CancelSubscription))
	subs.POST("/:id/resume", wrap(httphandlers.ResumeSubscription))
	subs.POST("/:id/change-tier", wrap(httphandlers.ChangeTier))
	subs.POST("/:id/change-tier/preview", wrap(httphandlers.ChangeTierPreview))
	subs.PUT("/:id/payment-method", wrap(httphandlers.UpdateSubscriptionPaymentMethod))
	// App-driven on-chain cancel/revoke (#266/#271): the full prepare -> sign ->
	// confirm -> mirror loop. solana-cancel-tx builds the unsigned cancel tx the
	// wallet signs+sends; solana-cancel confirms the signature landed on-chain and
	// then mirrors the cancel into the DB (stops the cranker). Solana is the source
	// of truth — there is no DB-only "soft cancel".
	subs.POST("/:id/solana-cancel-tx", wrap(httphandlers.PrepareSolanaCancelTx))
	subs.POST("/:id/solana-cancel", wrap(httphandlers.ConfirmSolanaCancel))
	// App-driven on-chain tier change (#272): the prepare -> sign -> confirm ->
	// mirror loop for changing tier on an existing Solana subscription. prepare
	// returns the SINGLE ATOMIC cancel-old+subscribe-new tx (co-signed for an
	// upgrade's prorated transfer); confirm verifies it landed on-chain and mirrors
	// the switch into the DB (old cancelled, new active, next_pull_at per kind).
	subs.POST("/:id/solana-tier-change", wrap(httphandlers.PrepareSolanaTierChange))
	subs.POST("/:id/solana-tier-change/confirm", wrap(httphandlers.ConfirmSolanaTierChange))

	// Payment methods.
	pm := group.Group("/payment-methods")
	pm.GET("", wrap(httphandlers.ListPaymentMethods))
	pm.POST("", wrap(httphandlers.CreatePaymentMethod))
	pm.PUT("/:id", wrap(httphandlers.UpdatePaymentMethod))
	pm.DELETE("/:id", wrap(httphandlers.DeletePaymentMethod))

	// Checkout: create a session and read/confirm it (browser self-checkout).
	checkout := group.Group("/checkout")
	checkout.POST("", wrap(httphandlers.CreateCheckoutSession))
	checkout.GET("/:id", wrap(httphandlers.GetCheckoutSession))
	checkout.POST("/:id/confirm", wrap(httphandlers.ConfirmCheckoutSession))

	// Stripe customer portal handoff, moved from the old `/stripe/portal` raw
	// user route to the canonical self surface.
	group.POST("/stripe/portal", wrap(httphandlers.CreatePortalSession))
}

// RegisterAdminRoutes mounts the browser-direct delegated admin billing
// surface on a merchant-scoped PUBLIC route group authenticated by a FEDERATED,
// MERCHANT-SIGNED delegated access token carrying browser-safe `org:` permissions
// (issue #259). Unlike the self-service surface (which has no `:user_id` and acts
// only on the token's own subject), these routes act on ANY user WITHIN the
// token's merchant via the `:user_id` path param.
//
// It REUSES the existing operator admin handlers unchanged. They are safe here
// because:
//   - the target user is the `:user_id` path param,
//   - the acting admin is r.GetUser() == the token's `delegated_sub` (so audit
//     fields like EntitlementGrant.GrantedBy record the acting admin),
//   - the merchant is pinned onto the request context by delegatedMW from the
//     token's validated issuer, so every merchant-owned query is scoped to that
//     merchant (RLS-enforced, #227). A `:user_id` belonging to another merchant is
//     therefore unreachable — fail closed, no cross-merchant access.
//
// delegatedMW must authenticate the delegated token (ginmw.DelegatedSelfRequired);
// it pins the resolved merchant + acting admin + merchant-permissions onto the
// context that the per-route RequirePermission gates and handlers read.
func RegisterAdminRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(delegatedMW)
	// Pin a merchant-scoped DB connection AFTER the delegated token resolves the
	// merchant, so RLS constrains every merchant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.MerchantDBConn(rt.DB))
	}

	read := ginmw.RequirePermission(controlplane.PermMerchantBillingRead)
	subRead := ginmw.RequirePermission(controlplane.PermMerchantBillingRead)
	entWrite := ginmw.RequirePermission(controlplane.PermMerchantEntitlementsWrite)
	productAccessWrite := ginmw.RequirePermission(controlplane.PermMerchantProductAccessWrite)
	payWrite := ginmw.RequirePermission(controlplane.PermMerchantPaymentsWrite)
	subWrite := ginmw.RequirePermission(controlplane.PermMerchantSubscriptionsWrite)
	configRead := ginmw.RequirePermission(controlplane.PermMerchantConfigurationRead)
	configWrite := ginmw.RequirePermission(controlplane.PermMerchantConfigurationWrite)
	metricsRead := ginmw.RequirePermission(controlplane.PermMerchantMetricsRead)
	secretsList := ginmw.RequirePermission(controlplane.PermMerchantSecretsList)
	secretsWrite := ginmw.RequirePermission(controlplane.PermMerchantSecretsWrite)
	secretsDelete := ginmw.RequirePermission(controlplane.PermMerchantSecretsDelete)
	secretsTest := ginmw.RequirePermission(controlplane.PermMerchantSecretsTest)

	// Merchant metrics (issue #259 + #232): a merchant admin reads THEIR OWN merchant's
	// analytics. The metrics queries are merchant-scoped to the request's merchant
	// (resolved from the delegated token's issuer + pinned above), so these never
	// expose another merchant's numbers — cross-merchant aggregation is the separate
	// platform-superadmin path, not reachable here.
	// #528: one folded metrics endpoint (was /metrics/{summary,revenue,subscriptions,
	// processors,churn}); ?period/?currency/?granularity control the shape.
	group.GET("/metrics", metricsRead, wrap(httphandlers.GetAdminMetrics))

	group.GET("/merchant-configuration", configRead, wrap(httphandlers.ServiceGetMerchantConfiguration))
	group.PUT("/merchant-configuration", configWrite, wrap(httphandlers.ServiceSetMerchantConfiguration))

	secretGroup := group.Group("/secrets")
	secretGroup.GET("", secretsList, merchantSecretListHandler(rt))
	secretGroup.GET("/registry", secretsList, merchantSecretRegistryHandler())
	secretGroup.PUT("/*name", secretsWrite, merchantSecretPutHandler(rt))
	secretGroup.DELETE("/*name", secretsDelete, merchantSecretDeleteHandler(rt))
	secretGroup.POST("/validate/*name", secretsTest, merchantSecretValidateHandler(rt))

	// Merchant-wide operational lists. They reuse admin handlers, but the delegated
	// middleware pins the merchant before RLS-aware queries run.
	group.GET("/repair-alerts", read, wrap(httphandlers.GetAdminRepairAlerts))
	group.GET("/manual-rebill-attempts", read, wrap(httphandlers.GetAdminManualRebillAttempts))

	users := group.Group("/users/:user_id")

	// Reads — the merchant-admin billing:read capability.
	users.GET("", read, wrap(httphandlers.GetAdminUserBillingProfile))
	users.GET("/payments", read, wrap(httphandlers.GetAdminUserPayments))

	// Entitlement grants/revokes — entitlements:write.
	users.POST("/entitlements", entWrite, wrap(httphandlers.GrantAdminEntitlement))
	users.DELETE("/entitlements/:id", entWrite, wrap(httphandlers.RevokeAdminEntitlement))

	// Product access (ownership) grants/revokes — product-access:write (#250, #528).
	// A separate concept from entitlements: durable per-(user,product) ownership.
	users.POST("/product-access", productAccessWrite, wrap(httphandlers.GrantAdminProductAccess))
	users.DELETE("/product-access/:id", productAccessWrite, wrap(httphandlers.RevokeAdminProductAccess))

	// Off-channel payment recording — payments:write.
	users.POST("/payments/off-channel", payWrite, wrap(httphandlers.AdminCreateOffChannelPayment))
	users.DELETE("/payment-methods/:id", payWrite, wrap(httphandlers.DeleteAdminUserPaymentMethod))

	// Merchant-wide payments (#528: ported from the retired per-user surface).
	payments := group.Group("/payments")
	payments.GET("", read, wrap(httphandlers.GetAdminPayments))
	payments.GET("/:id", read, wrap(httphandlers.GetAdminPayment))
	payments.POST("/:id/refunds", payWrite, wrap(httphandlers.AdminRefundPayment))

	// Subscriptions: a merchant admin may cancel a subscription owned by a user in
	// its merchant. Subscriptions are addressed by id; the handler operates within
	// the pinned merchant, so a sub outside the merchant is unreachable (fail closed).
	subs := group.Group("/subscriptions")
	subs.GET("", subRead, wrap(httphandlers.GetAdminSubscriptions))
	subs.GET("/:id", subRead, wrap(httphandlers.GetAdminSubscription))
	subs.DELETE("/:id", subWrite, wrap(httphandlers.AdminCancelSubscription)) // #528: cancel = DELETE
}
