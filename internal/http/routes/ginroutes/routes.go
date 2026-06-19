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
// service token-authenticated server-to-server billing operations live (issue #222). It
// REPLACES the retired private/mTLS service listener: machine callers present an
// OpenRails-issued tenant service token as a Bearer token against this public surface. The
// acting tenant is bound by the service token's owning tenant, not the URL.
const ServiceRoutePrefix = "/service"

// SelfRoutePrefix is the path under the merchant-scoped public API where
// browser-direct SELF-SERVICE billing operations live (issue #222 browser
// tier). A merchant's host frontend mints a short-lived DELEGATED ACCESS TOKEN for
// the logged-in end-user and the browser presents it as a Bearer token directly
// against this surface. The acting user is the token's `delegated_sub` and the
// acting tenant is bound by the token's `tenant` claim — never the URL.
const SelfRoutePrefix = "/self"

// AdminRoutePrefix is the path under the merchant-scoped public API where the
// (delegated) ADMIN billing surface lives (#259, #528). A merchant's host
// frontend mints a FEDERATED, TENANT-SIGNED delegated access token carrying
// `openrails:merchant:*` permissions for one of its ADMIN users; the browser
// presents it as a Bearer token directly against this surface to act on ANY user
// WITHIN the token's tenant via the `:user_id` path param. The acting admin is
// the token's `delegated_sub` (recorded for audit) and the acting tenant is
// pinned from the token's validated issuer — never the URL. (#528 retired the
// per-user `openrails:admin` surface; this delegated surface IS the admin API.)
const AdminRoutePrefix = "/admin"

func wrapHandler(rt *app.Runtime, fn func(r *httprequest.Request)) gin.HandlerFunc {
	return func(c *gin.Context) {
		fn(ginreq.New(c, rt))
	}
}

// RegisterServiceRoutes mounts the server-to-server billing surface on a
// merchant-scoped PUBLIC route group authenticated by OpenRails-issued tenant service tokens
// (issue #222). This replaces the retired private/mTLS listener and its
// certificate-scope model: every operation is gated by a service token permission from the
// canonical colon-form OpenRails permission catalog, and the acting tenant is the
// service token's owning tenant (pinned by ServiceTokenRequired before any merchant-owned DB access).
//
// oatMW must authenticate the service token (typically ginmw.ServiceTokenRequired); it pins the
// resolved tenant + permissions onto the context that the per-route
// RequireServiceTokenPermission gates and the handlers read.
func RegisterServiceRoutes(group *gin.RouterGroup, rt *app.Runtime, oatMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(oatMW)
	// Pin a merchant-scoped DB connection AFTER the service token resolves the tenant, so RLS
	// constrains every merchant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.MerchantDBConn(rt.DB))
	}

	// #480/#481: federated delegated-token issuers are no longer an OpenRails-owned
	// registry (the tenant_delegated_issuers table was dropped). Standalone JWKS/
	// issuer trust is AuthKit's remote_application registry (#74); register issuers
	// via AuthKit, not an OpenRails route.

	// Always batch (#354): one issuer, many subjects, one query; a single
	// lookup is an array of one.
	group.POST("/customers/by-external-subject/entitlements",
		ginmw.RequireServiceTokenPermission(controlplane.PermEntitlementsRead),
		wrap(httphandlers.ServiceGetExternalSubjectEntitlements),
	)

	customers := group.Group("/customers/:customer_id")
	customers.GET("/entitlements", ginmw.RequireServiceTokenPermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetCustomerEntitlements))

	users := group.Group("/users/:user_id")
	users.GET("/product-access", ginmw.RequireServiceTokenPermission(controlplane.PermEntitlementsRead), wrap(httphandlers.ServiceGetUserProductAccess))

	invokers := group.Group("/invokers/:invoker")
	invokers.GET("/credits", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetInvokerCredits))

	credits := group.Group("/credits")
	// SPEND (hot-path billing) operations — authorize/hold/capture draw down a
	// payer's balance, so they require BOTH the coarse credits:write operator
	// capability AND the explicit billing:spend (credits:spend) "may you bill this
	// payer" gate (issue #246). The two are checked in sequence; PermAdmin
	// satisfies either. Release returns a remainder (un-bills), so it needs write
	// but not spend.
	creditsSpend := ginmw.RequireServiceTokenPermission(controlplane.PermCreditsSpend)
	creditsWrite := ginmw.RequireServiceTokenPermission(controlplane.PermCreditsWrite)

	// Unified ADMISSION (issue #298): throughput (rate-limit) + money (hold) +
	// suspension + blocklist + endpoint gating in one call; emits x-ratelimit-*
	// + 429/Retry-After. Hot-path gate hosts call before doing work.
	group.POST("/admit", creditsWrite, creditsSpend, wrap(httphandlers.ServiceAdmit))
	// Cross-payer BATCH admission (#335): N admit items (mixed payers) in one
	// hop, per-item verdicts with single-admit semantics. Same spend gates.
	group.POST("/admit/batch", creditsWrite, creditsSpend, wrap(httphandlers.ServiceAdmitBatch))
	// Budget introspection (#304): spend-cap windows for a host /status dashboard.
	group.GET("/budget", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetBudget))
	// Tier policy admin (#298): configure a tier's throughput + entitled endpoints + money budgets.
	group.PUT("/payer-spend-limits", creditsWrite, wrap(httphandlers.ServiceSetPayerSpendLimits))
	// Tier SCHEDULE admin (#476): declare the cumulative-spend ladder ONCE; OpenRails
	// then auto-maintains each payer's tier (no host cranking of GraduateTier).
	group.PUT("/tier-schedules", creditsWrite, wrap(httphandlers.ServiceSetTierSchedule))
	// Merchant-scoped configuration. Missing keys use hardcoded service defaults.
	group.GET("/merchant-configuration", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetMerchantConfiguration))
	group.PUT("/merchant-configuration", creditsWrite, wrap(httphandlers.ServiceSetMerchantConfiguration))
	// Graduated-tier READ (#477): the payer's current auto-maintained tier, for a
	// host that drives its OWN per-tier capacity (e.g. tensorhub's scheduler cap).
	group.GET("/tier", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetTier))
	// Wasted-spend report (#488): the host reports a FAILED attempt that cost it $
	// (a refunded hold, a content-filter reject). Accrues into the payer's per-tier
	// bad_spend windows + the invoker's flat windows; admit denies when over.
	group.POST("/wasted-spend", creditsWrite, wrap(httphandlers.ServiceReportWastedSpend))
	// Wasted-spend usage READ (#488): the payer's + invoker's running wasted-$ totals.
	group.GET("/abuse-usage", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceAbuseUsage))
	// Arrears credit-line admin (#489): operator sets the per-payer negative-balance
	// ceiling. Admin-gated (openrails:admin) — NOT self-serve. Read is credits:read.
	group.PUT("/credit-limit", ginmw.RequireServiceTokenPermission(controlplane.PermAdmin), wrap(httphandlers.ServiceSetCreditLimit))
	group.GET("/credit-limit", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetCreditLimit))
	// Per-invoker spend limits (#473/#517): the payer caps how much its delegated
	// invokers/roles may spend. Written/read with the operator credits gates.
	group.PUT("/invoker-spend-limits", creditsWrite, wrap(httphandlers.ServiceSetInvokerSpendLimits))
	group.GET("/invoker-spend-limits", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetInvokerSpendLimits))

	// Payer balance snapshot (issue #235/#247): available = balance - held.
	credits.GET("/balance", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetCreditsBalance))

	credits.POST("/deposit", creditsWrite, wrap(httphandlers.ServiceDepositCredits))
	credits.POST("/withdraw", creditsWrite, wrap(httphandlers.ServiceWithdrawCredits))
	credits.POST("/holds/:id/capture", creditsWrite, creditsSpend, wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/holds/:id/release", creditsWrite, wrap(httphandlers.ServiceReleaseHold))
	credits.POST("/hold/:id/capture", creditsWrite, creditsSpend, wrap(httphandlers.ServiceCaptureHold))
	credits.POST("/hold/:id/release", creditsWrite, wrap(httphandlers.ServiceReleaseHold))
	// #311: per-dimension spend rollup for the platform usage/revenue surfaces.
	credits.POST("/usage/rollup", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceUsageRollup))
	// #410: per-resource daily revenue (by the usage_event resource column, cross-payer).
	credits.POST("/usage/resource-revenue", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceResourceRevenue))
	credits.GET("/transactions/lookup", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceLookupCreditTransaction))
	credits.GET("/invokers/:invoker", ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead), wrap(httphandlers.ServiceGetInvokerCredits))

	// Merchant billing-account admin surface (issue #242): configure prepaid|arrears
	// mode + spend caps + auto-top-up, read settings, and list usage. Tensorhub's
	// billing-admin proxies to these with its service service token; OpenRails owns the model.
	creditsRead := ginmw.RequireServiceTokenPermission(controlplane.PermCreditsRead)
	credits.PUT("/account-settings", creditsWrite, wrap(httphandlers.ServiceSetCreditAccountSettings))
	credits.GET("/account-settings", creditsRead, wrap(httphandlers.ServiceGetCreditAccountSettings))
	credits.GET("/transactions", creditsRead, wrap(httphandlers.ServiceListCustomerCreditTransactions))
	// #472: credit-type definition CRUD removed — money has no credit_type dimension.
}

// RegisterSelfServiceRoutes mounts the browser-direct SELF-SERVICE billing
// surface on a merchant-scoped PUBLIC route group authenticated by a browser's
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
	// Pin a merchant-scoped DB connection AFTER the delegated token resolves the
	// tenant, so RLS constrains every merchant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.MerchantDBConn(rt.DB))
	}

	read := ginmw.RequireDelegatedPermission(controlplane.PermSelfBillingRead)

	// Credit ACCOUNT surface (issue #339 gap-fill): the caller's OWN account
	// settings + balance/outstanding, settings writes, and transaction history.
	// The subject is ALWAYS the delegated principal's subject — no
	// customer_id parameter exists on this surface. Settings writes are
	// gated by the dedicated self billing:write permission so a read-only token
	// can inspect but never reconfigure.
	billingWrite := ginmw.RequireDelegatedPermission(controlplane.PermSelfBillingWrite)
	group.GET("/account", read, wrap(httphandlers.GetMyCreditAccount))
	group.PUT("/account/settings", billingWrite, wrap(httphandlers.SetMyCreditAccountSettings))
	group.GET("/account/transactions", read, wrap(httphandlers.GetMyAccountTransactions))

	// Account + balance/credits read.
	group.GET("/status", read, wrap(httphandlers.GetMyBillingStatus))
	group.GET("/credits", read, wrap(httphandlers.GetMyCredits))
	group.GET("/credits/:currency", read, wrap(httphandlers.GetMyCreditsType))
	group.GET("/credits/:currency/transactions", read, wrap(httphandlers.GetMyCreditTransactions))

	// Usage breakdown (issue #289): the acting user's metered usage rolled up by
	// event_type (endpoint/model) over a [from, to) window, with summed dimensions.
	// Scoped to the token's subject + tenant like every other self route.
	group.GET("/usage", read, wrap(httphandlers.GetMyUsage))

	// Invoices (issue #303): the acting user's finalized monthly itemized
	// statements (newest first, paginated) and a single invoice with its line
	// items. Scoped to the token's subject + tenant like every other self route.
	group.GET("/invoices", read, wrap(httphandlers.GetMyInvoices))
	group.GET("/invoices/:id", read, wrap(httphandlers.GetMyInvoice))

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
	subs.POST("/:id/change-tier/preview", subscriptionManage, wrap(httphandlers.ChangeTierPreview))
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

	walletManage := ginmw.RequireDelegatedPermission(controlplane.PermSelfWallets)
	wallets := group.Group("/wallets")
	wallets.GET("/solana", read, wrap(httphandlers.GetMySolanaWallet))
	wallets.PUT("/solana", walletManage, wrap(httphandlers.UpsertMySolanaWallet))
	wallets.DELETE("/solana", walletManage, wrap(httphandlers.DeleteMySolanaWallet))

	// Checkout: create a session and read/confirm it (browser self-checkout).
	checkoutCreate := ginmw.RequireDelegatedPermission(controlplane.PermSelfCheckoutCreate)
	checkout := group.Group("/checkout")
	checkout.POST("", checkoutCreate, wrap(httphandlers.CreateCheckoutSession))
	checkout.GET("/:id", read, wrap(httphandlers.GetCheckoutSession))
	checkout.POST("/:id/confirm", checkoutCreate, wrap(httphandlers.ConfirmCheckoutSession))

	// USDC funding handoff: external providers fund the user's live wallet, then
	// the existing checkout path collects from that wallet after balance checks.
	group.GET("/usdc-funding-options", read, wrap(httphandlers.GetUSDCFundingOptions))
	funding := group.Group("/usdc-funding-sessions")
	funding.POST("", checkoutCreate, wrap(httphandlers.CreateUSDCFundingSession))
	funding.GET("/:id", read, wrap(httphandlers.GetUSDCFundingSession))
}

// RegisterMerchantAdminRoutes mounts the browser-direct MERCHANT-ADMIN billing
// surface on a merchant-scoped PUBLIC route group authenticated by a FEDERATED,
// TENANT-SIGNED delegated access token carrying `openrails:merchant:*` permissions
// (issue #259). Unlike the self-service surface (which has no `:user_id` and acts
// only on the token's own subject), these routes act on ANY user WITHIN the
// token's tenant via the `:user_id` path param.
//
// It REUSES the existing operator admin handlers unchanged. They are safe here
// because:
//   - the target user is the `:user_id` path param,
//   - the acting admin is r.GetUser() == the token's `delegated_sub` (so audit
//     fields like EntitlementGrant.GrantedBy record the acting admin),
//   - the tenant is pinned onto the request context by delegatedMW from the
//     token's validated issuer, so every merchant-owned query is scoped to that
//     tenant (RLS-enforced, #227). A `:user_id` belonging to another tenant is
//     therefore unreachable — fail closed, no cross-tenant access.
//
// delegatedMW must authenticate the delegated token (ginmw.DelegatedSelfRequired);
// it pins the resolved tenant + acting admin + tenant-permissions onto the
// context that the per-route RequireDelegatedPermission gates and handlers read.
func RegisterAdminRoutes(group *gin.RouterGroup, rt *app.Runtime, delegatedMW gin.HandlerFunc) {
	wrap := func(fn func(r *httprequest.Request)) gin.HandlerFunc {
		return wrapHandler(rt, fn)
	}

	group.Use(delegatedMW)
	// Pin a merchant-scoped DB connection AFTER the delegated token resolves the
	// tenant, so RLS constrains every merchant-owned query (issue #227).
	if rt != nil && rt.DB != nil {
		group.Use(ginmw.MerchantDBConn(rt.DB))
	}

	read := ginmw.RequireDelegatedPermission(controlplane.PermMerchantBillingRead)
	subRead := ginmw.RequireDelegatedPermission(controlplane.PermMerchantBillingRead)
	entWrite := ginmw.RequireDelegatedPermission(controlplane.PermMerchantEntitlementsWrite)
	payWrite := ginmw.RequireDelegatedPermission(controlplane.PermMerchantPaymentsWrite)
	subWrite := ginmw.RequireDelegatedPermission(controlplane.PermMerchantSubscriptionsWrite)
	configRead := ginmw.RequireDelegatedPermission(controlplane.PermMerchantConfigurationRead)
	configWrite := ginmw.RequireDelegatedPermission(controlplane.PermMerchantConfigurationWrite)
	metricsRead := ginmw.RequireDelegatedPermission(controlplane.PermMerchantMetricsRead)
	secretsList := ginmw.RequireDelegatedPermission(controlplane.PermMerchantSecretsList)
	secretsWrite := ginmw.RequireDelegatedPermission(controlplane.PermMerchantSecretsWrite)
	secretsDelete := ginmw.RequireDelegatedPermission(controlplane.PermMerchantSecretsDelete)
	secretsTest := ginmw.RequireDelegatedPermission(controlplane.PermMerchantSecretsTest)

	// Merchant metrics (issue #259 + #232): a merchant admin reads THEIR OWN merchant's
	// analytics. The metrics queries are merchant-scoped to the request's tenant
	// (resolved from the delegated token's issuer + pinned above), so these never
	// expose another merchant's numbers — cross-tenant aggregation is the separate
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
	// middleware pins the tenant before RLS-aware queries run.
	group.GET("/repair-alerts", read, wrap(httphandlers.GetAdminRepairAlerts))
	group.GET("/manual-rebill-attempts", read, wrap(httphandlers.GetAdminManualRebillAttempts))

	users := group.Group("/users/:user_id")

	// Reads — the merchant-admin billing:read capability.
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

	// Merchant-wide payments (#528: ported from the retired per-user surface).
	payments := group.Group("/payments")
	payments.GET("", read, wrap(httphandlers.GetAdminPayments))
	payments.GET("/:id", read, wrap(httphandlers.GetAdminPayment))
	payments.POST("/:id/refunds", payWrite, wrap(httphandlers.AdminRefundPayment))

	// Subscriptions: a merchant admin may cancel a subscription owned by a user in
	// its tenant. Subscriptions are addressed by id; the handler operates within
	// the pinned tenant, so a sub outside the tenant is unreachable (fail closed).
	subs := group.Group("/subscriptions")
	subs.GET("", subRead, wrap(httphandlers.GetAdminSubscriptions))
	subs.GET("/:id", subRead, wrap(httphandlers.GetAdminSubscription))
	subs.DELETE("/:id", subWrite, wrap(httphandlers.AdminCancelSubscription)) // #528: cancel = DELETE
}
